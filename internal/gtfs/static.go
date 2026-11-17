// Package gtfs loads a static GTFS zip into compact in-memory structures:
// stop times as one flat array, indexed per stop and per trip.
package gtfs

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Stop struct {
	ID, Code, Name string
	Lat, Lon       float64
	Parent         string
}

type Route struct {
	ID, ShortName, LongName string
	Color, TextColor        string
	Type                    int
	Agency                  string
}

// Line is what people call the route: its short name, else its id.
func (r *Route) Line() string {
	if r.ShortName != "" {
		return r.ShortName
	}
	return r.ID
}

type Trip struct {
	ID        string
	Route     int32
	Service   int32
	Direction int8 // -1 when unknown
	Headsign  string
	// ShortName is trip_short_name: the train number on Trenitalia's boards
	// ("12694"), an internal run code on Rome's buses.
	ShortName string
	Shape     string
	// StopTimes[First : First+Count] are this trip's stops in order.
	First, Count int32
}

// StopTime times are seconds from the start of the service day (may exceed
// 24h for trips after midnight).
type StopTime struct {
	Trip, Stop   int32
	Seq          int32
	Arr, Dep     int32
	Pickup, Drop uint8 // GTFS pickup_type / drop_off_type (0 regular, 1 none)
}

type service struct {
	weekdays   uint8 // bit 0 = Monday
	start, end int   // yyyymmdd, 0 = none
	added      map[int]bool
	removed    map[int]bool
}

// RunKey identifies a run independently of its trip id: line, direction and
// start time. Realtime feeds sometimes use ids of another timetable version.
type RunKey struct {
	Route     int32
	Direction int8
	Start     int32
}

type Feed struct {
	Stops     []Stop
	Routes    []Route
	Trips     []Trip
	StopTimes []StopTime

	StopIdx  map[string]int32
	RouteIdx map[string]int32
	TripIdx  map[string]int32
	// ByStop[stop] lists stop time indices at that stop, by departure time.
	ByStop [][]int32
	RunIdx map[RunKey]int32
	// Starts lists every trip by its first departure time.
	Starts []int32

	services   []service
	serviceIdx map[string]int32

	Location *time.Location
	// Hash of the zip, to tell versions apart.
	Version string
}

