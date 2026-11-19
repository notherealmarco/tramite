package places

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
	_ "time/tzdata" // America/Los_Angeles without the OS database

	"git.marcorealacci.me/marcorealacci/tramite/internal/config"
	"git.marcorealacci.me/marcorealacci/tramite/internal/metrics"
)

// Budget keeps place search within the provider's free monthly allowance.
//
// Google bills a search session (the suggestions typed, then the picked
// place's details) as just the details: suggestions are free when the
// session ends with a details request, and billed one by one when it is
// abandoned. Budget counts every suggestion request as billable when made
// and takes a session's back when its details come: an abandoned session,
// or one forgotten across a restart, stays counted (never under-counts).
//
// Months are Google's billing months: calendar months, Pacific time. The
// counts survive restarts (a small JSON file).
type Budget struct {
	path  string
	limit config.MonthlyLimit
	now   func() time.Time
	log   *slog.Logger

	mu       sync.Mutex
	usage    usage
	sessions map[string]*session
	pruned   time.Time
}

type usage struct {
	Month string `json:"month"` // "2026-11"
	// Autocomplete: suggestion requests billable so far (abandoned sessions,
	// and open ones until they end).
	Autocomplete int `json:"autocomplete"`
	Details      int `json:"details"`
}

type session struct {
	requests int
	last     time.Time
}

// sessionTTL: a session quiet for this long is taken as abandoned (its
// requests stay counted). Google's own sessions last a few minutes.
const sessionTTL = 15 * time.Minute

var billing = mustLoad("America/Los_Angeles")

func mustLoad(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

func NewBudget(path string, limit config.MonthlyLimit, now func() time.Time, log *slog.Logger) *Budget {
	b := &Budget{path: path, limit: limit, now: now, log: log, sessions: map[string]*session{}}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &b.usage); err != nil {
			log.Warn("place search usage unreadable, counting from zero", "path", path, "err", err)
		}
	}
	b.rollover()
	b.publish()
	return b
}

// AllowSearch: whether a new suggestion request fits, else how long until
// next month. Suggestions are refused too once details are used up: the
// picked place could not be located.
func (b *Budget) AllowSearch() (time.Duration, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover()
	if b.usage.Autocomplete >= b.limit.Autocomplete || b.usage.Details >= b.limit.Details {
		return b.untilNextMonth(), false
	}
	return 0, true
}

func (b *Budget) AllowDetails() (time.Duration, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover()
	if b.usage.Details >= b.limit.Details {
		return b.untilNextMonth(), false
	}
	return 0, true
}

// RecordAutocomplete counts one suggestion request of session (may be empty:
// then it is billed alone).
func (b *Budget) RecordAutocomplete(sessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.rollover()
	b.prune(now)
	b.usage.Autocomplete++
	if sessionID != "" {
		s := b.sessions[sessionID]
		if s == nil {
			s = &session{}
			b.sessions[sessionID] = s
		}
		s.requests++
		s.last = now
	}
	b.save()
}

// RecordDetails counts one details request; it ends session, whose
// suggestions are then free.
func (b *Budget) RecordDetails(sessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover()
	b.usage.Details++
	if s := b.sessions[sessionID]; s != nil && sessionID != "" {
		// Requests made last month were reset with the month already.
		b.usage.Autocomplete = max(0, b.usage.Autocomplete-s.requests)
		delete(b.sessions, sessionID)
	}
	b.save()
}

// Usage: this month's billable counts.
func (b *Budget) Usage() (autocomplete, details int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover()
	return b.usage.Autocomplete, b.usage.Details
}

func (b *Budget) rollover() {
	if m := b.now().In(billing).Format("2006-01"); m != b.usage.Month {
		if b.usage.Month != "" {
			b.log.Info("place search: new billing month", "month", m,
				"autocomplete", b.usage.Autocomplete, "details", b.usage.Details)
		}
		b.usage = usage{Month: m}
		// Open sessions' requests were last month's: nothing to take back.
		b.sessions = map[string]*session{}
		b.save()
	}
}

func (b *Budget) untilNextMonth() time.Duration {
	t := b.now().In(billing)
	next := time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, billing)
	return next.Sub(t)
}

func (b *Budget) prune(now time.Time) {
	if now.Sub(b.pruned) < time.Minute {
		return
	}
	b.pruned = now
	for id, s := range b.sessions {
		if now.Sub(s.last) > sessionTTL {
			delete(b.sessions, id)
		}
	}
}

func (b *Budget) save() {
	b.publish()
	raw, _ := json.Marshal(b.usage)
	if err := os.MkdirAll(filepath.Dir(b.path), 0o755); err != nil {
		b.log.Warn("place search usage not saved", "err", err)
		return
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err == nil {
		err = os.Rename(tmp, b.path)
		if err != nil {
			b.log.Warn("place search usage not saved", "err", err)
		}
	}
}

func (b *Budget) publish() {
	metrics.PlacesBillable.WithLabelValues("autocomplete").Set(float64(b.usage.Autocomplete))
	metrics.PlacesBillable.WithLabelValues("details").Set(float64(b.usage.Details))
}
