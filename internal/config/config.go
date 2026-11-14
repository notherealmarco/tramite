// Package config reads the service configuration: where to listen, where to
// keep data, and the list of GTFS sources with the processing each one gets.
package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	// Listen address, e.g. ":8080".
	Listen string `yaml:"listen"`
	// DataDir holds the raw feed archive, static GTFS copies and state that
	// must survive a restart.
	DataDir string   `yaml:"data_dir"`
	Sources []Source `yaml:"sources"`
}

// Source is one GTFS dataset (an operator or a region) with its realtime feeds.
type Source struct {
	// ID prefixes everything from this source in the API, e.g. "rome".
	ID   string `yaml:"id"`
	Name string `yaml:"name"`
	// TransitousFeedID is the source's feed id in Transitous/MOTIS (e.g.
	// "it-Lazio-Rome"); ids in responses are then MOTIS-compatible.
	TransitousFeedID string `yaml:"transitous_feed_id"`
	// Timezone overrides the GTFS agency timezone.
	Timezone string `yaml:"timezone"`

	Static   StaticFeed   `yaml:"static"`
	Realtime RealtimeFeed `yaml:"realtime"`
	// Headers sent with every request to this source (API keys, client ids).
	Headers map[string]string `yaml:"headers"`
	// Archive stores every fetched feed under data_dir/raw (for analysis).
	Archive bool `yaml:"archive"`

	Quirks Quirks `yaml:"quirks"`
}

type StaticFeed struct {
	URL string `yaml:"url"`
	// How often to check for a new timetable (conditional request).
	CheckEvery Duration `yaml:"check_every"`
}

type RealtimeFeed struct {
	TripUpdates      string `yaml:"trip_updates"`
	VehiclePositions string `yaml:"vehicle_positions"`
	Alerts           string `yaml:"alerts"`
	// MinPoll is the shortest time between two requests when the upstream
	// can't answer "not modified" (every check downloads the whole feed).
	MinPoll Duration `yaml:"min_poll"`
	// FastPoll is the check interval around an expected update when the
	// upstream supports conditional requests.
	FastPoll Duration `yaml:"fast_poll"`
	// MaxAge: a feed whose own timestamp is older than this is stale and
	// ignored (buses not shown, departures back on the timetable).
	MaxAge Duration `yaml:"max_age"`
}

// Quirks switches on processing a source needs. Everything is off by default:
// a source without quirks is served as it comes.
type Quirks struct {
	Ghosts    GhostQuirk     `yaml:"ghost_memory"`
	Inference InferenceQuirk `yaml:"inference"`
}

// GhostQuirk: buses that vanish from the feeds near the end of their trip
// and reappear when the driver sets the next trip.
type GhostQuirk struct {
	Enabled bool `yaml:"enabled"`
	// A bus counts as a ghost if it vanished with at most this many stops
	// left on its trip.
	MaxStopsLeft int `yaml:"max_stops_left"`
	// How long a vanished bus is remembered.
	Hold Duration `yaml:"hold"`
}

// InferenceQuirk: timetable-only departures from a terminus that a known bus
// (coming back from the other direction, or remembered there) should run.
type InferenceQuirk struct {
	Enabled bool `yaml:"enabled"`
	// Least time at the terminus between arriving and leaving again.
	Turnaround Duration `yaml:"turnaround"`
	// A bus whose trip ends within this distance of the terminus counts.
	MaxEndDistanceMeters float64 `yaml:"max_end_distance_m"`
	// A bus that could only leave this much after the timetable is more
	// likely to run a later departure.
	MaxDelay Duration `yaml:"max_delay"`
	// Only departures within this time of their terminus start are looked at.
	Horizon Duration `yaml:"horizon"`
	// Inferred times further away than this at a stop are shown as scheduled.
	MaxWait Duration `yaml:"max_wait"`
	// Keep only one inferred departure per line and terminus: the first on
	// time one, else the first delayed one.
	FirstOnly bool `yaml:"first_only"`
}

// Duration reads "90s", "15m" etc.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Load reads and validates a config file, filling in defaults.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

func Parse(b []byte) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	c.applyDefaults()
	return &c, c.validate()
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.DataDir == "" {
		c.DataDir = "data"
	}
	for i := range c.Sources {
		s := &c.Sources[i]
		def := func(d *Duration, v time.Duration) {
			if d.Duration == 0 {
				d.Duration = v
			}
		}
		def(&s.Static.CheckEvery, 6*time.Hour)
		def(&s.Realtime.MinPoll, 15*time.Second)
		def(&s.Realtime.FastPoll, 2*time.Second)
		def(&s.Realtime.MaxAge, 10*time.Minute)
		def(&s.Quirks.Ghosts.Hold, 15*time.Minute)
		if s.Quirks.Ghosts.MaxStopsLeft == 0 {
			s.Quirks.Ghosts.MaxStopsLeft = 2
		}
		q := &s.Quirks.Inference
		def(&q.Turnaround, 3*time.Minute)
		def(&q.MaxDelay, time.Hour)
		def(&q.Horizon, time.Hour)
		def(&q.MaxWait, 30*time.Minute)
		if q.MaxEndDistanceMeters == 0 {
			q.MaxEndDistanceMeters = 300
		}
	}
}

func (c *Config) validate() error {
	seen := map[string]bool{}
	for _, s := range c.Sources {
		if !idRe.MatchString(s.ID) {
			return fmt.Errorf("source id %q: use lowercase letters, digits and dashes", s.ID)
		}
		if seen[s.ID] {
			return fmt.Errorf("source id %q used twice", s.ID)
		}
		seen[s.ID] = true
		if s.Static.URL == "" {
			return fmt.Errorf("source %s: static.url is required", s.ID)
		}
	}
	return nil
}