// Load parses a GTFS zip. loc overrides the agency timezone when not nil.
func Load(data []byte, loc *time.Location, version string) (*Feed, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name[strings.LastIndex(f.Name, "/")+1:]] = f
	}
	f := &Feed{
		StopIdx: map[string]int32{}, RouteIdx: map[string]int32{}, TripIdx: map[string]int32{},
		serviceIdx: map[string]int32{}, RunIdx: map[RunKey]int32{}, Version: version,
	}
	for _, name := range []string{"stops.txt", "routes.txt", "trips.txt", "stop_times.txt"} {
		if files[name] == nil {
			return nil, fmt.Errorf("gtfs: %s missing", name)
		}
	}
	if err := eachRow(files["agency.txt"], func(r row) error {
		if loc == nil && r.get("agency_timezone") != "" {
			l, err := time.LoadLocation(r.get("agency_timezone"))
			if err == nil {
				loc = l
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if loc == nil {
		loc = time.UTC
	}
	f.Location = loc

	if err := eachRow(files["stops.txt"], func(r row) error {
		f.StopIdx[r.get("stop_id")] = int32(len(f.Stops))
		lat, _ := strconv.ParseFloat(r.get("stop_lat"), 64)
		lon, _ := strconv.ParseFloat(r.get("stop_lon"), 64)
		f.Stops = append(f.Stops, Stop{ID: r.get("stop_id"), Code: r.get("stop_code"), Name: r.get("stop_name"),
			Lat: lat, Lon: lon, Parent: r.get("parent_station")})
		return nil
	}); err != nil {
		return nil, err
	}
	if err := eachRow(files["routes.txt"], func(r row) error {
		t, _ := strconv.Atoi(r.get("route_type"))
		f.RouteIdx[r.get("route_id")] = int32(len(f.Routes))
		f.Routes = append(f.Routes, Route{ID: r.get("route_id"), ShortName: r.get("route_short_name"),
			LongName: r.get("route_long_name"), Color: r.get("route_color"), TextColor: r.get("route_text_color"),
			Type: t, Agency: r.get("agency_id")})
		return nil
	}); err != nil {
		return nil, err
	}
	if err := f.loadCalendar(files["calendar.txt"], files["calendar_dates.txt"]); err != nil {
		return nil, err
	}
	if err := eachRow(files["trips.txt"], func(r row) error {
		route, ok := f.RouteIdx[r.get("route_id")]
		if !ok {
			return nil
		}
		dir := int8(-1)
		if d := r.get("direction_id"); d == "0" || d == "1" {
			dir = int8(d[0] - '0')
		}
		f.TripIdx[r.get("trip_id")] = int32(len(f.Trips))
		f.Trips = append(f.Trips, Trip{ID: r.get("trip_id"), Route: route, Service: f.service(r.get("service_id")),
			Direction: dir, Headsign: r.get("trip_headsign"),
			ShortName: r.get("trip_short_name"), Shape: r.get("shape_id"), First: -1})
		return nil
	}); err != nil {
		return nil, err
	}
	if err := f.loadStopTimes(files["stop_times.txt"]); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *Feed) service(id string) int32 {
	if i, ok := f.serviceIdx[id]; ok {
		return i
	}
	f.serviceIdx[id] = int32(len(f.services))
	f.services = append(f.services, service{added: map[int]bool{}, removed: map[int]bool{}})
	return int32(len(f.services) - 1)
}

func (f *Feed) loadCalendar(cal, dates *zip.File) error {
	days := []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}
	if err := eachRow(cal, func(r row) error {
		s := &f.services[f.service(r.get("service_id"))]
		for i, d := range days {
			if r.get(d) == "1" {
				s.weekdays |= 1 << i
			}
		}
		s.start, _ = strconv.Atoi(r.get("start_date"))
		s.end, _ = strconv.Atoi(r.get("end_date"))
		return nil
	}); err != nil {
		return err
	}
	return eachRow(dates, func(r row) error {
		s := &f.services[f.service(r.get("service_id"))]
		d, _ := strconv.Atoi(r.get("date"))
		switch r.get("exception_type") {
		case "1":
			s.added[d] = true
		case "2":
			s.removed[d] = true
		}
		return nil
	})
}

func (f *Feed) loadStopTimes(file *zip.File) error {
	st := make([]StopTime, 0, 1<<20)
	err := eachRow(file, func(r row) error {
		trip, ok := f.TripIdx[r.get("trip_id")]
		if !ok {
			return nil
		}
		stop, ok := f.StopIdx[r.get("stop_id")]
		if !ok {
			return nil
		}
		seq, _ := strconv.Atoi(r.get("stop_sequence"))
		arr, okA := parseTime(r.get("arrival_time"))
		dep, okD := parseTime(r.get("departure_time"))
		if !okA && !okD {
			return nil // untimed stop: not supported yet
		}
		if !okA {
			arr = dep
		}
		if !okD {
			dep = arr
		}
		pu, _ := strconv.Atoi(r.get("pickup_type"))
		do, _ := strconv.Atoi(r.get("drop_off_type"))
		st = append(st, StopTime{Trip: trip, Stop: stop, Seq: int32(seq), Arr: arr, Dep: dep, Pickup: uint8(pu), Drop: uint8(do)})
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(st, func(i, j int) bool {
		if st[i].Trip != st[j].Trip {
			return st[i].Trip < st[j].Trip
		}
		return st[i].Seq < st[j].Seq
	})
	// Exact-size copies: the growing slice can be twice as big as needed.
	f.StopTimes = slices.Clone(st)
	st = f.StopTimes
	perStop := make([]int32, len(f.Stops))
	for i := range st {
		perStop[st[i].Stop]++
	}
	flat := make([]int32, len(st))
	f.ByStop = make([][]int32, len(f.Stops))
	off := 0
	for i, n := range perStop {
		f.ByStop[i] = flat[off : off : off+int(n)]
		off += int(n)
	}
	for i := range st {
		t := &f.Trips[st[i].Trip]
		if t.First < 0 {
			t.First = int32(i)
		}
		t.Count++
		f.ByStop[st[i].Stop] = append(f.ByStop[st[i].Stop], int32(i))
	}
	for _, idx := range f.ByStop {
		sort.Slice(idx, func(a, b int) bool { return st[idx[a]].Dep < st[idx[b]].Dep })
	}
	for i := range f.Trips {
		t := &f.Trips[i]
		if t.First >= 0 {
			f.RunIdx[RunKey{t.Route, t.Direction, st[t.First].Dep}] = int32(i)
			f.Starts = append(f.Starts, int32(i))
		}
	}
	sort.Slice(f.Starts, func(a, b int) bool { return f.StartDep(f.Starts[a]) < f.StartDep(f.Starts[b]) })
	return nil
}

// TripStops returns trip t's stop times in order.
func (f *Feed) TripStops(t int32) []StopTime {
	tr := &f.Trips[t]
	if tr.First < 0 {
		return nil
	}
	return f.StopTimes[tr.First : tr.First+tr.Count]
}

// StartDep is a trip's first departure (seconds from service day start).
func (f *Feed) StartDep(t int32) int32 { return f.StopTimes[f.Trips[t].First].Dep }

// LastStop is a trip's final stop time.
func (f *Feed) LastStop(t int32) StopTime {
	tr := &f.Trips[t]
	return f.StopTimes[tr.First+tr.Count-1]
}

// Active tells whether a service runs on date (yyyymmdd).
func (f *Feed) Active(svc int32, date int) bool {
	s := &f.services[svc]
	if s.removed[date] {
		return false
	}
	if s.added[date] {
		return true
	}
	if s.start == 0 || date < s.start || date > s.end {
		return false
	}
	y, m, d := date/10000, date%10000/100, date%100
	wd := (int(time.Date(y, time.Month(m), d, 12, 0, 0, 0, time.UTC).Weekday()) + 6) % 7
	return s.weekdays&(1<<wd) != 0
}

// ServiceDayStart is the GTFS reference time of a service date: noon minus
// 12 hours, local time (right across DST changes).
func (f *Feed) ServiceDayStart(date int) time.Time {
	return time.Date(date/10000, time.Month(date%10000/100), date%100, 12, 0, 0, 0, f.Location).Add(-12 * time.Hour)
}

// DateOf is the yyyymmdd of t in the feed's timezone, shifted by days.
func (f *Feed) DateOf(t time.Time, days int) int {
	l := t.In(f.Location).AddDate(0, 0, days)
	return l.Year()*10000 + int(l.Month())*100 + l.Day()
}

func parseTime(s string) (int32, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	p := strings.Split(s, ":")
	if len(p) != 3 {
		return 0, false
	}
	h, e1 := strconv.Atoi(p[0])
	m, e2 := strconv.Atoi(p[1])
	sec, e3 := strconv.Atoi(p[2])
	if e1 != nil || e2 != nil || e3 != nil {
		return 0, false
	}
	return int32(h*3600 + m*60 + sec), true
}

type row struct {
	rec []string
	col map[string]int
}

func (r row) get(name string) string {
	if i, ok := r.col[name]; ok && i < len(r.rec) {
		return strings.TrimSpace(r.rec[i])
	}
	return ""
}

// eachRow calls fn for every record of an optional CSV file.
func eachRow(file *zip.File, fn func(row) error) error {
	if file == nil {
		return nil
	}
	rc, err := file.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	cr := csv.NewReader(rc)
	cr.ReuseRecord = true
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	head, err := cr.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	col := map[string]int{}
	for i, h := range head {
		col[strings.TrimPrefix(strings.TrimSpace(h), "\ufeff")] = i
	}
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", file.Name, err)
		}
		if err := fn(row{rec, col}); err != nil {
			return err
		}
	}
}
