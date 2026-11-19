// Package places serves place search for the app: suggestions while typing
// and the position of the one picked, from a provider (Google Places for
// now), kept within the provider's free monthly allowance and per-client
// limits. Clients see only Tramite's ids ("google:<id>") and an attribution
// line to show with the results.
package places

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"git.marcorealacci.me/marcorealacci/tramite/internal/config"
	"git.marcorealacci.me/marcorealacci/tramite/internal/metrics"
)

// Suggestion is one result while typing. It has no position: that costs a
// second request, made only for the one picked ([Service.Locate]).
type Suggestion struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Subtitle string `json:"subtitle,omitempty"`
	// Kind: "address" or "place".
	Kind string `json:"kind"`
	// DistanceMeters from the position the client sent, when known.
	DistanceMeters *int `json:"distanceMeters,omitempty"`
}

type Location struct {
	ID  string  `json:"id"`
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

type LatLon struct{ Lat, Lon float64 }

type Query struct {
	Text string
	// Lang: the client's language ("it", "en"), may be empty.
	Lang string
	// Near: where the client is (or looks on the map), may be nil.
	Near *LatLon
	// Session groups the requests of one search, from the first letter to
	// the picked result (the provider bills them together).
	Session string
}

// Provider is a place search service. Ids are the provider's own.
type Provider interface {
	Autocomplete(ctx context.Context, q Query) ([]Suggestion, error)
	Locate(ctx context.Context, id, session, lang string) (Location, error)
}

var (
	// ErrNotFound: no such place (or an id of another provider).
	ErrNotFound = errors.New("place not found")
	// ErrUpstreamLimit: the provider refused for quota or rate (its own
	// limits, e.g. the daily caps set in the Google Cloud console).
	ErrUpstreamLimit = errors.New("provider limit reached")
)

// LimitError: not now; ask again after RetryAfter.
type LimitError struct {
	// Reason: "quota" (monthly allowance used up, or the provider's own
	// limit) or "rate_limit" (this client asked too often).
	Reason     string
	RetryAfter time.Duration
}

func (e *LimitError) Error() string { return fmt.Sprintf("%s, retry after %s", e.Reason, e.RetryAfter) }

// Service is place search with its limits. Nil when place search is off.
type Service struct {
	// Name prefixes the ids clients see: "google".
	Name string
	// Attribution is shown with the results, as the provider requires.
	Attribution string

	provider Provider
	budget   *Budget
	limiter  *Limiter
	log      *slog.Logger
}

// upstreamLimitRetry: how long clients wait after the provider itself said
// no. Its per-minute limits lift soon; a daily cap is asked again each time.
const upstreamLimitRetry = time.Minute

// New sets up place search from cfg, or returns nil when it is off (no
// provider, or no key in the environment variable cfg names).
func New(cfg config.Places, dataDir string, getenv func(string) string, log *slog.Logger) *Service {
	if cfg.Provider == "" {
		return nil
	}
	key := getenv(cfg.APIKeyEnv)
	if key == "" {
		log.Warn("place search off: no API key", "provider", cfg.Provider, "env", cfg.APIKeyEnv)
		return nil
	}
	var p Provider
	var attribution string
	switch cfg.Provider {
	case "google":
		g := &Google{Key: key, Region: cfg.Region, RadiusMeters: cfg.RadiusMeters}
		if b := cfg.Bias; b != nil {
			g.Bias = &config.Circle{Lat: b.Lat, Lon: b.Lon, RadiusMeters: b.RadiusMeters}
		}
		p, attribution = g, "Google Maps"
	default:
		return nil // rejected by config validation
	}
	return NewService(cfg.Provider, attribution, p,
		NewBudget(filepath.Join(dataDir, "places", "usage.json"), cfg.MonthlyLimit, time.Now, log),
		NewLimiter(cfg.PerIP.PerMinute, cfg.PerIP.PerDay, time.Now), log)
}

func NewService(name, attribution string, p Provider, b *Budget, l *Limiter, log *slog.Logger) *Service {
	return &Service{Name: name, Attribution: attribution, provider: p, budget: b, limiter: l, log: log}
}

// Admit checks client's per-IP limits for one more request.
func (s *Service) Admit(client string) error {
	if wait, ok := s.limiter.Allow(client); !ok {
		metrics.PlacesRequests.WithLabelValues("any", "rate_limit").Inc()
		return &LimitError{Reason: "rate_limit", RetryAfter: wait}
	}
	return nil
}

// Autocomplete: suggestions for q.Text, ids prefixed with the provider name.
func (s *Service) Autocomplete(ctx context.Context, q Query) ([]Suggestion, error) {
	if wait, ok := s.budget.AllowSearch(); !ok {
		metrics.PlacesRequests.WithLabelValues("autocomplete", "quota").Inc()
		return nil, &LimitError{Reason: "quota", RetryAfter: wait}
	}
	s.budget.RecordAutocomplete(q.Session)
	out, err := s.provider.Autocomplete(ctx, q)
	if err != nil {
		return nil, s.fail("autocomplete", err)
	}
	for i := range out {
		out[i].ID = s.Name + ":" + out[i].ID
	}
	metrics.PlacesRequests.WithLabelValues("autocomplete", "ok").Inc()
	return out, nil
}

// Locate: the position of place id (as given by [Service.Autocomplete]);
// with the search's session it ends that session.
func (s *Service) Locate(ctx context.Context, id, session, lang string) (Location, error) {
	raw, ok := strings.CutPrefix(id, s.Name+":")
	if !ok || raw == "" {
		return Location{}, ErrNotFound
	}
	if wait, ok := s.budget.AllowDetails(); !ok {
		metrics.PlacesRequests.WithLabelValues("details", "quota").Inc()
		return Location{}, &LimitError{Reason: "quota", RetryAfter: wait}
	}
	s.budget.RecordDetails(session)
	loc, err := s.provider.Locate(ctx, raw, session, lang)
	if err != nil {
		return Location{}, s.fail("details", err)
	}
	loc.ID = id
	metrics.PlacesRequests.WithLabelValues("details", "ok").Inc()
	return loc, nil
}

func (s *Service) fail(endpoint string, err error) error {
	switch {
	case errors.Is(err, ErrUpstreamLimit):
		metrics.PlacesRequests.WithLabelValues(endpoint, "upstream_limit").Inc()
		s.log.Warn("place search: provider limit", "endpoint", endpoint, "err", err)
		return &LimitError{Reason: "quota", RetryAfter: upstreamLimitRetry}
	case errors.Is(err, ErrNotFound):
		metrics.PlacesRequests.WithLabelValues(endpoint, "not_found").Inc()
		return err
	default:
		metrics.PlacesRequests.WithLabelValues(endpoint, "error").Inc()
		s.log.Warn("place search failed", "endpoint", endpoint, "err", err)
		return err
	}
}
