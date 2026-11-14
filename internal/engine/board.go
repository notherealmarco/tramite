package engine

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"git.marcorealacci.me/marcorealacci/tramite/internal/gtfs"
	"git.marcorealacci.me/marcorealacci/tramite/internal/live"
)

// Why a departure time is what it is. The first five are MOTIS's
// TimestampReason values; INFERRED is ours: no forecast, but a known bus
// should run it (see infer).
const (
	ReasonNoRT       = "NO_RT_DATA"
	ReasonPredicted  = "PREDICTED"
	ReasonPropagated = "PROPAGATED"
	ReasonInferred   = "INFERRED"
)

var ErrNotFound = errors.New("not found")

type Place struct {
	StopID string  `json:"stopId"`
	Name   string  `json:"name"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
}

type VehicleRef struct {
	ID string `json:"id"`
	// Started: the bus has left the first stop of this trip. Absent for an
	// inferred bus (it is still on its previous trip, or parked).
	Started             *bool  `json:"started,omitempty"`
	CurrentStopSequence int32  `json:"currentStopSequence,omitempty"`
	CurrentStatus       string `json:"currentStatus,omitempty"`
	// Where the bus is (live buses only).
	Lat       float64   `json:"lat,omitempty"`
	Lon       float64   `json:"lon,omitempty"`
	Bearing   float32   `json:"bearing,omitempty"`
	Timestamp time.Time `json:"timestamp,omitzero"`
	// How crowded the bus is now (GTFS-RT OccupancyStatus: EMPTY,
	// MANY_SEATS_AVAILABLE, FEW_SEATS_AVAILABLE, STANDING_ROOM_ONLY,
	// CRUSHED_STANDING_ROOM_ONLY, FULL, NOT_ACCEPTING_PASSENGERS), when the
	// feed reports it. Not given for an inferred bus: it is still on its
	// previous trip.
	OccupancyStatus     string  `json:"occupancyStatus,omitempty"`
	OccupancyPercentage *uint32 `json:"occupancyPercentage,omitempty"`
}

// StopTime is one departure from a stop, named after MOTIS's StopTime.
type StopTime struct {
	Source                   string      `json:"source"`
	TripID                   string      `json:"tripId"`
	GTFSTripID               string      `json:"gtfsTripId"`
	ServiceDate              string      `json:"serviceDate"`
	RouteID                  string      `json:"routeId"`
	RouteShortName           string      `json:"routeShortName"`
	RouteColor               string      `json:"routeColor,omitempty"`
	RouteTextColor           string      `json:"routeTextColor,omitempty"`
	RouteType                int         `json:"routeType"`
	DirectionID              *int8       `json:"directionId,omitempty"`
	Headsign                 string      `json:"headsign"`
	StopID                   string      `json:"stopId"`
	StopSequence             int32       `json:"stopSequence"`
	ScheduledArrival         time.Time   `json:"scheduledArrival"`
	Arrival                  time.Time   `json:"arrival"`
	ScheduledDeparture       time.Time   `json:"scheduledDeparture"`
	Departure                time.Time   `json:"departure"`
	RealTime                 bool        `json:"realTime"`
	DepartureTimestampReason string      `json:"departureTimestampReason"`
	Cancelled                bool        `json:"cancelled,omitempty"`
	TripFrom                 Place       `json:"tripFrom"`
	TripTo                   Place       `json:"tripTo"`
	Vehicle                  *VehicleRef `json:"vehicle,omitempty"`
}

type Board struct {
	Stop      Place      `json:"stop"`
	StopTimes []StopTime `json:"stopTimes"`
}

const lookback = 90 * time.Minute // late buses still to come

// Prefix of this source's ids in the API: its Transitous feed id when
// configured (ids then match Transitous), else its own id.
func (s *Source) Prefix() string {
	if s.Cfg.TransitousFeedID != "" {
		return s.Cfg.TransitousFeedID
	}
	return s.Cfg.ID
}

// localID strips the prefix from a stop or route id ("it-Lazio-Rome_71527").
func (s *Source) localID(id string) string { return strings.TrimPrefix(id, s.Prefix()+"_") }

// Owns tells whether an API id (stop, route or trip) belongs to this source.
func (s *Source) Owns(id string) bool {
	p := s.Prefix() + "_"
	if strings.HasPrefix(id, p) {
		return true
	}
	// Trip ids: <date>_<HH:MM>_<prefix>_<trip>.
	if date, rest, ok := strings.Cut(id, "_"); ok && len(date) == 8 {
		if _, rest, ok := strings.Cut(rest, "_"); ok {
			return strings.HasPrefix(rest, p)
		}
	}
	return false
}

// resolveTripID reads an API trip id: <date>_<HH:MM>_<prefix>_<trip>
// (Transitous form), or a plain GTFS trip id (run around now).
func (s *Source) resolveTripID(id string, now time.Time) (TripDay, bool) {
	f := s.st.feed
	if date, rest, ok := strings.Cut(id, "_"); ok && len(date) == 8 {
		if _, rest, ok := strings.Cut(rest, "_"); ok {
			if raw, ok := strings.CutPrefix(rest, s.Prefix()+"_"); ok {
				if t, ok := f.TripIdx[raw]; ok && f.Trips[t].First >= 0 {
					if d, err := strconv.Atoi(date); err == nil {
						return TripDay{t, d}, true
					}
				}
			}
		}
	}
	t, ok := f.TripIdx[strings.TrimPrefix(id, s.Prefix()+"_")]
	if !ok || f.Trips[t].First < 0 {
		return TripDay{}, false
	}
	return TripDay{t, s.guessDate(t, now)}, true
}

func (s *Source) place(stop int32) Place {
	st := &s.st.feed.Stops[stop]
	return Place{StopID: s.Prefix() + "_" + st.ID, Name: st.Name, Lat: st.Lat, Lon: st.Lon}
}

// tripID in Transitous (MOTIS) form: <service date>_<HH:MM start>_<prefix>_<trip id>.
func (s *Source) tripID(td TripDay) string {
	f := s.st.feed
	dep := f.StartDep(td.Trip)
	return fmt.Sprintf("%08d_%02d:%02d_%s_%s", td.Date, dep/3600, dep%3600/60, s.Prefix(), f.Trips[td.Trip].ID)
}

// Departures from a stop within window, soonest first, at most n.
func (s *Source) Departures(stopID string, n int, window time.Duration) (*Board, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f := s.st.feed
	if f == nil {
		return nil, fmt.Errorf("%s: timetable not loaded yet", s.Cfg.ID)
	}
	stop, ok := f.StopIdx[s.localID(stopID)]
	if !ok {
		return nil, ErrNotFound
	}
	now := s.Now()
	out := []StopTime{}
	for _, off := range []int{-1, 0} {
		date := f.DateOf(now, off)
		ds := f.ServiceDayStart(date)
		lo := int32(now.Add(-lookback).Sub(ds) / time.Second)
		hi := int32(now.Add(window).Sub(ds) / time.Second)
		idx := f.ByStop[stop]
		i := sort.Search(len(idx), func(i int) bool { return f.StopTimes[idx[i]].Dep >= lo })
		for ; i < len(idx) && f.StopTimes[idx[i]].Dep <= hi; i++ {
			st := f.StopTimes[idx[i]]
			tr := &f.Trips[st.Trip]
			if !f.Active(tr.Service, date) || st.Pickup == 1 {
				continue
			}
			if last := f.LastStop(st.Trip); last.Seq == st.Seq {
				continue // arrival at the end of the trip: nothing leaves
			}
			if d, ok := s.departure(TripDay{st.Trip, date}, st, now, window); ok {
				out = append(out, d)
			}
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Departure.Before(out[b].Departure) })
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return &Board{Stop: s.place(stop), StopTimes: out}, nil
}

func (s *Source) departure(td TripDay, st gtfs.StopTime, now time.Time, window time.Duration) (StopTime, bool) {
	d, visible := s.row(td, st, now)
	return d, visible && d.Departure.Before(now.Add(window))
}

// row is a trip's stop time at one stop with everything known about it, and
// whether a board should list it (not cancelled long ago, bus not past it,
// not gone).
func (s *Source) row(td TripDay, st gtfs.StopTime, now time.Time) (StopTime, bool) {
	f := s.st.feed
	tr := &f.Trips[td.Trip]
	route := &f.Routes[tr.Route]
	sched := s.sched(td.Date, st.Dep)
	schedArr := s.sched(td.Date, st.Arr)
	d := StopTime{
		Source: s.Cfg.ID, TripID: s.tripID(td), GTFSTripID: tr.ID, ServiceDate: fmt.Sprintf("%08d", td.Date),
		RouteID: s.Prefix() + "_" + route.ID, RouteShortName: route.Line(), RouteColor: route.Color, RouteTextColor: route.TextColor,
		RouteType: route.Type, Headsign: tr.Headsign, StopSequence: st.Seq, StopID: s.place(st.Stop).StopID,
		ScheduledDeparture: sched.UTC(), Departure: sched.UTC(), DepartureTimestampReason: ReasonNoRT,
		ScheduledArrival: schedArr.UTC(), Arrival: schedArr.UTC(),
		TripFrom: s.place(f.StopTimes[tr.First].Stop), TripTo: s.place(f.LastStop(td.Trip).Stop),
	}
	if tr.Direction >= 0 {
		dir := tr.Direction
		d.DirectionID = &dir
	}
	visible := true
	first := f.StopTimes[tr.First].Seq == st.Seq
	v := s.vehTripAt(now)[td]
	if v != nil {
		started := v.Seq == 0 || v.Seq > f.StopTimes[tr.First].Seq
		d.Vehicle = &VehicleRef{ID: v.Key, Started: &started, CurrentStopSequence: v.Seq, CurrentStatus: v.Status,
			OccupancyStatus: v.Occupancy, OccupancyPercentage: v.OccupancyPercentage}
		if v.HasPos {
			d.Vehicle.Lat, d.Vehicle.Lon, d.Vehicle.Bearing = v.Lat, v.Lon, v.Bearing
			d.Vehicle.Timestamp = v.Timestamp.UTC()
		}
		if v.Seq > st.Seq {
			visible = false // already past this stop
		}
	}
	waiting := v != nil && !*d.Vehicle.Started && first

	if tu := s.tusAt(now)[td]; tu != nil {
		if tu.Trip.Canceled {
			d.Cancelled = true
			return d, visible && sched.After(now.Add(-time.Minute))
		}
		t, reason, passed, skipped := s.realtimeAt(td, tu, st, sched)
		if passed && !waiting {
			visible = false
		}
		if skipped {
			d.Cancelled = true
		}
		if reason != "" {
			d.Departure, d.RealTime, d.DepartureTimestampReason = t.UTC(), true, reason
		}
	}
	if !d.RealTime {
		if in, ok := s.inferredAt(now)[td]; ok {
			t := sched.Add(in.Delay)
			if t.Sub(now) <= s.Cfg.Quirks.Inference.MaxWait.Duration {
				d.Departure, d.DepartureTimestampReason = t.UTC(), ReasonInferred
				d.Vehicle = &VehicleRef{ID: in.Vehicle}
			}
		}
	}
	// The arrival moves with the departure (same delay).
	d.Arrival = schedArr.Add(d.Departure.Sub(sched)).UTC()
	// Gone, unless its bus is still waiting at the first stop (the operator
	// keeps its forecast at "now" until it leaves).
	if d.Departure.Before(now.Add(-30*time.Second)) && !waiting {
		visible = false
	}
	return d, visible
}

// TripAt is one trip's row at one stop, whether or not a board would list
// it (bus already past, departure gone): for following a ride.
func (s *Source) TripAt(stopID, tripID string) (*Board, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f := s.st.feed
	if f == nil {
		return nil, fmt.Errorf("%s: timetable not loaded yet", s.Cfg.ID)
	}
	stop, ok := f.StopIdx[s.localID(stopID)]
	if !ok {
		return nil, ErrNotFound
	}
	now := s.Now()
	td, ok := s.resolveTripID(tripID, now)
	if !ok {
		return nil, ErrNotFound
	}
	out := []StopTime{}
	for _, st := range f.TripStops(td.Trip) {
		if st.Stop == stop {
			d, _ := s.row(td, st, now)
			out = append(out, d)
			break
		}
	}
	return &Board{Stop: s.place(stop), StopTimes: out}, nil
}

// realtimeAt: the trip update's time at this stop. A prediction for the stop
// itself, else the delay of the last prediction before it carried forward.
// passed: the trip has predictions only for later stops (gone by).
func (s *Source) realtimeAt(td TripDay, tu *live.TripUpdate, st gtfs.StopTime, sched time.Time) (t time.Time, reason string, passed, skipped bool) {
	f := s.st.feed
	stopID := f.Stops[st.Stop].ID
	var prev *live.StopTimeUpdate
	for i := range tu.Updates {
		u := &tu.Updates[i]
		if (u.Seq != 0 && u.Seq == st.Seq) || (u.Seq == 0 && u.StopID == stopID) {
			if u.Skipped {
				return t, "", false, true
			}
			if u.NoData {
				return t, "", false, false
			}
			if x := u.Time(); !x.IsZero() {
				return x, ReasonPredicted, false, false
			}
			if u.Delay != nil {
				return sched.Add(time.Duration(*u.Delay) * time.Second), ReasonPredicted, false, false
			}
			return t, "", false, false
		}
		if u.Seq != 0 && u.Seq < st.Seq && !u.NoData && !u.Skipped {
			prev = u
		}
	}
	if prev != nil {
		for _, x := range f.TripStops(td.Trip) {
			if x.Seq != prev.Seq {
				continue
			}
			var delay time.Duration
			switch {
			case !prev.Dep.IsZero():
				delay = prev.Dep.Sub(s.sched(td.Date, x.Dep))
			case !prev.Arr.IsZero():
				delay = prev.Arr.Sub(s.sched(td.Date, x.Arr))
			case prev.Delay != nil:
				delay = time.Duration(*prev.Delay) * time.Second
			}
			return sched.Add(delay), ReasonPropagated, false, false
		}
	}
	if len(tu.Updates) > 0 && tu.Updates[0].Seq > st.Seq {
		return t, "", true, false
	}
	if tu.Delay != nil {
		return sched.Add(time.Duration(*tu.Delay) * time.Second), ReasonPropagated, false, false
	}
	return t, "", false, false
}
