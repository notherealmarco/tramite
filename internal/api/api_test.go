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
	"net/url"
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
	resp, err := http.Get(srv.URL + "/v1/stops/rome_A/departures?n=2")
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
	req, _ := http.NewRequest("GET", srv.URL+"/v1/stops/rome_A/departures?n=2", nil)
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
		"/v1/stops/nope_A/departures":           404,
		"/v1/stops/rome_ZZ/departures":          404,
		"/v1/stops/rome_A/departures?n=0":       400,
		"/v1/stops/rome_A/departures?trip=nope": 404,
		// A trip id of another timetable version, matched by run.
		"/v1/stops/rome_A/departures?trip=20261113_14:33_rome_old&line=rome_60": 200,
		"/v1/stops/rome_A/departures?trip=20261113_14:33_rome_old&line=rome_61": 404,
		"/v1/vehicles?bbox=1,2,3": 400,
		"/readyz":                 200,
		"/metrics":                200,
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
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v1/stream?line=rome_60", nil)
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
	if d := next(); !strings.Contains(d, `"id":`) {
		t.Fatalf("hello first: %s", d)
	}
	if d := next(); !strings.Contains(d, `"topic":"line=rome_60"`) || !strings.Contains(d, `"data":[]`) {
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

// Pub/sub: one stream, topics added and removed with POST.
func TestStreamTopics(t *testing.T) {
	srv, src, svc := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v1/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	next := func() (string, string) {
		var ev string
		for sc.Scan() {
			if e, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
				ev = e
			}
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				return ev, d
			}
		}
		t.Fatal("stream ended")
		return "", ""
	}
	ev, d := next()
	var hello struct{ ID string }
	if json.Unmarshal([]byte(d), &hello); ev != "hello" || len(hello.ID) != 32 {
		t.Fatalf("hello: %s %s", ev, d)
	}
	post := func(body string) int {
		r, err := http.Post(srv.URL+"/v1/stream/"+hello.ID+"/topics", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if c := post(`{"add":["line=rome_60","departures=rome_A,trip=20261113_14:33_rome_out1"]}`); c != 204 {
		t.Fatalf("add: %d", c)
	}
	got := map[string]string{}
	for range 2 {
		ev, d := next()
		var e struct{ Topic string }
		json.Unmarshal([]byte(d), &e)
		got[e.Topic] = ev
	}
	if got["line=rome_60"] != "vehicles" || got["departures=rome_A,trip=20261113_14:33_rome_out1"] != "departures" {
		t.Fatalf("current state on subscribe: %v", got)
	}
	// Unsubscribe the line: a new bus on it is not pushed; the trip still is.
	if c := post(`{"remove":["line=rome_60"]}`); c != 204 {
		t.Fatalf("remove: %d", c)
	}
	ts, vs, _ := live.ParseVehicles(g.Vehicles(g.At(14, 30, 0), g.VP{Label: "5039", Trip: "out1", Route: "60", Seq: 1, Lat: 41.9, Lon: 12.5}))
	src.ApplyVehicles(ts, vs)
	svc.Notify("rome")
	if _, d := next(); !strings.Contains(d, `"topic":"departures=rome_A,trip=`) || !strings.Contains(d, `"id":"5039"`) {
		t.Fatalf("after change: %s", d)
	}
	for body, want := range map[string]int{
		`{"add":["nope=1"]}`:          400,
		`{"add":["departures=zz_A"]}`: 400,
		`not json`:                    400,
	} {
		if c := post(body); c != want {
			t.Errorf("%s: got %d want %d", body, c, want)
		}
	}
	r, _ := http.Post(srv.URL+"/v1/stream/deadbeef/topics", "application/json", strings.NewReader(`{}`))
	r.Body.Close()
	if r.StatusCode != 404 {
		t.Fatalf("unknown stream: %d", r.StatusCode)
	}
}

func TestAreaVehiclesTripRowAndGzip(t *testing.T) {
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

	// One trip's row at a stop: out1 (14:33) waits for 3429, ready 14:43.
	r2, err := http.Get(srv.URL + "/v1/stops/rome_A/departures?trip=" + url.QueryEscape("20261113_14:33_rome_out1"))
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	var b engine.Board
	if err := json.NewDecoder(r2.Body).Decode(&b); err != nil {
		t.Fatal(err)
	}
	if len(b.StopTimes) != 1 || b.StopTimes[0].DepartureTimestampReason != engine.ReasonInferred ||
		!b.StopTimes[0].Departure.Equal(g.At(14, 43, 0)) || b.StopTimes[0].Vehicle.ID != "3429" {
		t.Fatalf("trip row: %+v", b.StopTimes)
	}
}
