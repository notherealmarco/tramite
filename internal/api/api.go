// Package api serves the HTTP API: JSON endpoints, an SSE stream that pushes
// the same data when it changes, the corrected GTFS-RT feed and metrics.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"git.marcorealacci.me/marcorealacci/tramite/internal/engine"
	"git.marcorealacci.me/marcorealacci/tramite/internal/metrics"
	"git.marcorealacci.me/marcorealacci/tramite/internal/service"
)

type API struct {
	Svc *service.Service
	Log *slog.Logger
	// Heartbeat keeps SSE connections open through proxies.
	Heartbeat time.Duration
}

const (
	defaultN      = 20
	maxN          = 200
	defaultWindow = 2 * time.Hour
	maxWindow     = 24 * time.Hour
)

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", a.ready)
	mux.Handle("GET /metrics", metrics.Handler())
	mux.HandleFunc("GET /v1/sources", a.sources)
	mux.HandleFunc("GET /v1/sources/{source}/stops/{stop}/departures", a.departures)
	mux.HandleFunc("GET /v1/sources/{source}/lines/{line}/vehicles", a.lineVehicles)
	mux.HandleFunc("GET /v1/sources/{source}/inferred", a.inferred)
	mux.HandleFunc("GET /v1/sources/{source}/gtfs-rt/trip-updates.pb", a.tripUpdates)
	mux.HandleFunc("GET /v1/vehicles", a.areaVehicles)
	mux.HandleFunc("GET /v1/stream", a.stream)
	return cors(gzipped(mux))
}

func cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Expose-Headers", "ETag")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "If-None-Match, Last-Event-ID")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (a *API) ready(w http.ResponseWriter, r *http.Request) {
	for _, s := range a.Svc.Sources {
		if !s.Ready() {
			http.Error(w, s.Cfg.ID+": timetable not loaded", http.StatusServiceUnavailable)
			return
		}
	}
	w.Write([]byte("ready\n"))
}

type sourceStatus struct {
	engine.Status
	Feeds map[string]service.FeedStatus `json:"feeds"`
}

func (a *API) sources(w http.ResponseWriter, r *http.Request) {
	out := []sourceStatus{}
	for _, s := range a.Svc.Sources {
		st := sourceStatus{Status: s.Status(), Feeds: map[string]service.FeedStatus{}}
		for _, feed := range []string{"vehicle_positions", "trip_updates", "alerts"} {
			if fs, ok := a.Svc.FeedStatus(s.Cfg.ID, feed); ok {
				st.Feeds[feed] = fs
			}
		}
		out = append(out, st)
	}
	writeJSON(w, r, out)
}

func (a *API) source(w http.ResponseWriter, id string) *engine.Source {
	s := a.Svc.Source(id)
	if s == nil {
		http.Error(w, "unknown source "+id, http.StatusNotFound)
	}
	return s
}

func (a *API) departures(w http.ResponseWriter, r *http.Request) {
	s := a.source(w, r.PathValue("source"))
	if s == nil {
		return
	}
	n, window, err := boardParams(r.URL.Query().Get("n"), r.URL.Query().Get("window"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	b, err := s.Departures(r.PathValue("stop"), n, window)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, r, b)
}

func (a *API) inferred(w http.ResponseWriter, r *http.Request) {
	s := a.source(w, r.PathValue("source"))
	if s == nil {
		return
	}
	writeJSON(w, r, s.InferredDepartures())
}

func (a *API) lineVehicles(w http.ResponseWriter, r *http.Request) {
	s := a.source(w, r.PathValue("source"))
	if s == nil {
		return
	}
	writeJSON(w, r, s.Vehicles(r.PathValue("line"), nil))
}

func (a *API) areaVehicles(w http.ResponseWriter, r *http.Request) {
	box, err := parseBBox(r.URL.Query().Get("bbox"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, r, a.vehiclesIn(box))
}

func (a *API) vehiclesIn(box engine.BBox) []engine.VehicleInfo {
	out := []engine.VehicleInfo{}
	for _, s := range a.Svc.Sources {
		out = append(out, s.Vehicles("", &box)...)
	}
	return out
}

func (a *API) tripUpdates(w http.ResponseWriter, r *http.Request) {
	s := a.source(w, r.PathValue("source"))
	if s == nil {
		return
	}
	b, err := s.CorrectedTripUpdates()
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	writeETagged(w, r, b)
}

func boardParams(nStr, windowStr string) (int, time.Duration, error) {
	n, window := defaultN, defaultWindow
	if nStr != "" {
		v, err := strconv.Atoi(nStr)
		if err != nil || v < 1 || v > maxN {
			return 0, 0, fmt.Errorf("n must be 1..%d", maxN)
		}
		n = v
	}
	if windowStr != "" {
		v, err := time.ParseDuration(windowStr)
		if err != nil || v <= 0 || v > maxWindow {
			return 0, 0, fmt.Errorf("window must be a duration up to %s, e.g. 90m", maxWindow)
		}
		window = v
	}
	return n, window, nil
}

// parseBBox reads "minLat,minLon,maxLat,maxLon".
func parseBBox(s string) (engine.BBox, error) {
	p := strings.Split(s, ",")
	if len(p) != 4 {
		return engine.BBox{}, errors.New("bbox must be minLat,minLon,maxLat,maxLon")
	}
	var v [4]float64
	for i := range p {
		x, err := strconv.ParseFloat(strings.TrimSpace(p[i]), 64)
		if err != nil {
			return engine.BBox{}, errors.New("bbox must be minLat,minLon,maxLat,maxLon")
		}
		v[i] = x
	}
	if v[0] > v[2] || v[1] > v[3] {
		return engine.BBox{}, errors.New("bbox: min must not exceed max")
	}
	if (v[2]-v[0])*(v[3]-v[1]) > 4 {
		return engine.BBox{}, errors.New("bbox too large (max ~2x2 degrees)")
	}
	return engine.BBox{MinLat: v[0], MinLon: v[1], MaxLat: v[2], MaxLon: v[3]}, nil
}

func writeErr(w http.ResponseWriter, err error) {
	if errors.Is(err, engine.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	http.Error(w, err.Error(), http.StatusServiceUnavailable)
}

func writeJSON(w http.ResponseWriter, r *http.Request, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	writeETagged(w, r, b)
}

// writeETagged answers 304 when the client already has this content.
func writeETagged(w http.ResponseWriter, r *http.Request, b []byte) {
	sum := sha256.Sum256(b)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Write(b)
}
