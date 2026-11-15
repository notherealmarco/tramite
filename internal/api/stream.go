package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"git.marcorealacci.me/marcorealacci/tramite/internal/engine"
	"git.marcorealacci.me/marcorealacci/tramite/internal/metrics"
)

// Publish/subscribe over Server-Sent Events. A client keeps one stream open
// (GET /v1/stream); the first event ("hello") carries the stream's id. It then
// adds and removes topics with POST /v1/stream/{id}/topics. Each added topic
// gets its current state right away, then a new event whenever it changes.
//
// Topics, written as in the POST body (and accepted as query parameters of
// GET /v1/stream for a first set):
//
//	departures=<stop>[,<n>][,trip=<trip>[,line=<line>]]
//	                                         a stop's departures; with trip=, that trip's row only
//	                                         (line= helps match a trip id from another timetable version)
//	line=<prefix>_<number>                   buses of a line (every route with that short name)
//	route=<prefix>_<route_id>                buses of one GTFS route
//	bbox=<minLat>,<minLon>,<maxLat>,<maxLon> buses in an area (all sources)
//
// Stop, line, route and trip ids are the API's (Transitous) ids.

const maxTopics = 200

// A topic is one thing a stream follows.
type topic struct {
	name    string // as written by the client, echoed in events
	kind    string // event name: "departures", "trip" or "vehicles"
	source  string // source whose changes affect it; "" = any
	compute func() (any, error)
	last    []byte // last data sent
}

// parseTopic reads one topic ("departures=…", "line=…", ...).
func (a *API) parseTopic(name string) (*topic, error) {
	kind, v, ok := strings.Cut(name, "=")
	if !ok || v == "" {
		return nil, fmt.Errorf("topic %q: want <kind>=<value>", name)
	}
	switch kind {
	case "departures":
		parts := strings.Split(v, ",")
		stop, n, trip, line := parts[0], "", "", ""
		for _, p := range parts[1:] {
			if t, ok := strings.CutPrefix(p, "trip="); ok {
				trip = t
			} else if l, ok := strings.CutPrefix(p, "line="); ok {
				line = l
			} else {
				n = p
			}
		}
		s := a.owner(stop)
		if s == nil {
			return nil, fmt.Errorf("topic %q: no source covers this stop", name)
		}
		if trip == "" {
			if _, _, err := boardParams(n, ""); err != nil {
				return nil, fmt.Errorf("topic %q: %w", name, err)
			}
		}
		return &topic{name: name, kind: "departures", source: s.Cfg.ID,
			compute: func() (any, error) { return a.board(s, stop, trip, line, n, "") }}, nil
	case "trip":
		parts := strings.Split(v, ",")
		trip, line, stop := parts[0], "", ""
		for _, p := range parts[1:] {
			if l, ok := strings.CutPrefix(p, "line="); ok {
				line = l
			} else if x, ok := strings.CutPrefix(p, "stop="); ok {
				stop = x
			} else {
				return nil, fmt.Errorf("topic %q: unknown option %q (line=, stop=)", name, p)
			}
		}
		s := a.owner(trip)
		if s == nil {
			return nil, fmt.Errorf("topic %q: no source covers this trip", name)
		}
		return &topic{name: name, kind: "trip", source: s.Cfg.ID,
			compute: func() (any, error) { return s.Trip(trip, line, stop) }}, nil
	case "line", "route":
		s := a.owner(v)
		if s == nil {
			return nil, fmt.Errorf("topic %q: no source covers this %s", name, kind)
		}
		f := engine.VehicleFilter{Line: v}
		if kind == "route" {
			f = engine.VehicleFilter{Route: v}
		}
		return &topic{name: name, kind: "vehicles", source: s.Cfg.ID,
			compute: func() (any, error) { return s.Vehicles(f), nil }}, nil
	case "bbox":
		box, err := parseBBox(v)
		if err != nil {
			return nil, fmt.Errorf("topic %q: %w", name, err)
		}
		return &topic{name: name, kind: "vehicles",
			compute: func() (any, error) { return a.vehiclesIn(box), nil }}, nil
	}
	return nil, fmt.Errorf("topic %q: unknown kind %q (departures, trip, line, route, bbox)", name, kind)
}

