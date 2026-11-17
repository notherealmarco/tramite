// Package gtfstest builds small GTFS feeds and realtime messages for tests.
package gtfstest

import (
	"archive/zip"
	"bytes"
	"time"

	rt "github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"
)

// Zip packs files (name -> CSV content) into a GTFS zip.
func Zip(files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, _ := zw.Create(name)
		w.Write([]byte(content))
	}
	zw.Close()
	return buf.Bytes()
}

// Line60 is one line between terminus A and terminus C through B:
// out* trips A->B->C (direction 0), in* trips C->B->A (direction 1).
var Line60 = map[string]string{
	"agency.txt": "agency_id,agency_name,agency_url,agency_timezone\nATAC,ATAC,https://atac.example,Europe/Rome\n",
	"stops.txt": "stop_id,stop_name,stop_lat,stop_lon\n" +
		"A,LGO PUGLIESE,41.9000,12.5000\n" +
		"B,MIDDLE,41.9100,12.5100\n" +
		"C,P.ZA VENEZIA,41.9200,12.5200\n",
	"routes.txt": "route_id,agency_id,route_short_name,route_type,route_color\n60,ATAC,60,3,D02F2F\n",
	"calendar.txt": "service_id,monday,tuesday,wednesday,thursday,friday,saturday,sunday,start_date,end_date\n" +
		"ALL,1,1,1,1,1,1,1,20260101,20271231\n",
	"trips.txt": "route_id,service_id,trip_id,trip_headsign,trip_short_name,direction_id\n" +
		"60,ALL,out1,P.ZA VENEZIA,12694,0\n" +
		"60,ALL,out2,P.ZA VENEZIA,,0\n" +
		"60,ALL,out3,P.ZA VENEZIA,,0\n" +
		"60,ALL,in1,LGO PUGLIESE,,1\n" +
		"60,ALL,in2,LGO PUGLIESE,,1\n",
	"stop_times.txt": "trip_id,arrival_time,departure_time,stop_id,stop_sequence\n" +
		"out1,14:33:00,14:33:00,A,1\nout1,14:40:00,14:40:00,B,2\nout1,14:50:00,14:50:00,C,3\n" +
		"out2,14:49:00,14:49:00,A,1\nout2,14:56:00,14:56:00,B,2\nout2,15:06:00,15:06:00,C,3\n" +
		"out3,15:05:00,15:05:00,A,1\nout3,15:12:00,15:12:00,B,2\nout3,15:22:00,15:22:00,C,3\n" +
		"in1,14:05:00,14:05:00,C,1\nin1,14:15:00,14:15:00,B,2\nin1,14:25:00,14:25:00,A,3\n" +
		"in2,14:20:00,14:20:00,C,1\nin2,14:30:00,14:30:00,B,2\nin2,14:40:00,14:40:00,A,3\n",
}

// Rome is the timezone of Line60.
var Rome, _ = time.LoadLocation("Europe/Rome")

// At is a time on 2026-11-13 in Rome.
func At(h, m, s int) time.Time { return time.Date(2026, 11, 13, h, m, s, 0, Rome) }

// VP is one vehicle position.
type VP struct {
	Label, Trip, Route string
	Dir                uint32
	Seq                uint32
	Status             rt.VehiclePosition_VehicleStopStatus
	Lat, Lon           float32
	Occupancy          *rt.VehiclePosition_OccupancyStatus
}

func Vehicles(ts time.Time, vs ...VP) []byte {
	m := header(ts)
	for _, v := range vs {
		m.Entity = append(m.Entity, &rt.FeedEntity{Id: proto.String(v.Label), Vehicle: &rt.VehiclePosition{
			Trip:                &rt.TripDescriptor{TripId: proto.String(v.Trip), RouteId: proto.String(v.Route), DirectionId: proto.Uint32(v.Dir), StartDate: proto.String("20261113")},
			Vehicle:             &rt.VehicleDescriptor{Id: proto.String(v.Label), Label: proto.String(v.Label)},
			Position:            &rt.Position{Latitude: proto.Float32(v.Lat), Longitude: proto.Float32(v.Lon)},
			CurrentStopSequence: proto.Uint32(v.Seq),
			CurrentStatus:       v.Status.Enum(),
			Timestamp:           proto.Uint64(uint64(ts.Unix())),
			OccupancyStatus:     v.Occupancy,
		}})
	}
	b, _ := proto.Marshal(m)
	return b
}

// STU is a stop time update: stop sequence and predicted time.
type STU struct {
	Seq    uint32
	Stop   string
	Time   time.Time
	NoData bool
}

type TU struct {
	Trip, Vehicle string
	Updates       []STU
}

func TripUpdates(ts time.Time, tus ...TU) []byte {
	m := header(ts)
	for _, t := range tus {
		u := &rt.TripUpdate{Trip: &rt.TripDescriptor{TripId: proto.String(t.Trip), StartDate: proto.String("20261113")},
			Vehicle: &rt.VehicleDescriptor{Label: proto.String(t.Vehicle)}}
		for _, s := range t.Updates {
			stu := &rt.TripUpdate_StopTimeUpdate{StopSequence: proto.Uint32(s.Seq), StopId: proto.String(s.Stop),
				Arrival: &rt.TripUpdate_StopTimeEvent{Time: proto.Int64(s.Time.Unix())}}
			if s.NoData {
				stu.ScheduleRelationship = rt.TripUpdate_StopTimeUpdate_NO_DATA.Enum()
			}
			u.StopTimeUpdate = append(u.StopTimeUpdate, stu)
		}
		m.Entity = append(m.Entity, &rt.FeedEntity{Id: proto.String(t.Trip), TripUpdate: u})
	}
	b, _ := proto.Marshal(m)
	return b
}

func header(ts time.Time) *rt.FeedMessage {
	return &rt.FeedMessage{Header: &rt.FeedHeader{GtfsRealtimeVersion: proto.String("2.0"), Timestamp: proto.Uint64(uint64(ts.Unix()))}}
}
