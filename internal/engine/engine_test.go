package engine_test

import (
	"testing"
	"time"

	rt "github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"

	"git.marcorealacci.me/marcorealacci/tramite/internal/config"
	"git.marcorealacci.me/marcorealacci/tramite/internal/engine"
	"git.marcorealacci.me/marcorealacci/tramite/internal/gtfs"
	g "git.marcorealacci.me/marcorealacci/tramite/internal/gtfstest"
	"git.marcorealacci.me/marcorealacci/tramite/internal/live"
)

const (
	stopped = rt.VehiclePosition_STOPPED_AT
	transit = rt.VehiclePosition_IN_TRANSIT_TO
)

type fixture struct {
	t   *testing.T
	src *engine.Source
	now time.Time
}

func newFixture(t *testing.T, quirks bool, firstOnly bool) *fixture {
	t.Helper()
	cfg, err := config.Parse([]byte(`sources: [{id: rome, transitous_feed_id: it-Lazio-Rome, static: {url: x}}]`))
	if err != nil {
		t.Fatal(err)
	}
	sc := cfg.Sources[0]
	sc.Quirks.Ghosts.Enabled = quirks
	sc.Quirks.Inference.Enabled = quirks
	sc.Quirks.Inference.FirstOnly = firstOnly
	fx := &fixture{t: t, now: g.At(14, 30, 0)}
	fx.src = engine.NewSource(sc, "")
	fx.src.Now = func() time.Time { return fx.now }
	f, err := gtfs.Load(g.Zip(g.Line60), nil, "v1")
	if err != nil {
		t.Fatal(err)
	}
	fx.src.SetStatic(f)
	return fx
}

func (fx *fixture) vehicles(vs ...g.VP) {
	ts, parsed, err := live.ParseVehicles(g.Vehicles(fx.now, vs...))
	if err != nil {
		fx.t.Fatal(err)
	}
	fx.src.ApplyVehicles(ts, parsed)
}

func (fx *fixture) tripUpdates(tus ...g.TU) {
	raw := g.TripUpdates(fx.now, tus...)
	ts, parsed, err := live.ParseTripUpdates(raw)
	if err != nil {
		fx.t.Fatal(err)
	}
	fx.src.ApplyTripUpdates(ts, parsed, raw)
}

// board returns the departures at stop by GTFS trip id.
func (fx *fixture) board(stop string) map[string]engine.StopTime {
	fx.t.Helper()
	b, err := fx.src.Departures(stop, 50, 3*time.Hour)
	if err != nil {
		fx.t.Fatal(err)
	}
	out := map[string]engine.StopTime{}
	for _, d := range b.StopTimes {
		out[d.GTFSTripID] = d
	}
	return out
}

func TestBoardTimetableOnly(t *testing.T) {
	fx := newFixture(t, false, false)
	b := fx.board("A")
	if len(b) != 3 {
		t.Fatalf("want out1..out3 (trips ending at A have no departure), got %v", keys(b))
	}
	d := b["out1"]
	if d.DepartureTimestampReason != engine.ReasonNoRT || d.RealTime || !d.Departure.Equal(g.At(14, 33, 0)) {
		t.Fatalf("out1: %+v", d)
	}
	if d.TripID != "20261113_14:33_it-Lazio-Rome_out1" || d.TripFrom.StopID != "it-Lazio-Rome_A" {
		t.Fatalf("Transitous ids: %s %s", d.TripID, d.TripFrom.StopID)
	}
	// Transitous-prefixed stop id works too.
	if len(fx.board("it-Lazio-Rome_A")) != 3 {
		t.Fatal("prefixed stop id not accepted")
	}
}

// A bus waiting at its first stop: the operator's forecast slides with the
// clock and can be a little in the past. It must stay listed (stop 71527,
// bus 5039 on line 60).
func TestWaitingBusAtTerminusStaysListed(t *testing.T) {
	fx := newFixture(t, false, false)
	fx.now = g.At(14, 44, 57)
	fx.tripUpdates(g.TU{Trip: "out1", Vehicle: "5039", Updates: []g.STU{{Seq: 1, Stop: "A", Time: g.At(14, 44, 0)}}})
	fx.vehicles(g.VP{Label: "5039", Trip: "out1", Route: "60", Seq: 1, Status: stopped, Lat: 41.9, Lon: 12.5})
	d, ok := fx.board("A")["out1"]
	if !ok {
		t.Fatal("waiting bus dropped from the board")
	}
	if !d.RealTime || d.DepartureTimestampReason != engine.ReasonPredicted || d.Vehicle == nil || *d.Vehicle.Started {
		t.Fatalf("want live, not started: %+v %+v", d, d.Vehicle)
	}

	// Once it has left, it's gone from A.
	fx.vehicles(g.VP{Label: "5039", Trip: "out1", Route: "60", Seq: 2, Status: transit, Lat: 41.905, Lon: 12.505})
	if _, ok := fx.board("A")["out1"]; ok {
		t.Fatal("departed bus still listed at its first stop")
	}
}

