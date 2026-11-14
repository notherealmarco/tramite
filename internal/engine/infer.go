package engine

import (
	"sort"
	"time"
)

type run struct {
	td    TripDay
	sched time.Time
}

type incoming struct {
	ready   time.Time // end of its trip (or when it vanished), not before now
	vehicle string
	ghost   bool
}

// infer finds timetable-only departures from a terminus that a known bus
// should run: buses of the line heading to the terminus (other direction,
// trip ending nearby) and buses remembered there after vanishing. Per line
// and terminus the departures still needing a bus, in order, take the buses
// in order of arrival (plus the turnaround). A departure with a live
// forecast but no bus on it yet takes one and keeps its forecast.
func (s *Source) infer(now time.Time) map[TripDay]Inference {
	f := s.st.feed
	q := s.Cfg.Quirks.Inference
	type key struct{ route, stop int32 }
	groups := map[key][]run{}
	for _, off := range []int{-1, 0} {
		date := f.DateOf(now, off)
		ds := f.ServiceDayStart(date)
		lo := int32(now.Add(-time.Minute).Sub(ds) / time.Second)
		hi := int32(now.Add(q.Horizon.Duration).Sub(ds) / time.Second)
		i := sort.Search(len(f.Starts), func(i int) bool { return f.StartDep(f.Starts[i]) >= lo })
		for ; i < len(f.Starts) && f.StartDep(f.Starts[i]) < hi; i++ {
			t := f.Starts[i]
			tr := &f.Trips[t]
			if !f.Active(tr.Service, date) {
				continue
			}
			first := f.StopTimes[tr.First]
			k := key{tr.Route, first.Stop}
			groups[k] = append(groups[k], run{TripDay{t, date}, ds.Add(time.Duration(first.Dep) * time.Second)})
		}
	}

	vehTrip, vehOf, tus := s.vehTripAt(now), s.vehOfAt(now), s.tusAt(now)
	out := map[TripDay]Inference{}
	for k, runs := range groups {
		sort.Slice(runs, func(a, b int) bool { return runs[a].sched.Before(runs[b].sched) })
		var needing []run
		anyTimetable := false
		for _, r := range runs {
			if vehTrip[r.td] != nil {
				continue // its bus is already on it
			}
			needing = append(needing, r)
			if tus[r.td] == nil {
				anyTimetable = true
			}
		}
		if !anyTimetable {
			continue
		}
		direction := int8(-1)
		own := map[int32]bool{}
		for _, r := range runs {
			own[r.td.Trip] = true
			if d := f.Trips[r.td.Trip].Direction; d >= 0 && direction < 0 {
				direction = d
			}
		}
		term := f.Stops[k.stop]
		near := func(stop int32) bool {
			st := f.Stops[stop]
			return distanceMeters(st.Lat, st.Lon, term.Lat, term.Lon) <= q.MaxEndDistanceMeters
		}
		otherWay := func(d int8) bool { return direction < 0 || d < 0 || d != direction }
		limit := now.Add(q.Horizon.Duration)

		var buses []incoming
		for key, td := range vehOf {
			tr := &f.Trips[td.Trip]
			if tr.Route != k.route || own[td.Trip] || !otherWay(tr.Direction) || !near(f.LastStop(td.Trip).Stop) {
				continue
			}
			end := s.predictedEnd(td, now)
			if !end.Before(limit) {
				continue
			}
			if end.Before(now) {
				end = now
			}
			buses = append(buses, incoming{end, key, false})
		}
		for _, g := range s.st.ghosts {
			route, ok := f.RouteIdx[g.RouteID]
			stop, ok2 := f.StopIdx[g.EndStopID]
			if !ok || !ok2 || route != k.route || !otherWay(g.Direction) || !near(stop) {
				continue
			}
			end := g.EndTime
			if end.Before(g.LastSeen) {
				end = g.LastSeen
			}
			if end.Before(now) {
				end = now
			}
			buses = append(buses, incoming{end, g.Vehicle, true})
		}
		sort.Slice(buses, func(a, b int) bool { return buses[a].ready.Before(buses[b].ready) })

		type result struct {
			td TripDay
			in Inference
		}
		var res []result
		b := 0
		for _, r := range needing {
			if b >= len(buses) {
				break
			}
			ready := buses[b].ready.Add(q.Turnaround.Duration)
			delay := ready.Sub(r.sched)
			live := tus[r.td] != nil
			// Too late for this one (every later bus is later still): it stays
			// on the timetable and this bus goes to the next departure. A
			// live departure keeps its bus whatever the timetable says.
			if !live && delay > q.MaxDelay.Duration {
				continue
			}
			bus := buses[b]
			b++
			if live {
				continue
			}
			if delay < 0 {
				delay = 0
			}
			res = append(res, result{r.td, Inference{Delay: delay.Round(time.Second), Vehicle: bus.vehicle, Ghost: bus.ghost}})
		}
		if q.FirstOnly && len(res) > 1 {
			pick := res[0]
			for _, r := range res {
				if r.in.Delay == 0 {
					pick = r
					break
				}
			}
			res = []result{pick}
		}
		for _, r := range res {
			out[r.td] = r.in
		}
	}
	return out
}
