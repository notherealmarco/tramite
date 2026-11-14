# Tramite

Realtime transit data, prepared from GTFS feeds for apps. Tramite fetches the
static timetable and the GTFS-Realtime feeds of each configured source, keeps
them in memory, fixes what it knows how to fix, and serves small,
ready-to-display answers (JSON, pushed over SSE), so clients never download a
whole city's feed.

## What it does

- **Fetching.** Static GTFS: checked every few hours with conditional
  requests. Realtime: adaptive polling. Tramite learns each feed's update
  rhythm, waits after a new version and checks every couple of seconds
  around the expected next one. This works when the upstream answers
  `304 Not Modified`; otherwise it polls at a fixed, polite interval. A
  feed that stops changing is flagged stale.
- **Matching.** Realtime trips are matched to the timetable by trip id, else
  by run (line, direction, start time) when the ids come from another
  timetable version.
- **Quirks**, switched on per source:
  - `ghost_memory`: buses that vanish from the feeds near the end of their
    trip (Rome: about one stop before the end, until the driver sets the next
    trip) are remembered for a while.
  - `inference`: a timetable-only departure from a terminus gets the bus
    that should run it: one heading there (other direction) or one
    remembered there. On time, or late by the time the bus can be ready.
    Shown only when less than `max_wait` away at the stop; optionally only
    the first per line and terminus.
- **Archive** (optional): every feed version saved under `data/raw` for
  analysis.

## API

Times are UTC (RFC 3339). Field names follow MOTIS where one exists. With
`transitous_feed_id` set, stop and trip ids match Transitous's:
`it-Lazio-Rome_71527`, `20261113_14:33_it-Lazio-Rome_0#2384-15`. Stop ids
are accepted with or without the prefix.

| Endpoint | |
|---|---|
| `GET /v1/sources` | sources, timetable version, feed freshness, learned intervals |
| `GET /v1/sources/{source}/stops/{stop}/departures?n=20&window=2h` | a stop's departures |
| `GET /v1/sources/{source}/lines/{line}/vehicles` | live buses of a line (short name or route id) |
| `GET /v1/vehicles?bbox=minLat,minLon,maxLat,maxLon` | live buses in an area, all sources |
| `GET /v1/stream?departures=rome:71527[,n]&line=rome:60&bbox=...` | SSE: current state on connect, then each change (repeat `departures`/`line`) |
| `GET /v1/sources/{source}/gtfs-rt/trip-updates.pb` | upstream TripUpdates plus inferred departures (`inferred:<trip>` entities), e.g. for MOTIS |
| `GET /healthz`, `GET /readyz` | liveness; ready once every timetable is loaded |
| `GET /metrics` | OpenMetrics (feed age, fetch lag, stale feeds, SSE clients, ...) |

JSON endpoints send an `ETag` and answer `If-None-Match` with 304.

### Departure fields

| `realTime` | `departureTimestampReason` | `vehicle.started` | Meaning |
|---|---|---|---|
| false | `NO_RT_DATA` | | timetable only |
| true | `PREDICTED` / `PROPAGATED` | false | live, bus waiting at the first stop |
| true | `PREDICTED` / `PROPAGATED` | true | live, bus on its way |
| false | `INFERRED` | | a known bus should run it; late when `departure` > `scheduledDeparture` |

`PROPAGATED`: the delay of an earlier stop carried forward. For `INFERRED`,
`vehicle.id` is the bus expected to run the departure.

### Stream events

`event: departures` or `event: vehicles`, with
`data: {"topic": "departures=rome:71527", "data": <same JSON as the endpoint>}`.
A heartbeat comment every 20 s. To change topics, open a new stream.

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

Memory: Rome's full timetable (166k trips, 5M stop times) loads in about 3 s
and the process runs at about 260 MB.

## Development and releases

- `just test`: vet, gofmt check and tests with the race detector (what CI
  runs on `devel` and pull requests).
- `just release alpha|beta|stable [major|minor|patch|X.Y.Z]`: fast-forwards
  `main` to `devel` and tags `vX.Y.Z[-alpha.N|-beta.N]`, only if the commit's
  CI passed. `just promote stable` tags the current `main` as-is.
- The `release` workflow builds the image and pushes it to this Forgejo's
  registry as `:<version>`, `:<channel>` (alpha / beta / stable) and
  `:latest`, then calls the Portainer webhook.

Forgejo secrets: `REGISTRY_TOKEN` (a token with package write access),
optional `REGISTRY_USER` (default: the repository owner), `PORTAINER_WEBHOOK`.
