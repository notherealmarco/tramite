package engine

import (
	"errors"
	"fmt"
	"sort"
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
	StopSequence             int32       `json:"stopSequence"`
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

// StopID accepts the source's own id or the Transitous-prefixed one.
func (s *Source) localStopID(id string) string {
	if p := s.Cfg.TransitousFeedID; p != "" {
		return strings.TrimPrefix(id, p+"_")
	}
	return id
}

func (s *Source) place(stop int32) Place {
	st := &s.st.feed.Stops[stop]
	id := st.ID
	if p := s.Cfg.TransitousFeedID; p != "" {
		id = p + "_" + id
	}
	return Place{StopID: id, Name: st.Name, Lat: st.Lat, Lon: st.Lon}
}

// tripID in MOTIS form when the Transitous feed id is configured:
// <service date>_<HH:MM start>_<feed>_<trip id>.
func (s *Source) tripID(td TripDay) string {
	f := s.st.feed
	id := f.Trips[td.Trip].ID
	p := s.Cfg.TransitousFeedID
	if p == "" {
		return id
	}
	dep := f.StartDep(td.Trip)
	return fmt.Sprintf("%08d_%02d:%02d_%s_%s", td.Date, dep/3600, dep%3600/60, p, id)
}

// Departures from a stop within window, soonest first, at most n.
func (s *Source) Departures(stopID string, n int, window time.Duration) (*Board, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f := s.st.feed
	if f == nil {
		return nil, fmt.Errorf("%s: timetable not loaded yet", s.Cfg.ID)
	}
	stop, ok := f.StopIdx[s.localStopID(stopID)]
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
	f := s.st.feed
	tr := &f.Trips[td.Trip]
	route := &f.Routes[tr.Route]
	sched := s.sched(td.Date, st.Dep)
	d := StopTime{
		Source: s.Cfg.ID, TripID: s.tripID(td), GTFSTripID: tr.ID, ServiceDate: fmt.Sprintf("%08d", td.Date),
		RouteID: route.ID, RouteShortName: route.Line(), RouteColor: route.Color, RouteTextColor: route.TextColor,
		RouteType: route.Type, Headsign: tr.Headsign, StopSequence: st.Seq,
		ScheduledDeparture: sched.UTC(), Departure: sched.UTC(), DepartureTimestampReason: ReasonNoRT,
		TripFrom: s.place(f.StopTimes[tr.First].Stop), TripTo: s.place(f.LastStop(td.Trip).Stop),
	}
	if tr.Direction >= 0 {
		dir := tr.Direction
		d.DirectionID = &dir
	}
	first := f.StopTimes[tr.First].Seq == st.Seq
	v := s.st.vehTrip[td]
	if v != nil {
		started := v.Seq == 0 || v.Seq > f.StopTimes[tr.First].Seq
		d.Vehicle = &VehicleRef{ID: v.Key, Started: &started, CurrentStopSequence: v.Seq, CurrentStatus: v.Status}
		if v.Seq > st.Seq {
			return d, false // already past this stop
		}
	}
	waiting := v != nil && !*d.Vehicle.Started && first

	if tu := s.st.tus[td]; tu != nil {
		if tu.Trip.Canceled {
			d.Cancelled = true
			return d, sched.After(now.Add(-time.Minute))
		}
		t, reason, passed, skipped := s.realtimeAt(td, tu, st, sched)
		if passed && !waiting {
			return d, false
		}
		if skipped {
			d.Cancelled = true
		}
		if reason != "" {
			d.Departure, d.RealTime, d.DepartureTimestampReason = t.UTC(), true, reason
		}
	}
	if !d.RealTime {
		if in, ok := s.st.inferred[td]; ok {
			t := sched.Add(in.Delay)
			if t.Sub(now) <= s.Cfg.Quirks.Inference.MaxWait.Duration {
				d.Departure, d.DepartureTimestampReason = t.UTC(), ReasonInferred
				d.Vehicle = &VehicleRef{ID: in.Vehicle}
			}
		}
	}
	// Gone, unless its bus is still waiting at the first stop (the operator
	// keeps its forecast at "now" until it leaves).
	if d.Departure.Before(now.Add(-30*time.Second)) && !waiting {
		return d, false
	}
	return d, d.Departure.Before(now.Add(window))
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
