// Package engine holds each source's live state: the static timetable, the
// latest realtime feeds, buses remembered after vanishing (ghosts) and the
// departures inferred from them, and answers queries on it.
package engine

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.marcorealacci.me/marcorealacci/tramite/internal/config"
	"git.marcorealacci.me/marcorealacci/tramite/internal/gtfs"
	"git.marcorealacci.me/marcorealacci/tramite/internal/live"
	"git.marcorealacci.me/marcorealacci/tramite/internal/metrics"
)

// TripDay is one run of a trip: the trip on a service date (yyyymmdd).
type TripDay struct {
	Trip int32
	Date int
}

// Ghost is a bus that vanished from the feeds near the end of its trip.
// Kept by ids (not indices) so it survives timetable reloads and restarts.
type Ghost struct {
	Vehicle   string    `json:"vehicle"`
	TripID    string    `json:"tripId"`
	RouteID   string    `json:"routeId"`
	Direction int8      `json:"direction"`
	EndStopID string    `json:"endStopId"`
	EndTime   time.Time `json:"endTime"`
	LastSeen  time.Time `json:"lastSeen"`
	Lat       float64   `json:"lat"`
	Lon       float64   `json:"lon"`
}

// Inference: a timetable-only departure a known bus should run, Delay after
// the timetable (0 = on time).
type Inference struct {
	Delay   time.Duration
	Vehicle string
	Ghost   bool
}

type state struct {
	feed *gtfs.Feed

	vehicles map[string]*live.Vehicle
	vehOf    map[string]TripDay
	vehTrip  map[TripDay]*live.Vehicle
	vehTS    time.Time

	tus   map[TripDay]*live.TripUpdate
	tuRaw []byte
	tuTS  time.Time

	ghosts   map[string]*Ghost
	inferred map[TripDay]Inference

	version uint64
}

// Source is one configured GTFS source and its live state.
type Source struct {
	Cfg config.Source
	// Now is the clock (tests replace it).
	Now func() time.Time
	// StateDir keeps state across restarts (ghosts); empty disables it.
	StateDir string

	mu sync.RWMutex
	st state
}

func NewSource(cfg config.Source, stateDir string) *Source {
	s := &Source{Cfg: cfg, Now: time.Now, StateDir: stateDir}
	s.st.ghosts = map[string]*Ghost{}
	s.st.inferred = map[TripDay]Inference{}
	s.loadGhosts()
	return s
}

// Ready: a timetable is loaded.
func (s *Source) Ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st.feed != nil
}

// Version changes whenever the state does.
func (s *Source) Version() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st.version
}

// SetStatic swaps in a new timetable; realtime data is re-resolved on it.
func (s *Source) SetStatic(f *gtfs.Feed) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.feed = f
	metrics.StaticTrips.WithLabelValues(s.Cfg.ID).Set(float64(len(f.Trips)))
	if s.st.vehicles != nil {
		vs := make([]live.Vehicle, 0, len(s.st.vehicles))
		for _, v := range s.st.vehicles {
			vs = append(vs, *v)
		}
		s.applyVehicles(s.st.vehTS, vs, false)
	}
	if s.st.tus != nil {
		tus := make([]live.TripUpdate, 0, len(s.st.tus))
		for _, u := range s.st.tus {
			tus = append(tus, *u)
		}
		s.applyTripUpdates(s.st.tuTS, tus, s.st.tuRaw)
	}
	s.recompute()
}

// ApplyVehicles replaces the vehicle positions.
func (s *Source) ApplyVehicles(ts time.Time, vs []live.Vehicle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyVehicles(ts, vs, true)
	s.recompute()
	metrics.Vehicles.WithLabelValues(s.Cfg.ID).Set(float64(len(vs)))
	s.saveGhosts()
}

// ApplyTripUpdates replaces the trip updates; raw is kept for the corrected feed.
func (s *Source) ApplyTripUpdates(ts time.Time, tus []live.TripUpdate, raw []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyTripUpdates(ts, tus, raw)
	s.recompute()
}

func (s *Source) applyTripUpdates(ts time.Time, tus []live.TripUpdate, raw []byte) {
	s.st.tuTS, s.st.tuRaw = ts, raw
	s.st.tus = make(map[TripDay]*live.TripUpdate, len(tus))
	if s.st.feed == nil {
		return
	}
	now := s.Now()
	for i := range tus {
		if td, ok := s.resolve(tus[i].Trip, now); ok {
			s.st.tus[td] = &tus[i]
		}
	}
}

