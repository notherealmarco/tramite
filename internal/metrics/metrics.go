// Package metrics holds the service's OpenMetrics instruments.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	Registry = prometheus.NewRegistry()

	// Fetches by outcome: new, not_modified, unchanged (same content), error.
	Fetches = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tramite_fetches_total",
		Help: "Upstream feed requests by outcome.",
	}, []string{"source", "feed", "result"})

	// How old a new feed version was when we got it (feed header timestamp
	// to download): what adaptive polling tries to keep small.
	FetchLag = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "tramite_fetch_lag_seconds",
		Help:    "Age of a new feed version (header timestamp) when it was downloaded.",
		Buckets: []float64{0.5, 1, 2, 3, 5, 8, 12, 20, 30, 60, 120},
	}, []string{"source", "feed"})

	FeedAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tramite_feed_age_seconds",
		Help: "Age of the newest feed version held (header timestamp).",
	}, []string{"source", "feed"})

	FeedInterval = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tramite_feed_interval_seconds",
		Help: "Learned typical time between upstream feed versions.",
	}, []string{"source", "feed"})

	Stale = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tramite_feed_stale",
		Help: "1 when a feed has not changed for much longer than usual.",
	}, []string{"source", "feed"})

	ProcessSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "tramite_process_seconds",
		Help:    "Time from a new feed version to updated state (before pushing).",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 14),
	}, []string{"source"})

	Vehicles = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tramite_vehicles",
		Help: "Live vehicles in the latest feed.",
	}, []string{"source"})

	Ghosts = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tramite_ghosts",
		Help: "Vehicles remembered after vanishing near the end of their trip.",
	}, []string{"source"})

	Inferred = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tramite_inferred_departures",
		Help: "Departures with an inferred bus, by kind (on_time, delayed).",
	}, []string{"source", "kind"})

	StaticTrips = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tramite_static_trips",
		Help: "Trips in the loaded timetable.",
	}, []string{"source"})

	StreamClients = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "tramite_stream_clients",
		Help: "Open SSE streams.",
	})

	StreamEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tramite_stream_events_total",
		Help: "SSE events sent, by kind.",
	}, []string{"kind"})
)

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		Fetches, FetchLag, FeedAge, FeedInterval, Stale, ProcessSeconds,
		Vehicles, Ghosts, Inferred, StaticTrips, StreamClients, StreamEvents,
	)
}

// Handler serves the metrics, in OpenMetrics format when asked for it.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{EnableOpenMetrics: true})
}
