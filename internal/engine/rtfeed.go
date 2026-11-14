package engine

import (
	"fmt"
	"sort"

	rt "github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"
)

// CorrectedTripUpdates is the upstream TripUpdates feed plus a TripUpdate
// for every inferred departure (its delay from the first stop on), for
// consumers that read GTFS-RT, e.g. a MOTIS instance. GTFS-RT has no way to
// say a time is inferred: those trips carry entity ids "inferred:<trip>".
func (s *Source) CorrectedTripUpdates() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var m rt.FeedMessage
	if len(s.st.tuRaw) > 0 {
		if err := proto.Unmarshal(s.st.tuRaw, &m); err != nil {
			return nil, err
		}
	}
	if m.Header == nil {
		m.Header = &rt.FeedHeader{GtfsRealtimeVersion: proto.String("2.0")}
	}
	f := s.st.feed
	if f == nil {
		return proto.Marshal(&m)
	}
	tds := make([]TripDay, 0, len(s.st.inferred))
	for td := range s.st.inferred {
		tds = append(tds, td)
	}
	sort.Slice(tds, func(a, b int) bool {
		if tds[a].Date != tds[b].Date {
			return tds[a].Date < tds[b].Date
		}
		return tds[a].Trip < tds[b].Trip
	})
	for _, td := range tds {
		in := s.st.inferred[td]
		tr := &f.Trips[td.Trip]
		first := f.StopTimes[tr.First]
		delay := int32(in.Delay.Seconds())
		dep := s.sched(td.Date, first.Dep).Unix() + int64(delay)
		desc := &rt.TripDescriptor{
			TripId:    proto.String(tr.ID),
			RouteId:   proto.String(f.Routes[tr.Route].ID),
			StartDate: proto.String(fmt.Sprintf("%08d", td.Date)),
			StartTime: proto.String(fmt.Sprintf("%02d:%02d:%02d", first.Dep/3600, first.Dep%3600/60, first.Dep%60)),
		}
		if tr.Direction >= 0 {
			desc.DirectionId = proto.Uint32(uint32(tr.Direction))
		}
		m.Entity = append(m.Entity, &rt.FeedEntity{
			Id: proto.String("inferred:" + tr.ID),
			TripUpdate: &rt.TripUpdate{
				Trip:    desc,
				Vehicle: &rt.VehicleDescriptor{Label: proto.String(in.Vehicle)},
				Delay:   proto.Int32(delay),
				StopTimeUpdate: []*rt.TripUpdate_StopTimeUpdate{{
					StopSequence: proto.Uint32(uint32(first.Seq)),
					StopId:       proto.String(f.Stops[first.Stop].ID),
					Departure:    &rt.TripUpdate_StopTimeEvent{Delay: proto.Int32(delay), Time: proto.Int64(dep)},
				}},
			},
		})
	}
	return proto.Marshal(&m)
}
