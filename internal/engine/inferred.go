package engine

import (
	"fmt"
	"sort"
	"time"
)

// InferredDeparture is one departure a known bus should run (see infer), for
// clients that combine it with routes planned elsewhere.
type InferredDeparture struct {
	TripID      string `json:"tripId"`
	GTFSTripID  string `json:"gtfsTripId"`
	ServiceDate string `json:"serviceDate"`
	// Departure from the first stop, and the delay after the timetable at
	// every stop (0 = on time).
	ScheduledDeparture time.Time `json:"scheduledDeparture"`
	Departure          time.Time `json:"departure"`
	DelaySeconds       int       `json:"delaySeconds"`
	Vehicle            string    `json:"vehicle"`
	// Ghost: the bus vanished from the feeds near the terminus.
	Ghost bool `json:"ghost,omitempty"`
}

type Inferred struct {
	// Show an inferred time only when it is at most this far away at the
	// stop where it is shown; beyond, the timetable is as good.
	MaxWaitSeconds int                 `json:"maxWaitSeconds"`
	Departures     []InferredDeparture `json:"departures"`
}

// InferredDepartures lists the current inferences (empty while the vehicle
// feed is stale).
func (s *Source) InferredDepartures() Inferred {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := Inferred{MaxWaitSeconds: int(s.Cfg.Quirks.Inference.MaxWait.Seconds()), Departures: []InferredDeparture{}}
	f := s.st.feed
	if f == nil {
		return out
	}
	for td, in := range s.inferredAt(s.Now()) {
		tr := &f.Trips[td.Trip]
		sched := s.sched(td.Date, f.StopTimes[tr.First].Dep)
		out.Departures = append(out.Departures, InferredDeparture{
			TripID: s.tripID(td), GTFSTripID: tr.ID, ServiceDate: fmt.Sprintf("%08d", td.Date),
			ScheduledDeparture: sched.UTC(), Departure: sched.Add(in.Delay).UTC(),
			DelaySeconds: int(in.Delay.Seconds()), Vehicle: in.Vehicle, Ghost: in.Ghost,
		})
	}
	sort.Slice(out.Departures, func(a, b int) bool {
		return out.Departures[a].ScheduledDeparture.Before(out.Departures[b].ScheduledDeparture)
	})
	return out
}
