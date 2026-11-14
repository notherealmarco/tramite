package api_test

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.marcorealacci.me/marcorealacci/tramite/internal/api"
	"git.marcorealacci.me/marcorealacci/tramite/internal/config"
	"git.marcorealacci.me/marcorealacci/tramite/internal/engine"
	"git.marcorealacci.me/marcorealacci/tramite/internal/gtfs"
	g "git.marcorealacci.me/marcorealacci/tramite/internal/gtfstest"
	"git.marcorealacci.me/marcorealacci/tramite/internal/live"
	"git.marcorealacci.me/marcorealacci/tramite/internal/service"
)

func setup(t *testing.T) (*httptest.Server, *engine.Source, *service.Service) {
	t.Helper()
	cfg, err := config.Parse([]byte(`sources: [{id: rome, static: {url: x}}]`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = t.TempDir()
	svc := service.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	src := svc.Source("rome")
	now := g.At(14, 30, 0)
	src.Now = func() time.Time { return now }
	f, err := gtfs.Load(g.Zip(g.Line60), nil, "v1")
	if err != nil {
		t.Fatal(err)
	}
	src.SetStatic(f)
	srv := httptest.NewServer((&api.API{Svc: svc}).Handler())
	t.Cleanup(srv.Close)
	return srv, src, svc
}

func TestDeparturesEndpointAndETag(t *testing.T) {
	srv, _, _ := setup(t)
	resp, err := http.Get(srv.URL + "/v1/sources/rome/stops/A/departures?n=2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b engine.Board
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || len(b.StopTimes) != 2 || b.StopTimes[0].GTFSTripID != "out1" {
		t.Fatalf("status %d board %+v", resp.StatusCode, b)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/v1/sources/rome/stops/A/departures?n=2", nil)
	req.Header.Set("If-None-Match", resp.Header.Get("ETag"))
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusNotModified {
		t.Fatalf("want 304, got %d", r2.StatusCode)
	}
	for path, want := range map[string]int{
		"/v1/sources/nope/stops/A/departures":     404,
		"/v1/sources/rome/stops/ZZ/departures":    404,
		"/v1/sources/rome/stops/A/departures?n=0": 400,
		"/v1/vehicles?bbox=1,2,3":                 400,
		"/readyz":                                 200,
		"/metrics":                                200,
	} {
		r, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != want {
			t.Errorf("%s: got %d want %d", path, r.StatusCode, want)
		}
	}
}

// The stream sends the current state on connect, then again when it changes.
func TestStream(t *testing.T) {
	srv, src, svc := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v1/stream?line=rome:60", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	next := func() string {
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				return d
			}
		}
		t.Fatal("stream ended")
		return ""
	}
	if d := next(); !strings.Contains(d, `"topic":"line=rome:60"`) || !strings.Contains(d, `"data":[]`) {
		t.Fatalf("first event: %s", d)
	}
	// A bus appears on line 60: pushed.
	ts, vs, _ := live.ParseVehicles(g.Vehicles(g.At(14, 30, 0), g.VP{Label: "5039", Trip: "out1", Route: "60", Seq: 1, Lat: 41.9, Lon: 12.5}))
	src.ApplyVehicles(ts, vs)
	svc.Notify("rome")
	if d := next(); !strings.Contains(d, `"id":"5039"`) {
		t.Fatalf("second event: %s", d)
	}
}

func TestStreamRejectsEmpty(t *testing.T) {
	srv, _, _ := setup(t)
	r, err := http.Get(srv.URL + "/v1/stream")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 400 {
		t.Fatalf("got %d", r.StatusCode)
	}
}

func TestSourceVehiclesInferredAndGzip(t *testing.T) {
	srv, src, _ := setup(t)
	src.Cfg.Quirks.Inference.Enabled = true
	ts, vs, _ := live.ParseVehicles(g.Vehicles(g.At(14, 30, 0),
		g.VP{Label: "3429", Trip: "in2", Route: "60", Dir: 1, Seq: 2, Lat: 41.905, Lon: 12.505}))
	src.ApplyVehicles(ts, vs)

	req, _ := http.NewRequest("GET", srv.URL+"/v1/vehicles?bbox=41.8,12.4,42.0,12.6", nil)
	req.Header.Set("Accept-Encoding", "gzip") // set by hand: no transparent decoding
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("want gzip, got %q", resp.Header.Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var vehicles []engine.VehicleInfo
	if err := json.NewDecoder(zr).Decode(&vehicles); err != nil || len(vehicles) != 1 || vehicles[0].StartDate != "20261113" {
		t.Fatalf("vehicles: %+v %v", vehicles, err)
	}

	r2, err := http.Get(srv.URL + "/v1/sources/rome/inferred")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	var inf engine.Inferred
	if err := json.NewDecoder(r2.Body).Decode(&inf); err != nil {
		t.Fatal(err)
	}
	// in2 ends at A 14:40 (timetable), ready 14:43: out1 (14:33) runs 10 min late.
	if inf.MaxWaitSeconds != 1800 || len(inf.Departures) != 1 || inf.Departures[0].GTFSTripID != "out1" ||
		inf.Departures[0].DelaySeconds != 600 || inf.Departures[0].Vehicle != "3429" {
		t.Fatalf("inferred: %+v", inf)
	}
}