func (s *Source) applyVehicles(ts time.Time, vs []live.Vehicle, trackGhosts bool) {
	now := s.Now()
	prev, prevOf := s.st.vehicles, s.st.vehOf
	// After an upstream outage the old snapshot says nothing about who
	// vanished just now.
	if !s.fresh(s.st.vehTS, now) {
		prev = nil
	}
	s.st.vehTS = ts
	s.st.vehicles = make(map[string]*live.Vehicle, len(vs))
	s.st.vehOf = make(map[string]TripDay, len(vs))
	s.st.vehTrip = make(map[TripDay]*live.Vehicle, len(vs))
	for i := range vs {
		v := &vs[i]
		s.st.vehicles[v.Key] = v
		if s.st.feed == nil {
			continue
		}
		if td, ok := s.resolve(v.Trip, now); ok {
			s.st.vehOf[v.Key] = td
			s.st.vehTrip[td] = v
		}
	}
	if !s.Cfg.Quirks.Ghosts.Enabled || s.st.feed == nil {
		return
	}
	// Back in the feed: no longer a ghost.
	for k := range s.st.ghosts {
		if _, ok := s.st.vehicles[k]; ok {
			delete(s.st.ghosts, k)
		}
	}
	if trackGhosts {
		for k, v := range prev {
			if _, still := s.st.vehicles[k]; still {
				continue
			}
			td, ok := prevOf[k]
			if !ok {
				continue
			}
			if left := s.stopsLeft(td.Trip, v.Seq); left < 0 || left > s.Cfg.Quirks.Ghosts.MaxStopsLeft {
				continue
			}
			f := s.st.feed
			tr := &f.Trips[td.Trip]
			last := f.LastStop(td.Trip)
			end := s.predictedEnd(td, now)
			seen := v.Timestamp
			if seen.IsZero() || seen.After(now) {
				seen = now
			}
			s.st.ghosts[k] = &Ghost{Vehicle: k, TripID: tr.ID, RouteID: f.Routes[tr.Route].ID, Direction: tr.Direction,
				EndStopID: f.Stops[last.Stop].ID, EndTime: end, LastSeen: seen, Lat: v.Lat, Lon: v.Lon}
		}
	}
	for k, g := range s.st.ghosts {
		if now.Sub(g.LastSeen) > s.Cfg.Quirks.Ghosts.Hold.Duration {
			delete(s.st.ghosts, k)
		}
	}
	metrics.Ghosts.WithLabelValues(s.Cfg.ID).Set(float64(len(s.st.ghosts)))
}

// stopsLeft after the stop with sequence seq (-1 if unknown).
func (s *Source) stopsLeft(trip int32, seq int32) int {
	if seq == 0 {
		return -1
	}
	sts := s.st.feed.TripStops(trip)
	for i := range sts {
		if sts[i].Seq == seq {
			return len(sts) - 1 - i
		}
	}
	return -1
}

func (s *Source) recompute() {
	s.st.version++
	if s.st.feed == nil || !s.Cfg.Quirks.Inference.Enabled {
		s.st.inferred = map[TripDay]Inference{}
		return
	}
	if !s.vehiclesFresh(s.Now()) {
		s.st.inferred = map[TripDay]Inference{}
	} else {
		s.st.inferred = s.infer(s.Now())
	}
	onTime, delayed := 0, 0
	for _, in := range s.st.inferred {
		if in.Delay > 0 {
			delayed++
		} else {
			onTime++
		}
	}
	metrics.Inferred.WithLabelValues(s.Cfg.ID, "on_time").Set(float64(onTime))
	metrics.Inferred.WithLabelValues(s.Cfg.ID, "delayed").Set(float64(delayed))
}

// Realtime data is used only while fresh: a feed whose timestamp is older
// than max_age (upstream frozen) counts as absent. Checked at query time,
// since a feed goes stale by not changing.
func (s *Source) fresh(ts, now time.Time) bool {
	return !ts.IsZero() && now.Sub(ts) <= s.Cfg.Realtime.MaxAge.Duration
}

func (s *Source) vehiclesFresh(now time.Time) bool { return s.fresh(s.st.vehTS, now) }
func (s *Source) tusFresh(now time.Time) bool      { return s.fresh(s.st.tuTS, now) }

func (s *Source) tusAt(now time.Time) map[TripDay]*live.TripUpdate {
	if s.tusFresh(now) {
		return s.st.tus
	}
	return nil
}

func (s *Source) vehTripAt(now time.Time) map[TripDay]*live.Vehicle {
	if s.vehiclesFresh(now) {
		return s.st.vehTrip
	}
	return nil
}

func (s *Source) vehOfAt(now time.Time) map[string]TripDay {
	if s.vehiclesFresh(now) {
		return s.st.vehOf
	}
	return nil
}

func (s *Source) vehiclesAt(now time.Time) map[string]*live.Vehicle {
	if s.vehiclesFresh(now) {
		return s.st.vehicles
	}
	return nil
}

// inferredAt: inferences need the vehicle positions they came from.
func (s *Source) inferredAt(now time.Time) map[TripDay]Inference {
	if s.vehiclesFresh(now) {
		return s.st.inferred
	}
	return nil
}

