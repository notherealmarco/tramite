package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"git.marcorealacci.me/marcorealacci/tramite/internal/metrics"
)

// A topic is one thing a stream follows: a stop's departures, a line's
// buses or the buses in an area.
type topic struct {
	name    string // as given, echoed in events
	kind    string // "departures" or "vehicles"
	source  string // "" = any source
	compute func() (any, error)
	last    []byte
}

// parseTopics reads the stream's query:
//
//	departures=<source>:<stop>[,<n>]  (repeatable)
//	line=<source>:<line>              (repeatable)
//	bbox=<minLat>,<minLon>,<maxLat>,<maxLon>
func (a *API) parseTopics(r *http.Request) ([]*topic, error) {
	q := r.URL.Query()
	var ts []*topic
	for _, v := range q["departures"] {
		src, rest, ok := strings.Cut(v, ":")
		if !ok || rest == "" {
			return nil, fmt.Errorf("departures=%q: want <source>:<stop>[,<n>]", v)
		}
		s := a.Svc.Source(src)
		if s == nil {
			return nil, fmt.Errorf("unknown source %q", src)
		}
		stop, nStr, _ := strings.Cut(rest, ",")
		n := defaultN
		if nStr != "" {
			x, err := strconv.Atoi(nStr)
			if err != nil || x < 1 || x > maxN {
				return nil, fmt.Errorf("departures=%q: n must be 1..%d", v, maxN)
			}
			n = x
		}
		ts = append(ts, &topic{name: "departures=" + v, kind: "departures", source: src,
			compute: func() (any, error) { return s.Departures(stop, n, defaultWindow) }})
	}
	for _, v := range q["line"] {
		src, line, ok := strings.Cut(v, ":")
		s := a.Svc.Source(src)
		if !ok || line == "" || s == nil {
			return nil, fmt.Errorf("line=%q: want <source>:<line> with a known source", v)
		}
		ts = append(ts, &topic{name: "line=" + v, kind: "vehicles", source: src,
			compute: func() (any, error) { return s.Vehicles(line, nil), nil }})
	}
	if v := q.Get("bbox"); v != "" {
		box, err := parseBBox(v)
		if err != nil {
			return nil, err
		}
		ts = append(ts, &topic{name: "bbox=" + v, kind: "vehicles",
			compute: func() (any, error) { return a.vehiclesIn(box), nil }})
	}
	if len(ts) == 0 {
		return nil, fmt.Errorf("subscribe to something: departures=, line= or bbox=")
	}
	if len(ts) > 20 {
		return nil, fmt.Errorf("at most 20 topics per stream")
	}
	return ts, nil
}

type event struct {
	Topic string `json:"topic"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

// stream is a Server-Sent Events stream: on connect the current state of
// every topic, then a new event for a topic each time its data changes.
// To change topics, open a new stream.
func (a *API) stream(w http.ResponseWriter, r *http.Request) {
	topics, err := a.parseTopics(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // nginx: don't buffer
	w.WriteHeader(http.StatusOK)

	changes, unsubscribe := a.Svc.Subscribe()
	defer unsubscribe()
	metrics.StreamClients.Inc()
	defer metrics.StreamClients.Dec()

	var id uint64
	if v, err := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64); err == nil {
		id = v
	}
	send := func(t *topic, force bool) error {
		v, err := t.compute()
		ev := event{Topic: t.name, Data: v}
		if err != nil {
			ev = event{Topic: t.name, Error: err.Error()}
		}
		b, _ := json.Marshal(ev)
		if !force && bytes.Equal(b, t.last) {
			return nil
		}
		t.last = b
		id++
		rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", id, t.kind, b); err != nil {
			return err
		}
		metrics.StreamEvents.WithLabelValues(t.kind).Inc()
		return rc.Flush()
	}

	fmt.Fprint(w, "retry: 3000\n\n")
	for _, t := range topics {
		if send(t, true) != nil {
			return
		}
	}
	hb := a.Heartbeat
	if hb == 0 {
		hb = 20 * time.Second
	}
	ticker := time.NewTicker(hb)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		case src := <-changes:
			changed := map[string]bool{src: true}
			// Coalesce a burst (vehicle and trip feeds often land together).
			timer := time.NewTimer(50 * time.Millisecond)
		drain:
			for {
				select {
				case s := <-changes:
					changed[s] = true
				case <-timer.C:
					break drain
				}
			}
			for _, t := range topics {
				if t.source == "" || changed[t.source] {
					if send(t, false) != nil {
						return
					}
				}
			}
		}
	}
}
