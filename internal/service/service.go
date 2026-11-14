// Package service runs the sources: fetches their static and realtime feeds,
// archives them, feeds the engine and tells listeners when state changes.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"git.marcorealacci.me/marcorealacci/tramite/internal/archive"
	"git.marcorealacci.me/marcorealacci/tramite/internal/config"
	"git.marcorealacci.me/marcorealacci/tramite/internal/engine"
	"git.marcorealacci.me/marcorealacci/tramite/internal/fetch"
	"git.marcorealacci.me/marcorealacci/tramite/internal/gtfs"
	"git.marcorealacci.me/marcorealacci/tramite/internal/live"
	"git.marcorealacci.me/marcorealacci/tramite/internal/metrics"
)

const userAgent = "tramite/0.1"

type Service struct {
	Cfg     *config.Config
	Sources []*engine.Source
	byID    map[string]*engine.Source
	client  *http.Client
	archive archive.Archive
	log     *slog.Logger

	mu        sync.Mutex
	listeners []chan string
	pollers   map[string]*fetch.Poller // "<source>/<feed>"
}

func New(cfg *config.Config, log *slog.Logger) *Service {
	s := &Service{Cfg: cfg, byID: map[string]*engine.Source{}, log: log,
		client:  &http.Client{Timeout: 60 * time.Second},
		archive: archive.Archive{Dir: cfg.DataDir}, pollers: map[string]*fetch.Poller{}}
	for _, sc := range cfg.Sources {
		src := engine.NewSource(sc, filepath.Join(cfg.DataDir, "state", sc.ID))
		s.Sources = append(s.Sources, src)
		s.byID[sc.ID] = src
	}
	return s
}

func (s *Service) Source(id string) *engine.Source { return s.byID[id] }

// Subscribe returns a channel receiving the id of each source that changed.
// Slow listeners miss notifications (they read the latest state anyway).
func (s *Service) Subscribe() (<-chan string, func()) {
	ch := make(chan string, 16)
	s.mu.Lock()
	s.listeners = append(s.listeners, ch)
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for i, l := range s.listeners {
			if l == ch {
				s.listeners = append(s.listeners[:i], s.listeners[i+1:]...)
				break
			}
		}
	}
}

// Notify tells stream listeners that a source changed.
func (s *Service) Notify(source string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.listeners {
		select {
		case l <- source:
		default:
		}
	}
}

// FeedStatus is the fetch schedule learned for one feed.
type FeedStatus struct {
	Interval time.Duration `json:"intervalNs"`
	Stale    bool          `json:"stale"`
}

func (s *Service) FeedStatus(source, feed string) (FeedStatus, bool) {
	s.mu.Lock()
	p := s.pollers[source+"/"+feed]
	s.mu.Unlock()
	if p == nil {
		return FeedStatus{}, false
	}
	return FeedStatus{Interval: p.Interval(), Stale: p.Stale(time.Now())}, true
}

// Run fetches all sources until ctx ends.
func (s *Service) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, src := range s.Sources {
		ready := make(chan struct{})
		wg.Add(1)
		go func() { defer wg.Done(); s.runStatic(ctx, src, ready) }()
		rtc := src.Cfg.Realtime
		for feed, url := range map[string]string{
			"vehicle_positions": rtc.VehiclePositions, "trip_updates": rtc.TripUpdates, "alerts": rtc.Alerts,
		} {
			if url == "" {
				continue
			}
			wg.Add(1)
			go func() { defer wg.Done(); s.runRealtime(ctx, src, feed, url, ready) }()
		}
	}
	wg.Wait()
}

type staticMeta struct {
	LastModified string `json:"lastModified"`
	Version      string `json:"version"`
}