// resolve maps a realtime trip to a run of the timetable: by trip id, else
// by line, direction and start time (ids of another timetable version).
func (s *Source) resolve(r live.TripRef, now time.Time) (TripDay, bool) {
	f := s.st.feed
	t, ok := f.TripIdx[r.TripID]
	if !ok {
		route, okR := f.RouteIdx[r.RouteID]
		start, okS := parseHMS(r.StartTime)
		if !okR || !okS || r.Direction < 0 {
			return TripDay{}, false
		}
		if t, ok = f.RunIdx[gtfs.RunKey{Route: route, Direction: r.Direction, Start: start}]; !ok {
			return TripDay{}, false
		}
	}
	if f.Trips[t].First < 0 {
		return TripDay{}, false
	}
	if d, err := strconv.Atoi(r.StartDate); err == nil && d > 19000101 {
		return TripDay{t, d}, true
	}
	return TripDay{t, s.guessDate(t, now)}, true
}

// guessDate: the service date on which trip t runs around now.
func (s *Source) guessDate(t int32, now time.Time) int {
	f := s.st.feed
	for _, off := range []int{0, -1} {
		d := f.DateOf(now, off)
		if !f.Active(f.Trips[t].Service, d) {
			continue
		}
		ds := f.ServiceDayStart(d)
		start := ds.Add(time.Duration(f.StartDep(t)) * time.Second)
		end := ds.Add(time.Duration(f.LastStop(t).Arr) * time.Second)
		if now.After(start.Add(-3*time.Hour)) && now.Before(end.Add(3*time.Hour)) {
			return d
		}
	}
	return f.DateOf(now, 0)
}

// sched is the timetabled time of a stop time on a service date.
func (s *Source) sched(date int, secs int32) time.Time {
	return s.st.feed.ServiceDayStart(date).Add(time.Duration(secs) * time.Second)
}

// predictedEnd: when a run reaches its last stop, best estimate. The last
// realtime prediction, carried to the end with the timetabled minutes left.
// Stops the feed marks as skipped or as having no data don't count (Rome:
// the last stop of nearly every trip is "no data", with the timetable time
// in it).
func (s *Source) predictedEnd(td TripDay, now time.Time) time.Time {
	f := s.st.feed
	last := f.LastStop(td.Trip)
	end := s.sched(td.Date, last.Arr)
	var u *live.StopTimeUpdate
	if tu := s.tusAt(now)[td]; tu != nil {
		for i := len(tu.Updates) - 1; i >= 0 && u == nil; i-- {
			if x := &tu.Updates[i]; !x.NoData && !x.Skipped && !x.Time().IsZero() {
				u = x
			}
		}
	}
	if u == nil {
		if v := s.vehTripAt(now)[td]; v != nil && end.Before(now) {
			return now // running late, no prediction: about now
		}
		return end
	}
	t := u.Time()
	if u.Seq == last.Seq || (u.Seq == 0 && u.StopID == f.Stops[last.Stop].ID) {
		return t
	}
	for _, st := range f.TripStops(td.Trip) {
		if st.Seq == u.Seq {
			return t.Add(time.Duration(last.Arr-st.Arr) * time.Second)
		}
	}
	return end
}

func parseHMS(s string) (int32, bool) {
	p := strings.Split(s, ":")
	if len(p) != 3 {
		return 0, false
	}
	h, e1 := strconv.Atoi(p[0])
	m, e2 := strconv.Atoi(p[1])
	x, e3 := strconv.Atoi(p[2])
	if e1 != nil || e2 != nil || e3 != nil {
		return 0, false
	}
	return int32(h*3600 + m*60 + x), true
}

func distanceMeters(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371000.0
	p1, p2 := lat1*math.Pi/180, lat2*math.Pi/180
	dp, dl := p2-p1, (lon2-lon1)*math.Pi/180
	h := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * r * math.Asin(math.Sqrt(h))
}

func (s *Source) ghostFile() string {
	if s.StateDir == "" {
		return ""
	}
	return filepath.Join(s.StateDir, "ghosts.json")
}

func (s *Source) loadGhosts() {
	p := s.ghostFile()
	if p == "" {
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var gs []*Ghost
	if json.Unmarshal(b, &gs) == nil {
		for _, g := range gs {
			s.st.ghosts[g.Vehicle] = g
		}
	}
}

func (s *Source) saveGhosts() {
	p := s.ghostFile()
	if p == "" || !s.Cfg.Quirks.Ghosts.Enabled {
		return
	}
	gs := make([]*Ghost, 0, len(s.st.ghosts))
	for _, g := range s.st.ghosts {
		gs = append(gs, g)
	}
	b, _ := json.Marshal(gs)
	if os.MkdirAll(s.StateDir, 0o755) != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, p)
	}
}
