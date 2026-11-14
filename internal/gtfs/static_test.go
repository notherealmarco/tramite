package gtfs_test

import (
	"testing"
	"time"

	"git.marcorealacci.me/marcorealacci/tramite/internal/gtfs"
	"git.marcorealacci.me/marcorealacci/tramite/internal/gtfstest"
)

func load(t *testing.T) *gtfs.Feed {
	t.Helper()
	f, err := gtfs.Load(gtfstest.Zip(gtfstest.Line60), nil, "v1")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestLoad(t *testing.T) {
	f := load(t)
	if len(f.Stops) != 3 || len(f.Trips) != 5 || len(f.StopTimes) != 15 {
		t.Fatalf("stops %d trips %d stop times %d", len(f.Stops), len(f.Trips), len(f.StopTimes))
	}
	if f.Location.String() != "Europe/Rome" {
		t.Fatalf("timezone %s", f.Location)
	}
	a := f.StopIdx["A"]
	var deps []int32
	for _, i := range f.ByStop[a] {
		deps = append(deps, f.StopTimes[i].Dep)
	}
	for i := 1; i < len(deps); i++ {
		if deps[i] < deps[i-1] {
			t.Fatalf("ByStop not sorted: %v", deps)
		}
	}
	out1 := f.TripIdx["out1"]
	if f.LastStop(out1).Stop != f.StopIdx["C"] || f.StartDep(out1) != 14*3600+33*60 {
		t.Fatal("trip first/last stop wrong")
	}
	if got := f.RunIdx[gtfs.RunKey{Route: f.RouteIdx["60"], Direction: 0, Start: 14*3600 + 49*60}]; got != f.TripIdx["out2"] {
		t.Fatalf("run index: got trip %d", got)
	}
	for i := 1; i < len(f.Starts); i++ {
		if f.StartDep(f.Starts[i]) < f.StartDep(f.Starts[i-1]) {
			t.Fatal("Starts not sorted")
		}
	}
}

func TestCalendar(t *testing.T) {
	f := load(t)
	svc := f.Trips[0].Service
	if !f.Active(svc, 20261113) || f.Active(svc, 20251231) || f.Active(svc, 20280101) {
		t.Fatal("calendar range wrong")
	}
}

func TestServiceDayStartAcrossDST(t *testing.T) {
	f := load(t)
	// 2026-10-25: clocks go back in Rome; GTFS times count from noon - 12h.
	got := f.ServiceDayStart(20261025)
	want := time.Date(2026, 10, 25, 12, 0, 0, 0, f.Location).Add(-12 * time.Hour)
	if !got.Equal(want) {
		t.Fatalf("got %s want %s", got, want)
	}
	if d := f.DateOf(gtfstest.At(0, 30, 0), -1); d != 20261112 {
		t.Fatalf("DateOf: %d", d)
	}
}