// change is a subscription update sent to a stream's goroutine.
type change struct {
	add    []*topic
	remove []string
	done   chan error
}

type streamConn struct {
	changes chan change
}

type registry struct {
	mu      sync.Mutex
	streams map[string]*streamConn
}

func (r *registry) put(id string, s *streamConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.streams == nil {
		r.streams = map[string]*streamConn{}
	}
	r.streams[id] = s
}

func (r *registry) get(id string) *streamConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.streams[id]
}

func (r *registry) drop(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.streams, id)
}

type event struct {
	Topic string `json:"topic"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

// stream serves GET /v1/stream.
func (a *API) stream(w http.ResponseWriter, r *http.Request) {
	var initial []*topic
	for kind, vs := range r.URL.Query() {
		for _, v := range vs {
			t, err := a.parseTopic(kind + "=" + v)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			initial = append(initial, t)
		}
	}
	if len(initial) > maxTopics {
		http.Error(w, fmt.Sprintf("at most %d topics per stream", maxTopics), http.StatusBadRequest)
		return
	}
	var raw [16]byte
	rand.Read(raw[:])
	id := hex.EncodeToString(raw[:])
	conn := &streamConn{changes: make(chan change)}
	a.streams.put(id, conn)
	defer a.streams.drop(id)

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // nginx: don't buffer
	w.WriteHeader(http.StatusOK)

	updates, unsubscribe := a.Svc.Subscribe()
	defer unsubscribe()
	metrics.StreamClients.Inc()
	defer metrics.StreamClients.Dec()

	var seq uint64
	write := func(kind string, b []byte) error {
		seq++
		rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", seq, kind, b); err != nil {
			return err
		}
		metrics.StreamEvents.WithLabelValues(kind).Inc()
		return rc.Flush()
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
		return write(t.kind, b)
	}

	topics := map[string]*topic{}
	fmt.Fprint(w, "retry: 3000\n\n")
	hello, _ := json.Marshal(map[string]string{"id": id})
	if write("hello", hello) != nil {
		return
	}
	for _, t := range initial {
		topics[t.name] = t
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
		case c := <-conn.changes:
			for _, name := range c.remove {
				delete(topics, name)
			}
			n := len(topics)
			for _, t := range c.add {
				if _, ok := topics[t.name]; !ok {
					n++
				}
			}
			if n > maxTopics {
				c.done <- fmt.Errorf("at most %d topics per stream", maxTopics)
				continue
			}
			var fresh []*topic
			for _, t := range c.add {
				if _, ok := topics[t.name]; !ok {
					topics[t.name] = t
					fresh = append(fresh, t)
				}
			}
			c.done <- nil
			for _, t := range fresh {
				if send(t, true) != nil {
					return
				}
			}
		case src := <-updates:
			changed := map[string]bool{src: true}
			// Coalesce a burst (vehicle and trip feeds often land together).
			timer := time.NewTimer(50 * time.Millisecond)
		drain:
			for {
				select {
				case s := <-updates:
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

// topicsBody is the POST /v1/stream/{id}/topics request.
type topicsBody struct {
	Add    []string `json:"add"`
	Remove []string `json:"remove"`
}

// updateTopics serves POST /v1/stream/{id}/topics: adds and removes topics
// of an open stream. Removing a topic it doesn't follow is not an error;
// adding one it already follows does nothing. 204 when applied.
func (a *API) updateTopics(w http.ResponseWriter, r *http.Request) {
	conn := a.streams.get(r.PathValue("id"))
	if conn == nil {
		http.Error(w, "unknown stream (closed?): open a new one", http.StatusNotFound)
		return
	}
	var body topicsBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&body); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	c := change{remove: body.Remove, done: make(chan error, 1)}
	for _, name := range body.Add {
		t, err := a.parseTopic(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.add = append(c.add, t)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	select {
	case conn.changes <- c:
	case <-ctx.Done():
		http.Error(w, "stream busy or closed", http.StatusServiceUnavailable)
		return
	}
	select {
	case err := <-c.done:
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case <-ctx.Done():
		http.Error(w, "stream busy or closed", http.StatusServiceUnavailable)
	}
}
