# Tramite

The real-time API server for [Trames](https://github.com/notherealmarco/trames)

Realtime transit data, prepared from GTFS feeds for the app. Tramite fetches the
static timetable and the GTFS-Realtime feeds of each configured source, keeps
them in memory, fixes what it knows how to fix, and serves small,
ready-to-display answers (JSON, pushed over SSE), so clients never download a
whole city's feed.

Many quirks have been added to workaround issues in the official feed of my city (Rome). 

## What it does

- **Fetching.** Static GTFS: checked every few hours with conditional
  requests. Realtime: adaptive polling. Tramite learns each feed's update
  rhythm, waits after a new version and checks every couple of seconds
  around the expected next one. This works when the upstream answers
  `304 Not Modified`; otherwise it polls at a fixed, polite interval.
- **Stale feeds.** A feed whose own timestamp is older than `max_age`
  (default 10 min) is flagged stale and its data ignored: no live buses,
  departures back on the timetable, polling slowed to `min_poll`. It's also
  flagged when it stops changing for much longer than its usual interval.
- **Matching.** Realtime trips are matched to the timetable by trip id, else
  by run (line, direction, start time) when the ids come from another
  timetable version.
- **Quirks**, switched on per source:
  - `day_off_times`: a prediction more than 12 hours from the timetable has
    the right time on the wrong day:
    it is moved by whole days to the day nearest the timetable, or dropped
    if it is then still more than `max_delay` (default 3 h) away. Counted in
    `tramite_day_off_times_total`.
  - `ghost_memory`: buses that vanish from the feeds near the end of their
    trip are remembered for a while.
  - `inference`: a timetable-only departure from a terminus gets the bus
    that should run it: one heading there (other direction) or one
    remembered there. On time, or late by the time the bus can be ready.
    Shown only when less than `max_wait` away at the stop; optionally only
    the first per line and terminus.
- **Archive** (optional): every feed version saved under `data/raw` for
  analysis.

## API

Times are UTC (RFC 3339). Field names follow MOTIS where one exists. Ids are
Transitous's: every stop, route and trip id carries its source's prefix (the
`transitous_feed_id`, else the source id), so clients never name a source.
Tramite finds the source from the prefix; an id no source covers is a 404.

- stop `it-Lazio-Rome_71527`
- line `it-Lazio-Rome_85`: what passengers call it, the short name; covers every GTFS route with that short name
- route `it-Lazio-Rome_<route_id>`: one GTFS route exactly
- trip `20261113_14:33_it-Lazio-Rome_0#2384-15` (service date, start, prefix, GTFS trip_id)

| Endpoint | |
|---|---|
| `GET /v1/sources` | what Tramite covers (`idPrefix` per source), timetable version, feed freshness |
| `GET /v1/stops/{stop}/departures?n=20&window=2h` | a stop's departures |
| `GET /v1/stops/{stop}/departures?trip={trip}[&line={line}]` | one trip's row at that stop, whatever its state (bus already past, departure gone): for following a ride. A trip id from another timetable version is matched by run (its date and start minute, the stop, and `line` when given) |
| `GET /v1/trips/{trip}[?line={line}&stop={stop}]` | a whole trip: every stop in order with its times, and its bus. `stop` (a stop it calls at) and `line` let a trip id from another timetable version be matched by run; without `stop` such an id is a 404 |
| `GET /v1/lines/{line}/vehicles` | live buses of a line |
| `GET /v1/routes/{route}/vehicles` | live buses of one GTFS route |
| `GET /v1/vehicles?bbox=minLat,minLon,maxLat,maxLon` | live buses in an area, all sources |
| `GET /v1/stream` | SSE stream (publish/subscribe, below) |
| `POST /v1/stream/{id}/topics` | add/remove topics of an open stream |
| `GET /v1/sources/{source}/gtfs-rt/trip-updates.pb` | upstream TripUpdates plus inferred departures (`inferred:<trip>` entities), e.g. for MOTIS |
| `GET /healthz`, `GET /readyz` | liveness; ready once every timetable is loaded |
| `GET /metrics` | OpenMetrics (feed age, fetch lag, stale feeds, SSE clients, ...) |

### Publish/subscribe

A client keeps **one** stream open and changes what it follows:

1. `GET /v1/stream`: the first event is `event: hello`, `data: {"id": "<stream id>"}`.
2. `POST /v1/stream/{id}/topics` with `{"add": [...], "remove": [...]}`: 204 when applied. Each
   added topic gets its current state right away, then an event whenever it changes. 404 when
   the stream is gone (open a new one and subscribe again).

Topics:

| Topic | Event | Data |
|---|---|---|
| `departures=<stop>[,<n>]` | `departures` | the stop's board |
| `departures=<stop>,trip=<trip>[,line=<line>]` | `departures` | that trip's row at the stop |
| `trip=<trip>[,line=<line>][,stop=<stop>]` | `trip` | the whole trip |
| `line=<line>` / `route=<route>` | `vehicles` | buses of a line / route |
| `bbox=<minLat>,<minLon>,<maxLat>,<maxLon>` | `vehicles` | buses in the area |

Events carry `{"topic": "<as subscribed>", "data": <same JSON as the GET endpoint>}`. A heartbeat
comment every 20 s. Topics can also be given as query parameters of `GET /v1/stream`
(`?departures=…&line=…`) for a first set. At most 200 topics per stream.

JSON endpoints send an `ETag` and answer `If-None-Match` with 304, and are
gzipped when the client accepts it.

### Departure fields

| `realTime` | `departureTimestampReason` | `vehicle.started` | Meaning |
|---|---|---|---|
| false | `NO_RT_DATA` | | timetable only |
| true | `PREDICTED` / `PROPAGATED` | false | live, bus waiting at the first stop |
| true | `PREDICTED` / `PROPAGATED` | true | live, bus on its way |
| false | `INFERRED` | | a known bus should run it; late when `departure` > `scheduledDeparture` |

`PROPAGATED`: the delay of an earlier stop carried forward, to a stop the
feed leaves out or marks as having no data (Rome: the last stop of nearly
every trip). For `INFERRED`,
`vehicle.id` is the bus expected to run the departure. Live buses also come
with their position (`vehicle.lat`, `lon`, `bearing`, `timestamp`). Each row
has `arrival`/`scheduledArrival` too (same delay as the departure).

### Trip fields

A trip answer has the trip once (`tripId`, `gtfsTripId`, `routeShortName`,
`headsign`, `tripFrom`, `tripTo`, `vehicle`, `cancelled`, ...) and
`stopTimes`: one entry per stop in order, with `stopId`, `name`, `lat`,
`lon`, `stopSequence`, the four times, `realTime`,
`departureTimestampReason` and `cancelled` (that stop is skipped). Stops the
bus has passed (`stopSequence` < `vehicle.currentStopSequence`) are on the
timetable. A stop on the trip twice is listed twice. An inferred trip is
`INFERRED` at every stop, with the delay of its first.

`vehicle.occupancyStatus` (and on the vehicles endpoints): how crowded the
bus is now, GTFS-RT `OccupancyStatus` names (`EMPTY`,
`MANY_SEATS_AVAILABLE`, `FEW_SEATS_AVAILABLE`, `STANDING_ROOM_ONLY`,
`CRUSHED_STANDING_ROOM_ONLY`, `FULL`, `NOT_ACCEPTING_PASSENGERS`). Only when
the feed reports it (in Rome about half the buses), never for an inferred
bus. `occupancyPercentage` when the feed gives one.

## Running

```bash
cp config.example.yaml config.yaml
just run
```

Docker: the image reads `/config/config.yaml` (`TRAMITE_CONFIG`) and keeps
data in `/data`:

```bash
docker run -p 8080:8080 -v ./config.yaml:/config/config.yaml:ro -v tramite-data:/data \
  git.marcorealacci.me/marcorealacci/tramite:latest
```

Set `data_dir: /data` in the config used in the container.

Replaying archived feeds: serve the files over HTTP, point the config at
them and start with `-clock 2026-11-13T14:43:50+01:00` so "now" is when they
were recorded.