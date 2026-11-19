package config

import (
	"testing"
	"time"
)

func TestParseDefaultsAndValidation(t *testing.T) {
	c, err := Parse([]byte(`
sources:
  - id: rome
    static: {url: https://example.org/gtfs.zip}
    quirks:
      ghost_memory: {enabled: true, hold: 20m}
`))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Sources[0]
	if c.Listen != ":8080" || s.Quirks.Ghosts.Hold.Duration != 20*time.Minute || s.Quirks.Ghosts.MaxStopsLeft != 2 ||
		s.Quirks.Inference.MaxWait.Duration != 30*time.Minute || s.Realtime.FastPoll.Duration != 2*time.Second ||
		s.Quirks.DayOff.Enabled || s.Quirks.DayOff.MaxDelay.Duration != 3*time.Hour {
		t.Fatalf("defaults: %+v", c)
	}
	for _, bad := range []string{
		`sources: [{id: Rome, static: {url: x}}]`,
		`sources: [{id: a, static: {url: x}}, {id: a, static: {url: y}}]`,
		`sources: [{id: a}]`,
		`sources: [{id: a, static: {url: x, check_every: soon}}]`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
}

func TestPlacesConfig(t *testing.T) {
	c, err := Parse([]byte(`
places:
  provider: google
  bias: {lat: 41.9, lon: 12.5}
  per_ip: {per_minute: 30, per_day: 500}
`))
	if err != nil {
		t.Fatal(err)
	}
	p := c.Places
	if p.APIKeyEnv != "TRAMITE_PLACES_API_KEY" || p.RadiusMeters != 30000 || p.Bias.RadiusMeters != 30000 ||
		p.MonthlyLimit.Autocomplete != 9500 || p.MonthlyLimit.Details != 9500 || p.PerIP.PerDay != 500 {
		t.Fatalf("defaults: %+v", p)
	}
	for _, bad := range []string{
		`places: {provider: mapbox}`,
		`places: {provider: google, radius_m: 60000}`,
		`places: {provider: google, per_ip: {per_day: -1}}`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
}