func TestPredictedPropagatedPassed(t *testing.T) {
	fx := newFixture(t, false, false)
	fx.now = g.At(14, 34, 0)
	fx.tripUpdates(g.TU{Trip: "out1", Updates: []g.STU{{Seq: 1, Stop: "A", Time: g.At(14, 38, 0)}}})
	a := fx.board("A")["out1"]
	if a.DepartureTimestampReason != engine.ReasonPredicted || !a.Departure.Equal(g.At(14, 38, 0)) {
		t.Fatalf("A: %+v", a)
	}
	b := fx.board("B")["out1"]
	if b.DepartureTimestampReason != engine.ReasonPropagated || !b.Departure.Equal(g.At(14, 45, 0)) {
		t.Fatalf("B: want 14:45 propagated, got %s %s", b.DepartureTimestampReason, b.Departure.In(g.Rome))
	}
	if _, ok := fx.board("C")["out1"]; ok {
		t.Fatal("C is the end of out1: nothing departs")
	}
	fx.tripUpdates(g.TU{Trip: "out1", Updates: []g.STU{{Seq: 2, Stop: "B", Time: g.At(14, 45, 0)}}})
	if _, ok := fx.board("A")["out1"]; ok {
		t.Fatal("A: predictions start after A, the bus has passed it")
	}
}

// A bus heading to the terminus makes the next departure on time (red),
// vanishes near the end (ghost) and still holds it, then reappears on it.
func TestGhostHoldsDepartureUntilBusReappears(t *testing.T) {
	fx := newFixture(t, true, true)
	fx.now = g.At(14, 36, 0)
	fx.tripUpdates(g.TU{Trip: "in2", Vehicle: "2110", Updates: []g.STU{{Seq: 3, Stop: "A", Time: g.At(14, 41, 0), NoData: true}}})
	fx.vehicles(g.VP{Label: "2110", Trip: "in2", Route: "60", Dir: 1, Seq: 2, Status: transit, Lat: 41.905, Lon: 12.505})

	d := fx.board("A")["out2"]
	if d.DepartureTimestampReason != engine.ReasonInferred || !d.Departure.Equal(g.At(14, 49, 0)) || d.Vehicle == nil || d.Vehicle.ID != "2110" {
		t.Fatalf("want out2 inferred on time with 2110: %+v %+v", d, d.Vehicle)
	}
	if o3 := fx.board("A")["out3"]; o3.DepartureTimestampReason != engine.ReasonNoRT {
		t.Fatalf("first only: out3 should stay scheduled, got %s", o3.DepartureTimestampReason)
	}

	// 2110 vanishes one stop before the end.
	fx.now = g.At(14, 38, 0)
	fx.tripUpdates()
	fx.vehicles()
	if st := fx.src.Status(); st.Ghosts != 1 {
		t.Fatalf("want 1 ghost, got %d", st.Ghosts)
	}
	d = fx.board("A")["out2"]
	if d.DepartureTimestampReason != engine.ReasonInferred || d.Vehicle.ID != "2110" {
		t.Fatalf("ghost should still hold out2: %+v", d)
	}

	// The driver sets the next trip: the bus is back, waiting at A.
	fx.now = g.At(14, 47, 0)
	fx.vehicles(g.VP{Label: "2110", Trip: "out2", Route: "60", Seq: 1, Status: stopped, Lat: 41.9, Lon: 12.5})
	if st := fx.src.Status(); st.Ghosts != 0 {
		t.Fatalf("ghost should be gone, got %d", st.Ghosts)
	}
	d = fx.board("A")["out2"]
	if d.Vehicle == nil || d.Vehicle.Started == nil || *d.Vehicle.Started {
		t.Fatalf("want 2110 on out2, not started: %+v", d.Vehicle)
	}
}

func TestGhostExpires(t *testing.T) {
	fx := newFixture(t, true, true)
	fx.now = g.At(14, 36, 0)
	fx.vehicles(g.VP{Label: "2110", Trip: "in2", Route: "60", Dir: 1, Seq: 2, Status: transit})
	fx.now = g.At(14, 37, 0)
	fx.vehicles()
	if fx.src.Status().Ghosts != 1 {
		t.Fatal("want a ghost")
	}
	fx.now = g.At(14, 53, 0) // > 15 min
	fx.vehicles()
	if n := fx.src.Status().Ghosts; n != 0 {
		t.Fatalf("ghost should have expired, %d left", n)
	}
}

