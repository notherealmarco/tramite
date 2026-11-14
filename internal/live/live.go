// Package live parses GTFS-Realtime feeds into plain structs.
package live

import (
	"time"

	rt "github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"
)

// GTFS-RT VehicleStopStatus names.
const (
	IncomingAt  = "INCOMING_AT"
	StoppedAt   = "STOPPED_AT"
	InTransitTo = "IN_TRANSIT_TO"
)

// TripRef is a GTFS-RT TripDescriptor.
type TripRef struct {
	TripID    string
	RouteID   string
	Direction int8 // -1 unknown
	StartTime string
	StartDate string
	Canceled  bool
}

type Vehicle struct {
	// Key is the label, else the id: what people see on the bus.
	Key       string
	ID, Label string
	Trip      TripRef
	Lat, Lon  float64
	Bearing   float32
	HasPos    bool
	Seq       int32 // current_stop_sequence, 0 unknown
	Status    string
	StopID    string
	Timestamp time.Time
}

type StopTimeUpdate struct {
	Seq      int32 // 0 unknown
	StopID   string
	Arr, Dep time.Time // zero when absent
	// Delay in seconds when the feed gives a delay without a time.
	Delay   *int32
	Skipped bool
	NoData  bool
}

// Time is the departure, else the arrival.
func (u StopTimeUpdate) Time() time.Time {
	if !u.Dep.IsZero() {
		return u.Dep
	}
	return u.Arr
}

type TripUpdate struct {
	Trip     TripRef
	Vehicle  string
	Delay    *int32 // trip-level delay, seconds
	Updates  []StopTimeUpdate
	Received time.Time
}

func trip(d *rt.TripDescriptor) TripRef {
	if d == nil {
		return TripRef{Direction: -1}
	}
	r := TripRef{TripID: d.GetTripId(), RouteID: d.GetRouteId(), Direction: -1,
		StartTime: d.GetStartTime(), StartDate: d.GetStartDate(),
		Canceled: d.GetScheduleRelationship() == rt.TripDescriptor_CANCELED}
	if d.DirectionId != nil {
		r.Direction = int8(d.GetDirectionId())
	}
	return r
}

func unix(s int64) time.Time {
	if s == 0 {
		return time.Time{}
	}
	return time.Unix(s, 0)
}

func parse(b []byte) (*rt.FeedMessage, time.Time, error) {
	var m rt.FeedMessage
	if err := proto.Unmarshal(b, &m); err != nil {
		return nil, time.Time{}, err
	}
	return &m, unix(int64(m.GetHeader().GetTimestamp())), nil
}

// HeaderTime reads only the feed timestamp.
func HeaderTime(b []byte) (time.Time, error) {
	_, ts, err := parse(b)
	return ts, err
}

func ParseVehicles(b []byte) (time.Time, []Vehicle, error) {
	m, ts, err := parse(b)
	if err != nil {
		return ts, nil, err
	}
	out := make([]Vehicle, 0, len(m.GetEntity()))
	for _, e := range m.GetEntity() {
		vp := e.GetVehicle()
		if vp == nil || e.GetIsDeleted() {
			continue
		}
		v := Vehicle{ID: vp.GetVehicle().GetId(), Label: vp.GetVehicle().GetLabel(), Trip: trip(vp.GetTrip()),
			Seq: int32(vp.GetCurrentStopSequence()), StopID: vp.GetStopId(), Timestamp: unix(int64(vp.GetTimestamp()))}
		v.Key = v.Label
		if v.Key == "" {
			v.Key = v.ID
		}
		if v.Key == "" {
			v.Key = e.GetId()
		}
		if p := vp.GetPosition(); p != nil {
			v.Lat, v.Lon, v.Bearing, v.HasPos = float64(p.GetLatitude()), float64(p.GetLongitude()), p.GetBearing(), true
		}
		if vp.CurrentStatus != nil {
			v.Status = vp.GetCurrentStatus().String()
		}
		out = append(out, v)
	}
	return ts, out, nil
}

func ParseTripUpdates(b []byte) (time.Time, []TripUpdate, error) {
	m, ts, err := parse(b)
	if err != nil {
		return ts, nil, err
	}
	out := make([]TripUpdate, 0, len(m.GetEntity()))
	for _, e := range m.GetEntity() {
		tu := e.GetTripUpdate()
		if tu == nil || e.GetIsDeleted() {
			continue
		}
		u := TripUpdate{Trip: trip(tu.GetTrip()), Vehicle: tu.GetVehicle().GetLabel(), Received: ts}
		if u.Vehicle == "" {
			u.Vehicle = tu.GetVehicle().GetId()
		}
		if tu.Delay != nil {
			d := tu.GetDelay()
			u.Delay = &d
		}
		for _, s := range tu.GetStopTimeUpdate() {
			st := StopTimeUpdate{Seq: int32(s.GetStopSequence()), StopID: s.GetStopId(),
				Skipped: s.GetScheduleRelationship() == rt.TripUpdate_StopTimeUpdate_SKIPPED,
				NoData:  s.GetScheduleRelationship() == rt.TripUpdate_StopTimeUpdate_NO_DATA}
			if a := s.GetArrival(); a != nil {
				st.Arr = unix(a.GetTime())
				if a.Time == nil && a.Delay != nil {
					d := a.GetDelay()
					st.Delay = &d
				}
			}
			if d := s.GetDeparture(); d != nil {
				st.Dep = unix(d.GetTime())
				if d.Time == nil && d.Delay != nil {
					v := d.GetDelay()
					st.Delay = &v
				}
			}
			u.Updates = append(u.Updates, st)
		}
		out = append(out, u)
	}
	return ts, out, nil
}
