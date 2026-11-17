package engine

import (
	"sort"
	"strings"
	"time"

	"git.marcorealacci.me/marcorealacci/tramite/internal/live"
)

// VehicleInfo is a live bus for the map.
type VehicleInfo struct {
	Source              string  `json:"source"`
	ID                  string  `json:"id"`
	RouteID             string  `json:"routeId,omitempty"`
	RouteShortName      string  `json:"routeShortName,omitempty"`
	RouteColor          string  `json:"routeColor,omitempty"`
	RouteTextColor      string  `json:"routeTextColor,omitempty"`
	Headsign            string  `json:"headsign,omitempty"`
	TripShortName       string  `json:"tripShortName,omitempty"`
	TripID              string  `json:"tripId,omitempty"`
	GTFSTripID          string  `json:"gtfsTripId,omitempty"`
	DirectionID         *int8   `json:"directionId,omitempty"`
	Lat                 float64 `json:"lat"`
	Lon                 float64 `json:"lon"`
	Bearing             float32 `json:"bearing,omitempty"`
	Started             *bool   `json:"started,omitempty"`
	CurrentStopSequence int32   `json:"currentStopSequence,omitempty"`
	CurrentStatus       string  `json:"currentStatus,omitempty"`
	OccupancyStatus     string  `json:"occupancyStatus,omitempty"`
	OccupancyPercentage *uint32 `json:"occupancyPercentage,omitempty"`
	StopID              string  `json:"stopId,omitempty"`
	// As in the feed's trip descriptor: service date (YYYYMMDD) and start
	// time (HH:MM:SS) of the trip being run.
	StartDate string    `json:"startDate,omitempty"`
	StartTime string    `json:"startTime,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// BBox in degrees.
type BBox struct{ MinLat, MinLon, MaxLat, MaxLon float64 }

func (b BBox) Contains(lat, lon float64) bool {
	return lat >= b.MinLat && lat <= b.MaxLat && lon >= b.MinLon && lon <= b.MaxLon
}

// VehicleFilter selects live buses; empty fields match everything.
type VehicleFilter struct {
	// Line: what passengers call it, the API line id "<prefix>_<number>"
	// ("it-Lazio-Rome_85"): every GTFS route with that short name.
	Line string
	// Route: one GTFS route exactly, the API route id "<prefix>_<route_id>".
	Route string
	Box   *BBox
}

// Vehicles returns the live buses matching f.
func (s *Source) Vehicles(f VehicleFilter) []VehicleInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []VehicleInfo{}
	now := s.Now()
	line := strings.TrimPrefix(f.Line, s.Prefix()+"_")
	for k, v := range s.vehiclesAt(now) {
		if !v.HasPos || (f.Box != nil && !f.Box.Contains(v.Lat, v.Lon)) {
			continue
		}
		vi := s.vehicleInfo(k, v, now)
		if (f.Route != "" && vi.RouteID != f.Route) || (f.Line != "" && !strings.EqualFold(vi.RouteShortName, line)) {
			continue
		}
		out = append(out, vi)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

func (s *Source) vehicleInfo(key string, v *live.Vehicle, now time.Time) VehicleInfo {
	vi := VehicleInfo{Source: s.Cfg.ID, ID: key, Lat: v.Lat, Lon: v.Lon, Bearing: v.Bearing,
		CurrentStopSequence: v.Seq, CurrentStatus: v.Status, StopID: v.StopID, Timestamp: v.Timestamp.UTC(),
		OccupancyStatus: v.Occupancy, OccupancyPercentage: v.OccupancyPercentage,
		RouteID: v.Trip.RouteID, GTFSTripID: v.Trip.TripID, StartDate: v.Trip.StartDate, StartTime: v.Trip.StartTime}
	f := s.st.feed
	if f == nil {
		return vi
	}
	if td, ok := s.vehOfAt(now)[key]; ok {
		tr := &f.Trips[td.Trip]
		r := &f.Routes[tr.Route]
		vi.RouteID, vi.RouteShortName, vi.RouteColor, vi.RouteTextColor = r.ID, r.Line(), r.Color, r.TextColor
		vi.Headsign, vi.TripShortName, vi.TripID, vi.GTFSTripID = tr.Headsign, tr.ShortName, s.tripID(td), tr.ID
		if tr.Direction >= 0 {
			d := tr.Direction
			vi.DirectionID = &d
		}
		started := v.Seq == 0 || v.Seq > f.StopTimes[tr.First].Seq
		vi.Started = &started
	} else if ri, ok := f.RouteIdx[v.Trip.RouteID]; ok {
		r := &f.Routes[ri]
		vi.RouteShortName, vi.RouteColor, vi.RouteTextColor = r.Line(), r.Color, r.TextColor
	}
	if vi.StopID != "" {
		vi.StopID = s.Prefix() + "_" + vi.StopID
	}
	if vi.RouteID != "" {
		vi.RouteID = s.Prefix() + "_" + vi.RouteID
	}
	if vi.TripID == "" && vi.GTFSTripID != "" {
		vi.TripID = s.Prefix() + "_" + vi.GTFSTripID
	}
	return vi
}

// Status of the source's feeds, for /v1/sources.
type Status struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	TransitousFeed string `json:"transitousFeedId,omitempty"`
	// IDPrefix starts every stop, route and trip id of this source (after
	// the date and time in trip ids): how clients tell what it covers.
	IDPrefix      string    `json:"idPrefix"`
	StaticVersion string    `json:"staticVersion,omitempty"`
	Trips         int       `json:"trips"`
	Vehicles      int       `json:"vehicles"`
	VehiclesAt    time.Time `json:"vehiclesAt,omitzero"`
	TripUpdates   int       `json:"tripUpdates"`
	TripUpdatesAt time.Time `json:"tripUpdatesAt,omitzero"`
	// Stale: the feed's last version is older than max_age; its data is ignored.
	VehiclesStale    bool `json:"vehiclesStale"`
	TripUpdatesStale bool `json:"tripUpdatesStale"`
	Ghosts           int  `json:"ghosts"`
	Inferred         int  `json:"inferred"`
}

func (s *Source) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Status{ID: s.Cfg.ID, Name: s.Cfg.Name, TransitousFeed: s.Cfg.TransitousFeedID, IDPrefix: s.Prefix(),
		Vehicles: len(s.st.vehicles), VehiclesAt: s.st.vehTS.UTC(), TripUpdates: len(s.st.tus),
		TripUpdatesAt: s.st.tuTS.UTC(), Ghosts: len(s.st.ghosts), Inferred: len(s.inferredAt(s.Now()))}
	now := s.Now()
	st.VehiclesStale = s.st.vehicles != nil && !s.vehiclesFresh(now)
	st.TripUpdatesStale = s.st.tus != nil && !s.tusFresh(now)
	if f := s.st.feed; f != nil {
		st.StaticVersion, st.Trips = f.Version, len(f.Trips)
	}
	return st
}
