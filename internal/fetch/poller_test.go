package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestConditionalFetch(t *testing.T) {
	body := "v1"
	lm := "Fri, 13 Nov 2026 13:00:00 GMT"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Modified-Since") == lm {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Last-Modified", lm)
		w.Write([]byte(body))
	}))
	defer srv.Close()
	p := &Poller{URL: srv.URL, MinPoll: 15 * time.Second, FastPoll: 2 * time.Second}
	ctx := context.Background()
	if p.NextDelay(time.Now()) != 15*time.Second {
		t.Fatal("before any 304: poll politely at MinPoll")
	}
	r, err := p.Fetch(ctx)
	if err != nil || !r.New || string(r.Body) != "v1" {
		t.Fatalf("first fetch: %+v %v", r, err)
	}
	r, err = p.Fetch(ctx)
	if err != nil || !r.NotModified {
		t.Fatalf("second fetch should be 304: %+v %v", r, err)
	}
	if p.NextDelay(time.Now()) != 2*time.Second {
		t.Fatal("with 304 and no history: FastPoll")
	}
	// Same content under a new date: not new.
	lm = "Fri, 13 Nov 2026 13:00:30 GMT"
	if r, _ = p.Fetch(ctx); r.New {
		t.Fatal("identical content reported as new")
	}
}

func TestLearnedSchedule(t *testing.T) {
	p := &Poller{MinPoll: 15 * time.Second, FastPoll: 2 * time.Second, supports304: true}
	t0 := time.Date(2026, 11, 13, 14, 0, 0, 0, time.UTC)
	// A version every 30 s.
	for i := 0; i < 10; i++ {
		v := t0.Add(time.Duration(i*30) * time.Second)
		p.Observe(v, v.Add(time.Second))
	}
	if p.Interval() != 30*time.Second {
		t.Fatalf("interval %s", p.Interval())
	}
	last := t0.Add(9*30*time.Second + time.Second)
	// Right after a version: wait until ~4 s before the next (30 - 2*2).
	if d := p.NextDelay(last); d < 25*time.Second || d > 27*time.Second {
		t.Fatalf("wait after a version: %s", d)
	}
	// Close to the expected time: check fast.
	if d := p.NextDelay(last.Add(27 * time.Second)); d != 2*time.Second {
		t.Fatalf("near expected update: %s", d)
	}
	if p.Stale(last.Add(time.Minute)) {
		t.Fatal("not stale after 1 min")
	}
	if !p.Stale(last.Add(3 * time.Minute)) {
		t.Fatal("stale after 3 min without versions (interval 30 s, floor 2 min)")
	}
	if d := p.NextDelay(last.Add(3 * time.Minute)); d != 10*time.Second {
		t.Fatalf("long overdue: back off to 5x FastPoll, got %s", d)
	}
}
