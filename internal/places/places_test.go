package places

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"git.marcorealacci.me/marcorealacci/tramite/internal/config"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeGoogle answers like the Places API (New) and records what it was sent.
type fakeGoogle struct {
	srv        *httptest.Server
	lastBody   map[string]any
	lastMask   string
	lastQuery  string
	autoStatus atomic.Int32
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	f := &fakeGoogle{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Goog-Api-Key") != "k" {
			http.Error(w, `{"error":{"status":"PERMISSION_DENIED"}}`, 403)
			return
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/places:autocomplete":
			if s := f.autoStatus.Load(); s != 0 {
				http.Error(w, `{"error":{"status":"RESOURCE_EXHAUSTED"}}`, int(s))
				return
			}
			f.lastBody = nil
			json.NewDecoder(r.Body).Decode(&f.lastBody)
			w.Write([]byte(`{"suggestions":[
				{"placePrediction":{"placeId":"P1","text":{"text":"Colosseo, Roma"},
				 "structuredFormat":{"mainText":{"text":"Colosseo"},"secondaryText":{"text":"Piazza del Colosseo, Roma"}},
				 "types":["tourist_attraction","point_of_interest","establishment"],"distanceMeters":850}},
				{"queryPrediction":{"text":{"text":"colosseo tickets"}}},
				{"placePrediction":{"placeId":"P2","text":{"text":"Via del Corso 1, Roma"},
				 "structuredFormat":{"mainText":{"text":"Via del Corso 1"},"secondaryText":{"text":"Roma RM"}},
				 "types":["street_address","geocode"]}}]}`))
		case r.Method == "GET" && r.URL.Path == "/v1/places/P1":
			f.lastMask, f.lastQuery = r.Header.Get("X-Goog-FieldMask"), r.URL.RawQuery
			w.Write([]byte(`{"location":{"latitude":41.8902,"longitude":12.4922}}`))
		default:
			http.Error(w, `{"error":{"status":"NOT_FOUND"}}`, 404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newService(t *testing.T, f *fakeGoogle, c *clock, limit config.MonthlyLimit) (*Service, *Budget) {
	g := &Google{Key: "k", Region: "it", RadiusMeters: 30000, BaseURL: f.srv.URL,
		Bias: &config.Circle{Lat: 41.9, Lon: 12.5, RadiusMeters: 20000}}
	b := NewBudget(filepath.Join(t.TempDir(), "usage.json"), limit, c.now, quiet)
	return NewService("google", "Google Maps", g, b, NewLimiter(0, 0, c.now), quiet), b
}

func TestAutocompleteAndLocate(t *testing.T) {
	f := newFakeGoogle(t)
	c := &clock{time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	s, b := newService(t, f, c, config.MonthlyLimit{Autocomplete: 100, Details: 100})
	ctx := context.Background()

	res, err := s.Autocomplete(ctx, Query{Text: "colos", Lang: "it", Session: "S", Near: &LatLon{41.89, 12.49}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].ID != "google:P1" || res[0].Name != "Colosseo" || res[0].Kind != "place" ||
		res[0].DistanceMeters == nil || *res[0].DistanceMeters != 850 || res[1].Kind != "address" || res[1].DistanceMeters != nil {
		t.Fatalf("suggestions %+v", res)
	}
	if f.lastBody["sessionToken"] != "S" || f.lastBody["regionCode"] != "it" || f.lastBody["origin"] == nil {
		t.Fatalf("sent %v", f.lastBody)
	}
	circle := f.lastBody["locationBias"].(map[string]any)["circle"].(map[string]any)
	if circle["radius"] != 30000.0 || circle["center"].(map[string]any)["latitude"] != 41.89 {
		t.Fatalf("bias %v", circle)
	}
	// Without a position: the configured bias.
	s.Autocomplete(ctx, Query{Text: "colos", Session: "S"})
	if circle := f.lastBody["locationBias"].(map[string]any)["circle"].(map[string]any); circle["radius"] != 20000.0 || f.lastBody["origin"] != nil {
		t.Fatalf("default bias %v", f.lastBody)
	}
	if a, _ := b.Usage(); a != 2 {
		t.Fatalf("open session counts %d, want 2", a)
	}

	loc, err := s.Locate(ctx, "google:P1", "S", "it")
	if err != nil {
		t.Fatal(err)
	}
	if loc.ID != "google:P1" || loc.Lat != 41.8902 || f.lastMask != "location" || f.lastQuery != "languageCode=it&sessionToken=S" {
		t.Fatalf("location %+v mask %q query %q", loc, f.lastMask, f.lastQuery)
	}
	// The session ended with details: its suggestions are free.
	if a, d := b.Usage(); a != 0 || d != 1 {
		t.Fatalf("usage %d/%d, want 0/1", a, d)
	}
	for _, id := range []string{"P1", "other:P1", "google:", "google:NOPE"} {
		if _, err := s.Locate(ctx, id, "", ""); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", id, err)
		}
	}
}

func TestMonthlyQuota(t *testing.T) {
	f := newFakeGoogle(t)
	// 20:00 UTC on 30 Nov is still November in California.
	c := &clock{time.Date(2026, 11, 30, 20, 0, 0, 0, time.UTC)}
	s, b := newService(t, f, c, config.MonthlyLimit{Autocomplete: 2, Details: 1})
	ctx := context.Background()
	s.Autocomplete(ctx, Query{Text: "a", Session: "X"}) // abandoned
	s.Autocomplete(ctx, Query{Text: "b"})               // no session: billed alone
	_, err := s.Autocomplete(ctx, Query{Text: "c"})
	var lim *LimitError
	if !errors.As(err, &lim) || lim.Reason != "quota" {
		t.Fatalf("want quota, got %v", err)
	}
	// Until 1 Dec 00:00 Pacific (08:00 UTC).
	if want := 12 * time.Hour; lim.RetryAfter != want {
		t.Fatalf("retry after %s, want %s", lim.RetryAfter, want)
	}
	// Counts survive a restart.
	b2 := NewBudget(b.path, b.limit, c.now, quiet)
	if a, _ := b2.Usage(); a != 2 {
		t.Fatalf("reloaded %d", a)
	}
	// Details used up: suggestions stop too (the pick couldn't be located).
	c.t = time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)
	if _, err := s.Autocomplete(ctx, Query{Text: "d", Session: "Y"}); err != nil {
		t.Fatalf("new month: %v", err)
	}
	if _, err := s.Locate(ctx, "google:P1", "Y", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Autocomplete(ctx, Query{Text: "e"}); !errors.As(err, &lim) {
		t.Fatalf("details used up, want quota: %v", err)
	}
}

func TestUpstreamLimit(t *testing.T) {
	f := newFakeGoogle(t)
	c := &clock{time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	s, _ := newService(t, f, c, config.MonthlyLimit{Autocomplete: 100, Details: 100})
	f.autoStatus.Store(429)
	_, err := s.Autocomplete(context.Background(), Query{Text: "a"})
	var lim *LimitError
	if !errors.As(err, &lim) || lim.Reason != "quota" || lim.RetryAfter != time.Minute {
		t.Fatalf("got %v", err)
	}
	f.autoStatus.Store(500)
	if _, err := s.Autocomplete(context.Background(), Query{Text: "a"}); err == nil || errors.As(err, &lim) {
		t.Fatalf("want plain error, got %v", err)
	}
}

func TestLimiter(t *testing.T) {
	c := &clock{time.Date(2026, 11, 19, 23, 59, 30, 0, time.UTC)}
	l := NewLimiter(2, 3, c.now)
	for i := range 2 {
		if _, ok := l.Allow("a"); !ok {
			t.Fatalf("request %d refused", i)
		}
	}
	if wait, ok := l.Allow("a"); ok || wait != 30*time.Second {
		t.Fatalf("minute: %v %s", ok, wait)
	}
	if _, ok := l.Allow("b"); !ok {
		t.Fatal("other client refused")
	}
	c.t = time.Date(2026, 11, 19, 23, 59, 59, 0, time.UTC) // same minute
	if _, ok := l.Allow("a"); ok {
		t.Fatal("same minute allowed")
	}
	c.t = time.Date(2026, 11, 20, 0, 0, 5, 0, time.UTC) // new day
	if _, ok := l.Allow("a"); !ok {
		t.Fatal("new day refused")
	}
	l2 := NewLimiter(0, 1, c.now)
	l2.Allow("a")
	if wait, ok := l2.Allow("a"); ok || wait != 24*time.Hour-5*time.Second {
		t.Fatalf("day: %v %s", ok, wait)
	}
}

func TestNewOff(t *testing.T) {
	env := func(string) string { return "" }
	if New(config.Places{}, t.TempDir(), env, quiet) != nil {
		t.Fatal("no provider: want off")
	}
	if New(config.Places{Provider: "google", APIKeyEnv: "K"}, t.TempDir(), env, quiet) != nil {
		t.Fatal("no key: want off")
	}
	s := New(config.Places{Provider: "google", APIKeyEnv: "K"}, t.TempDir(), func(string) string { return "k" }, quiet)
	if s == nil || s.Name != "google" || s.Attribution != "Google Maps" {
		t.Fatalf("got %+v", s)
	}
}
