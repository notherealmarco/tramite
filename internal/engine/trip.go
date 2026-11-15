package engine

import (
	"fmt"
	"time"
)

// TripStop is one stop of a trip, with its times there. Field names as in
// StopTime.
type TripStop struct {
	Place
	StopSequence             int32     `json:"stopSequence"`
	ScheduledArrival         time.Time `json:"scheduledArrival"`
	Arrival                  time.Time `json:"arrival"`
	ScheduledDeparture       time.Time `json:"scheduledDeparture"`
	Departure                time.Time `json:"departure"`
	RealTime                 bool      `json:"realTime"`
	DepartureTimestampReason string    `json:"departureTimestampReason"`
	Cancelled                bool      `json:"cancelled,omitempty"`
}

// TripView is a whole trip: what it is, its bus, and every stop in order
// with its times. For following a ride from where you board to where you
// get off, or the bus you are on.
type TripView struct {
	Source         string      `json:"source"`
	TripID         string      `json:"tripId"`
	GTFSTripID     string      `json:"gtfsTripId"`
	ServiceDate    string      `json:"serviceDate"`
	RouteID        string      `json:"routeId"`
	RouteShortName string      `json:"routeShortName"`
	RouteColor     string      `json:"routeColor,omitempty"`
	RouteTextColor string      `json:"routeTextColor,omitempty"`
	RouteType      int         `json:"routeType"`
	DirectionID    *int8       `json:"directionId,omitempty"`
	Headsign       string      `json:"headsign"`
	Cancelled      bool        `json:"cancelled,omitempty"`
	TripFrom       Place       `json:"tripFrom"`
	TripTo         Place       `json:"tripTo"`
	Vehicle        *VehicleRef `json:"vehicle,omitempty"`
	StopTimes      []TripStop  `json:"stopTimes"`
}

// Trip is a whole trip with its times at every stop, whatever its state
// (stops the bus has passed are listed too, on the timetable).
//
// A trip id from another version of the timetable is matched by run as in
// TripAt, which needs a stop the run calls at: [stopID] (optional; without
// it such an id is not found) and [line] when two lines' runs would match.
//
// An inferred trip (a known bus should run it, see infer) is inferred at
// every stop, with the same delay, however far away the stop is in time.
func (s *Source) Trip(tripID, line, stopID string) (*TripView, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f := s.st.feed
	if f == nil {
		return nil, fmt.Errorf("%s: timetable not loaded yet", s.Cfg.ID)
	}
	now := s.Now()
	td, ok := s.resolveTripID(tripID, now)
	if !ok && stopID != "" {
		if stop, known := f.StopIdx[s.localID(stopID)]; known {
			td, ok = s.resolveRun(tripID, stop, line)
		}
	}
	if !ok {
		return nil, ErrNotFound
	}
	var out *TripView
	var inferred *StopTime // the first stop's row, when the trip is inferred
	for i, st := range f.TripStops(td.Trip) {
		d, _ := s.row(td, st, now)
		if i == 0 {
			out = &TripView{
				Source: d.Source, TripID: d.TripID, GTFSTripID: d.GTFSTripID, ServiceDate: d.ServiceDate,
				RouteID: d.RouteID, RouteShortName: d.RouteShortName, RouteColor: d.RouteColor, RouteTextColor: d.RouteTextColor,
				RouteType: d.RouteType, DirectionID: d.DirectionID, Headsign: d.Headsign,
				TripFrom: d.TripFrom, TripTo: d.TripTo, Vehicle: d.Vehicle, StopTimes: []TripStop{},
			}
			if d.DepartureTimestampReason == ReasonInferred {
				first := d
				inferred = &first
			}
		}
		if inferred != nil && d.DepartureTimestampReason == ReasonNoRT {
			delay := inferred.Departure.Sub(inferred.ScheduledDeparture)
			d.Departure, d.Arrival = d.ScheduledDeparture.Add(delay), d.ScheduledArrival.Add(delay)
			d.DepartureTimestampReason = ReasonInferred
		}
		// Cancelled as a whole (every row then is), not one skipped stop.
		if i == 0 {
			if tu := s.tusAt(now)[td]; tu != nil && tu.Trip.Canceled {
				out.Cancelled = true
			}
		}
		out.StopTimes = append(out.StopTimes, TripStop{
			Place: s.place(st.Stop), StopSequence: d.StopSequence,
			ScheduledArrival: d.ScheduledArrival, Arrival: d.Arrival,
			ScheduledDeparture: d.ScheduledDeparture, Departure: d.Departure,
			RealTime: d.RealTime, DepartureTimestampReason: d.DepartureTimestampReason, Cancelled: d.Cancelled,
		})
	}
	if out == nil {
		return nil, ErrNotFound
	}
	return out, nil
}
