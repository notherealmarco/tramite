// Package fetch downloads upstream feeds with conditional requests and learns
// when each one publishes, to fetch new versions as soon as they appear.
package fetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Result of one check.
type Result struct {
	Body []byte
	// New: content differs from the last version seen.
	New       bool
	FetchedAt time.Time
	// NotModified: the upstream answered 304.
	NotModified bool
}

// Poller fetches one URL. Not safe for concurrent Fetch calls; the schedule
// accessors are.
type Poller struct {
	Client  *http.Client
	URL     string
	Headers map[string]string
	// MinPoll: interval when the upstream can't answer "not modified".
	MinPoll time.Duration
	// FastPoll: interval around an expected update when it can.
	FastPoll time.Duration

	mu           sync.Mutex
	lastModified string
	etag         string
	hash         [32]byte
	supports304  bool
	// versions: header timestamps (or arrival times) of the last versions.
	versions []time.Time
	lastNew  time.Time // local time the newest version was seen
}

func (p *Poller) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return http.DefaultClient
}

// Fetch does one conditional GET.
func (p *Poller) Fetch(ctx context.Context) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return Result{}, err
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	p.mu.Lock()
	if p.lastModified != "" {
		req.Header.Set("If-Modified-Since", p.lastModified)
	}
	if p.etag != "" {
		req.Header.Set("If-None-Match", p.etag)
	}
	p.mu.Unlock()
	resp, err := p.client().Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	now := time.Now()
	if resp.StatusCode == http.StatusNotModified {
		p.mu.Lock()
		p.supports304 = true
		p.mu.Unlock()
		io.Copy(io.Discard, resp.Body)
		return Result{FetchedAt: now, NotModified: true}, nil
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return Result{}, fmt.Errorf("%s: HTTP %d", p.URL, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Result{}, err
	}
	h := sha256.Sum256(body)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastModified = resp.Header.Get("Last-Modified")
	p.etag = resp.Header.Get("ETag")
	if h == p.hash && len(body) > 0 {
		return Result{Body: body, FetchedAt: now}, nil
	}
	p.hash = h
	return Result{Body: bytes.Clone(body), New: true, FetchedAt: now}, nil
}

// Observe records a new version: its own timestamp (feed header) when
// known, else the time it was seen. Feeds the learned schedule.
func (p *Poller) Observe(version, seenAt time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if version.IsZero() {
		version = seenAt
	}
	if n := len(p.versions); n > 0 && !version.After(p.versions[n-1]) {
		p.lastNew = seenAt
		return
	}
	p.versions = append(p.versions, version)
	if len(p.versions) > 30 {
		p.versions = p.versions[len(p.versions)-30:]
	}
	p.lastNew = seenAt
}

// intervals between consecutive versions, sorted.
func (p *Poller) intervals() []time.Duration {
	var d []time.Duration
	for i := 1; i < len(p.versions); i++ {
		if x := p.versions[i].Sub(p.versions[i-1]); x > 0 && x < time.Hour {
			d = append(d, x)
		}
	}
	sort.Slice(d, func(a, b int) bool { return d[a] < d[b] })
	return d
}

// Interval is the learned typical time between versions (0 if unknown).
func (p *Poller) Interval() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.intervals()
	if len(d) < 2 {
		return 0
	}
	return d[len(d)/2]
}

// Stale: no new version for much longer than usual.
func (p *Poller) Stale(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.intervals()
	if len(d) < 2 || p.lastNew.IsZero() {
		return false
	}
	limit := 3 * d[len(d)/2]
	if limit < 2*time.Minute {
		limit = 2 * time.Minute
	}
	return now.Sub(p.lastNew) > limit
}

// NextDelay says how long to wait before the next check.
//
// With conditional requests (304) checks are cheap: wait until shortly
// before the earliest expected update (the shortest recent interval after
// the last one), then check every FastPoll. Without them every check
// downloads the feed: poll every MinPoll.
func (p *Poller) NextDelay(now time.Time) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.supports304 {
		return p.MinPoll
	}
	d := p.intervals()
	if len(d) < 3 || p.lastNew.IsZero() {
		return p.FastPoll
	}
	shortest := d[len(d)/10] // ~10th percentile: robust to one odd gap
	expected := p.lastNew.Add(shortest - 2*p.FastPoll)
	if wait := expected.Sub(now); wait > p.FastPoll {
		return wait
	}
	// Long overdue (feed stuck): slow down, still cheap.
	if now.Sub(p.lastNew) > 3*d[len(d)/2] {
		return 5 * p.FastPoll
	}
	return p.FastPoll
}

// SeedLastModified primes the conditional request with a copy already held.
func (p *Poller) SeedLastModified(lastModified string, body []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastModified = lastModified
	p.hash = sha256.Sum256(body)
}

// LastModified as last sent by the upstream.
func (p *Poller) LastModified() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastModified
}

// LastVersion is the timestamp of the newest version seen (zero if none).
func (p *Poller) LastVersion() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.versions) == 0 {
		return time.Time{}
	}
	return p.versions[len(p.versions)-1]
}