func TestVanishedFarFromEndIsNoGhost(t *testing.T) {
	fx := newFixture(t, true, true)
	fx.src.Cfg.Quirks.Ghosts.MaxStopsLeft = 1
	fx.now = g.At(14, 21, 0)
	fx.vehicles(g.VP{Label: "2110", Trip: "in2", Route: "60", Dir: 1, Seq: 1, Status: stopped})
	fx.vehicles()
	if n := fx.src.Status().Ghosts; n != 0 {
		t.Fatalf("2 stops left with max 1: no ghost, got %d", n)
	}
}

// A bus arriving late: the departure is inferred delayed (purple) at the
// terminus and along the line, unless too far away to be useful.
func TestInferredDelayedAndMaxWait(t *testing.T) {
	fx := newFixture(t, true, true)
	fx.now = g.At(14, 40, 0)
	fx.tripUpdates(g.TU{Trip: "in2", Vehicle: "3429", Updates: []g.STU{{Seq: 3, Stop: "A", Time: g.At(14, 58, 0)}}})
	fx.vehicles(g.VP{Label: "3429", Trip: "in2", Route: "60", Dir: 1, Seq: 2, Status: transit})

	a := fx.board("A")["out2"] // 14:49, bus ready 14:58 + 3 = 15:01
	if a.DepartureTimestampReason != engine.ReasonInferred || !a.Departure.Equal(g.At(15, 1, 0)) {
		t.Fatalf("A: want inferred 15:01, got %s %s", a.DepartureTimestampReason, a.Departure.In(g.Rome))
	}
	b := fx.board("B")["out2"] // 14:56 + 12 = 15:08, 28 min away
	if b.DepartureTimestampReason != engine.ReasonInferred || !b.Departure.Equal(g.At(15, 8, 0)) {
		t.Fatalf("B: want inferred 15:08, got %s %s", b.DepartureTimestampReason, b.Departure.In(g.Rome))
	}
	fx.src.Cfg.Quirks.Inference.MaxWait.Duration = 25 * time.Minute
	b = fx.board("B")["out2"] // 28 min away > 25
	if b.DepartureTimestampReason != engine.ReasonNoRT || !b.Departure.Equal(g.At(14, 56, 0)) {
		t.Fatalf("B: beyond max_wait, want scheduled 14:56, got %s %s", b.DepartureTimestampReason, b.Departure.In(g.Rome))
	}
}

// First only: a later on-time departure wins over an earlier delayed one.
func TestFirstOnlyPrefersOnTime(t *testing.T) {
	for _, firstOnly := range []bool{true, false} {
		fx := newFixture(t, true, firstOnly)
		fx.now = g.At(14, 40, 0)
		fx.tripUpdates(
			g.TU{Trip: "in1", Vehicle: "X", Updates: []g.STU{{Seq: 3, Stop: "A", Time: g.At(14, 58, 0)}}},
			g.TU{Trip: "in2", Vehicle: "Y", Updates: []g.STU{{Seq: 3, Stop: "A", Time: g.At(14, 59, 0)}}},
		)
		fx.vehicles(
			g.VP{Label: "X", Trip: "in1", Route: "60", Dir: 1, Seq: 2, Status: transit},
			g.VP{Label: "Y", Trip: "in2", Route: "60", Dir: 1, Seq: 2, Status: transit},
		)
		b := fx.board("A")
		o2, o3 := b["out2"].DepartureTimestampReason, b["out3"].DepartureTimestampReason
		if firstOnly && (o2 != engine.ReasonNoRT || o3 != engine.ReasonInferred) {
			t.Fatalf("first only: want out2 scheduled, out3 inferred; got %s, %s", o2, o3)
		}
		if !firstOnly && (o2 != engine.ReasonInferred || o3 != engine.ReasonInferred) {
			t.Fatalf("all: want both inferred; got %s, %s", o2, o3)
		}
		if !b["out3"].Departure.Equal(g.At(15, 5, 0)) {
			t.Fatalf("out3 on time (Y ready 15:02): %s", b["out3"].Departure.In(g.Rome))
		}
	}
}

func TestVehiclesByLineAndArea(t *testing.T) {
	fx := newFixture(t, false, false)
	fx.vehicles(
		g.VP{Label: "1", Trip: "out1", Route: "60", Seq: 2, Lat: 41.905, Lon: 12.505},
		g.VP{Label: "2", Trip: "zzz", Route: "999", Seq: 2, Lat: 45, Lon: 9},
	)
	if vs := fx.src.Vehicles("60", nil); len(vs) != 1 || vs[0].ID != "1" || vs[0].Headsign != "P.ZA VENEZIA" || !*vs[0].Started {
		t.Fatalf("line 60: %+v", vs)
	}
	box := engine.BBox{MinLat: 44, MinLon: 8, MaxLat: 46, MaxLon: 10}
	if vs := fx.src.Vehicles("", &box); len(vs) != 1 || vs[0].ID != "2" {
		t.Fatalf("area: %+v", vs)
	}
}