func (s *Service) runStatic(ctx context.Context, src *engine.Source, ready chan struct{}) {
	cfg := src.Cfg
	dir := filepath.Join(s.Cfg.DataDir, "static", cfg.ID)
	current := filepath.Join(dir, "current.zip")
	metaPath := filepath.Join(dir, "current.json")
	var loc *time.Location
	if cfg.Timezone != "" {
		l, err := time.LoadLocation(cfg.Timezone)
		if err != nil {
			s.log.Error("bad timezone", "source", cfg.ID, "err", err)
		}
		loc = l
	}
	var once sync.Once
	markReady := func() { once.Do(func() { close(ready) }) }
	load := func(b []byte) bool {
		sum := sha256.Sum256(b)
		version := hex.EncodeToString(sum[:])[:12]
		t0 := time.Now()
		f, err := gtfs.Load(b, loc, version)
		if err != nil {
			s.log.Error("static GTFS load failed", "source", cfg.ID, "err", err)
			return false
		}
		src.SetStatic(f)
		runtime.GC() // the zip, CSV buffers and any previous timetable
		debug.FreeOSMemory()
		s.log.Info("static GTFS loaded", "source", cfg.ID, "version", version, "trips", len(f.Trips),
			"stop_times", len(f.StopTimes), "took", time.Since(t0).Round(time.Millisecond))
		markReady()
		s.Notify(cfg.ID)
		return true
	}

	p := &fetch.Poller{Client: s.client, URL: cfg.Static.URL, Headers: s.headers(cfg)}
	if b, err := os.ReadFile(current); err == nil && load(b) {
		// Ask for changes since the copy we have.
		var m staticMeta
		if mb, err := os.ReadFile(metaPath); err == nil && json.Unmarshal(mb, &m) == nil {
			p.SeedLastModified(m.LastModified, b)
		}
	}
	for {
		res, err := p.Fetch(ctx)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			metrics.Fetches.WithLabelValues(cfg.ID, "static", "error").Inc()
			s.log.Warn("static fetch failed", "source", cfg.ID, "err", err)
		case res.NotModified:
			metrics.Fetches.WithLabelValues(cfg.ID, "static", "not_modified").Inc()
		case !res.New:
			metrics.Fetches.WithLabelValues(cfg.ID, "static", "unchanged").Inc()
		default:
			metrics.Fetches.WithLabelValues(cfg.ID, "static", "new").Inc()
			if load(res.Body) {
				os.MkdirAll(dir, 0o755)
				writeAtomic(current, res.Body)
				mb, _ := json.Marshal(staticMeta{LastModified: p.LastModified(), Version: src.Status().StaticVersion})
				writeAtomic(metaPath, mb)
				if cfg.Archive {
					name := res.FetchedAt.Format("20060102-150405") + "_" + src.Status().StaticVersion + ".zip"
					writeAtomic(filepath.Join(dir, name), res.Body)
				}
			}
		}
		wait := cfg.Static.CheckEvery.Duration
		if !src.Ready() {
			wait = time.Minute // nothing to serve yet: retry soon
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (s *Service) runRealtime(ctx context.Context, src *engine.Source, feed, url string, ready chan struct{}) {
	cfg := src.Cfg
	select {
	case <-ctx.Done():
		return
	case <-ready:
	}
	p := &fetch.Poller{Client: s.client, URL: url, Headers: s.headers(cfg),
		MinPoll: cfg.Realtime.MinPoll.Duration, FastPoll: cfg.Realtime.FastPoll.Duration}
	s.mu.Lock()
	s.pollers[cfg.ID+"/"+feed] = p
	s.mu.Unlock()
	for {
		res, err := p.Fetch(ctx)
		now := time.Now()
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			metrics.Fetches.WithLabelValues(cfg.ID, feed, "error").Inc()
			s.log.Warn("realtime fetch failed", "source", cfg.ID, "feed", feed, "err", err)
		case res.NotModified:
			metrics.Fetches.WithLabelValues(cfg.ID, feed, "not_modified").Inc()
		case !res.New:
			metrics.Fetches.WithLabelValues(cfg.ID, feed, "unchanged").Inc()
		default:
			metrics.Fetches.WithLabelValues(cfg.ID, feed, "new").Inc()
			s.handle(src, p, feed, res)
		}
		if v := p.LastVersion(); !v.IsZero() {
			metrics.FeedAge.WithLabelValues(cfg.ID, feed).Set(now.Sub(v).Seconds())
		}
		stale := p.Stale(now)
		metrics.Stale.WithLabelValues(cfg.ID, feed).Set(b2f(stale))
		metrics.FeedInterval.WithLabelValues(cfg.ID, feed).Set(p.Interval().Seconds())
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.NextDelay(time.Now())):
		}
	}
}

func (s *Service) handle(src *engine.Source, p *fetch.Poller, feed string, res fetch.Result) {
	cfg := src.Cfg
	t0 := time.Now()
	var ts time.Time
	var err error
	switch feed {
	case "vehicle_positions":
		var vs []live.Vehicle
		if ts, vs, err = live.ParseVehicles(res.Body); err == nil {
			src.ApplyVehicles(ts, vs)
		}
	case "trip_updates":
		var tus []live.TripUpdate
		if ts, tus, err = live.ParseTripUpdates(res.Body); err == nil {
			src.ApplyTripUpdates(ts, tus, res.Body)
		}
	default:
		ts, err = live.HeaderTime(res.Body)
	}
	if err != nil {
		metrics.Fetches.WithLabelValues(cfg.ID, feed, "error").Inc()
		s.log.Warn("bad realtime feed", "source", cfg.ID, "feed", feed, "err", err)
		return
	}
	p.Observe(ts, res.FetchedAt)
	if !ts.IsZero() {
		metrics.FetchLag.WithLabelValues(cfg.ID, feed).Observe(res.FetchedAt.Sub(ts).Seconds())
	}
	if feed != "alerts" {
		metrics.ProcessSeconds.WithLabelValues(cfg.ID).Observe(time.Since(t0).Seconds())
		s.Notify(cfg.ID)
	}
	if cfg.Archive {
		if err := s.archive.Save(cfg.ID, feed, res.FetchedAt, ts, res.Body); err != nil {
			s.log.Warn("archive failed", "source", cfg.ID, "feed", feed, "err", err)
		}
	}
}

func (s *Service) headers(cfg config.Source) map[string]string {
	h := map[string]string{"User-Agent": userAgent}
	for k, v := range cfg.Headers {
		h[k] = v
	}
	return h
}

func writeAtomic(path string, b []byte) {
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, path)
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