func TestCorrectedTripUpdatesCarryInferred(t *testing.T) {
	fx := newFixture(t, true, true)
	fx.now = g.At(14, 40, 0)
	fx.tripUpdates(g.TU{Trip: "in2", Vehicle: "3429", Updates: []g.STU{{Seq: 3, Stop: "A", Time: g.At(14, 58, 0)}}})
	fx.vehicles(g.VP{Label: "3429", Trip: "in2", Route: "60", Dir: 1, Seq: 2, Status: transit})
	b, err := fx.src.CorrectedTripUpdates()
	if err != nil {
		t.Fatal(err)
	}
	var m rt.FeedMessage
	if err := proto.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range m.GetEntity() {
		if e.GetId() == "inferred:out2" {
			found = e.GetTripUpdate().GetDelay() == 12*60
		}
	}
	if len(m.GetEntity()) != 2 || !found {
		t.Fatalf("want upstream entity + inferred out2 (+12 min): %v", m.GetEntity())
	}
}

func keys(m map[string]engine.StopTime) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	return k
}

// A frozen upstream: once its data is older than max_age it is ignored.
func TestStaleFeedIgnored(t *testing.T) {
	fx := newFixture(t, true, true)
	fx.now = g.At(14, 40, 0)
	fx.tripUpdates(g.TU{Trip: "out3", Vehicle: "1", Updates: []g.STU{{Seq: 1, Stop: "A", Time: g.At(15, 9, 0)}}})
	fx.vehicles(g.VP{Label: "1", Trip: "out3", Route: "60", Seq: 1, Status: stopped, Lat: 41.9, Lon: 12.5})
	if d := fx.board("A")["out3"]; d.DepartureTimestampReason != engine.ReasonPredicted {
		t.Fatalf("fresh: want predicted, got %s", d.DepartureTimestampReason)
	}
	fx.now = g.At(14, 51, 0) // 11 min later, no new version
	d := fx.board("A")["out3"]
	if d.DepartureTimestampReason != engine.ReasonNoRT || d.RealTime || d.Vehicle != nil || !d.Departure.Equal(g.At(15, 5, 0)) {
		t.Fatalf("stale: want timetable 15:05 without vehicle, got %s %s %+v", d.DepartureTimestampReason, d.Departure.In(g.Rome), d.Vehicle)
	}
	if vs := fx.src.Vehicles("60", nil); len(vs) != 0 {
		t.Fatalf("stale: want no live buses, got %d", len(vs))
	}
	if st := fx.src.Status(); !st.VehiclesStale || !st.TripUpdatesStale || st.Inferred != 0 {
		t.Fatalf("status: %+v", st)
	}
}

// Buses missing after an upstream outage didn't vanish near a terminus.
func TestNoGhostsAfterOutage(t *testing.T) {
	fx := newFixture(t, true, true)
	fx.now = g.At(14, 36, 0)
	fx.vehicles(g.VP{Label: "2110", Trip: "in2", Route: "60", Dir: 1, Seq: 2, Status: transit})
	fx.now = g.At(14, 50, 0) // next version 14 min later
	fx.vehicles()
	if n := fx.src.Status().Ghosts; n != 0 {
		t.Fatalf("want no ghost after an outage, got %d", n)
	}
}

func TestOccupancy(t *testing.T) {
	fx := newFixture(t, false, false)
	fx.now = g.At(14, 30, 0)
	fx.vehicles(
		g.VP{Label: "5039", Trip: "out1", Route: "60", Seq: 1, Status: stopped, Lat: 41.9, Lon: 12.5,
			Occupancy: rt.VehiclePosition_STANDING_ROOM_ONLY.Enum()},
		g.VP{Label: "5088", Trip: "out2", Route: "60", Seq: 1, Status: stopped, Lat: 41.9, Lon: 12.5,
			Occupancy: rt.VehiclePosition_NO_DATA_AVAILABLE.Enum()},
	)
	b := fx.board("A")
	if v := b["out1"].Vehicle; v == nil || v.OccupancyStatus != "STANDING_ROOM_ONLY" {
		t.Fatalf("out1 vehicle: %+v", v)
	}
	if v := b["out2"].Vehicle; v == nil || v.OccupancyStatus != "" {
		t.Fatalf("NO_DATA_AVAILABLE should be omitted: %+v", v)
	}
	occ := map[string]string{}
	for _, v := range fx.src.Vehicles("60", nil) {
		occ[v.ID] = v.OccupancyStatus
	}
	if occ["5039"] != "STANDING_ROOM_ONLY" || occ["5088"] != "" {
		t.Fatalf("vehicles: %v", occ)
	}
}
