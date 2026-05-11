# SAIC-MQTT to PostgreSQL Logger — Implementation Plan

## Context

Jarle runs a TeslaMate stack (Grafana + PostgreSQL + MQTT) alongside a SAIC-iSmart-API gateway that publishes telemetry from his 2023 MG4 Electric to an internal MQTT broker. This Go microservice bridges that telemetry into a PostgreSQL database with TeslaMate-compatible schema, enabling Grafana dashboard reuse.

The original spec assumed bundled JSON payloads. **MQTT capture revealed the gateway publishes ~80 individual scalar values per refresh cycle on separate topics.** This fundamentally changed the architecture from JSON unmarshaling to topic-path-based field routing with a trigger-based snapshot approach.

---

## 1. Project Scaffolding

Create the Go module and project structure:

```
saic2postgresql/
├── main.go              # Entry point, signal handling, wiring
├── config.go            # Env var config struct
├── db.go                # PostgreSQL connection, schema creation, queries
├── mqtt.go              # MQTT connection, subscription, message routing
├── state.go             # In-memory vehicle state + state machine logic
├── Dockerfile
├── go.mod / go.sum
└── .gitignore
```

**Dependencies:** `github.com/eclipse/paho.golang/paho`, `github.com/jackc/pgx/v5`, `github.com/caarlos0/env/v11`

## 2. Configuration (`config.go`)

Environment variables:

| Variable | Default | Description |
|---|---|---|
| `MQTT_BROKER` | `tcp://mqtt:1883` | Broker address |
| `MQTT_TOPIC_PREFIX` | *(required)* | e.g. `saic/user@email.com/vehicles/VIN` |
| `DB_URI` | *(required)* | PostgreSQL connection string |
| `DRIVE_END_DEBOUNCE_SECONDS` | `180` | Seconds at speed=0 before ending a drive |

## 3. Database Schema (`db.go`)

Run `CREATE TABLE IF NOT EXISTS` on startup. **Separate database `mg_ismart`** (user creates it beforehand).

### `positions`
```sql
CREATE TABLE IF NOT EXISTS positions (
    id SERIAL PRIMARY KEY,
    date TIMESTAMP WITH TIME ZONE NOT NULL,
    latitude DOUBLE PRECISION,
    longitude DOUBLE PRECISION,
    speed DOUBLE PRECISION,
    power DOUBLE PRECISION,
    charging_power DOUBLE PRECISION,
    odometer DOUBLE PRECISION,
    battery_level DOUBLE PRECISION,
    outside_temp DOUBLE PRECISION,
    car_state VARCHAR(20)
);
CREATE INDEX IF NOT EXISTS idx_positions_date ON positions(date DESC);
```

### `drives`
```sql
CREATE TABLE IF NOT EXISTS drives (
    id SERIAL PRIMARY KEY,
    start_date TIMESTAMP WITH TIME ZONE NOT NULL,
    end_date TIMESTAMP WITH TIME ZONE,
    start_odometer DOUBLE PRECISION,
    end_odometer DOUBLE PRECISION,
    start_position_id INTEGER REFERENCES positions(id),
    end_position_id INTEGER REFERENCES positions(id),
    start_battery_level DOUBLE PRECISION,
    end_battery_level DOUBLE PRECISION,
    duration_minutes INTEGER
);
```

### `charges`
```sql
CREATE TABLE IF NOT EXISTS charges (
    id SERIAL PRIMARY KEY,
    start_date TIMESTAMP WITH TIME ZONE NOT NULL,
    end_date TIMESTAMP WITH TIME ZONE,
    start_battery_level DOUBLE PRECISION,
    end_battery_level DOUBLE PRECISION,
    energy_added DOUBLE PRECISION,
    start_position_id INTEGER REFERENCES positions(id),
    end_position_id INTEGER REFERENCES positions(id)
);
```

## 4. MQTT Subscription & Routing (`mqtt.go`)

Subscribe to `{MQTT_TOPIC_PREFIX}/#`. On each message:

1. Strip the prefix to get the **topic suffix** (e.g., `drivetrain/soc`, `location/speed`).
2. Parse the value (string → float/bool/string/JSON as appropriate).
3. Update the corresponding field in the in-memory `VehicleState` struct.
4. If the topic suffix is `refresh/lastVehicleState` → **trigger a snapshot evaluation** (this signals the refresh cycle is complete).

### Topics to handle (route by suffix)

| Suffix | Field in VehicleState | Type |
|---|---|---|
| `location/latitude` | Latitude | float |
| `location/longitude` | Longitude | float |
| `location/speed` | Speed | float |
| `location/heading` | Heading | int |
| `location/elevation` | Elevation | int |
| `drivetrain/mileage` | Odometer | float |
| `drivetrain/soc` | BatteryLevel | float |
| `drivetrain/soc_kwh` | BatteryKwh | float |
| `drivetrain/power` | Power | float |
| `drivetrain/running` | Running | bool |
| `drivetrain/charging` | Charging | bool |
| `drivetrain/chargerConnected` | ChargerConnected | bool |
| `drivetrain/totalBatteryCapacity` | BatteryCapacity | float |
| `climate/exteriorTemperature` | OutsideTemp | float |
| `obc/powerSinglePhase` | ChargingPowerSingle | float |
| `obc/powerThreePhase` | ChargingPowerThree | float |
| `available` | Available | string |
| `refresh/lastVehicleState` | *(trigger)* | timestamp |

All other topics are silently ignored.

## 5. State Machine (`state.go`)

### States: `Parked`, `Driving`, `Charging`

### In-memory struct
```go
type VehicleState struct {
    // Current field values (updated per-message)
    Latitude, Longitude   float64
    Speed, Odometer       float64
    Power, ChargingPower  float64  // ChargingPower = max(obc single, obc three)
    BatteryLevel          float64  // SoC %
    BatteryKwh            float64  // SoC kWh
    OutsideTemp           float64
    Running, Charging     bool
    ChargerConnected      bool
    Available             string

    // State machine
    CurrentState          State  // Parked, Driving, Charging
    ActiveDriveID         *int   // Non-nil while driving
    ActiveChargeID        *int   // Non-nil while charging
    LastPositionID        int
    LastInsertedSnapshot  Snapshot  // For deduplication
    SpeedZeroSince        *time.Time // For debounce
    ChargeStartKwh        float64   // soc_kwh at charge start
}
```

### Snapshot evaluation (on `refresh/lastVehicleState` trigger)

1. **Determine `car_state`**: driving > charging > online > offline
2. **Deduplication check**: Compare tracked fields (lat, lng, speed, soc, power, odometer, outside_temp) against `LastInsertedSnapshot`. If nothing changed, skip.
3. **INSERT position** if changed. Store returned `id` as `LastPositionID`.
4. **Evaluate state transitions:**

#### Parked -> Driving
- **Trigger:** `Running == true AND Speed > 0`
- **Action:** INSERT into `drives` with `start_date=now`, `start_odometer`, `start_position_id=LastPositionID`, `start_battery_level`. Store `ActiveDriveID`.

#### Driving -> Parked
- **Trigger A (immediate):** `Running == false` -> end drive now.
- **Trigger B (debounced):** `Speed == 0 AND Running == true` -> start debounce timer. If speed stays 0 for `DRIVE_END_DEBOUNCE_SECONDS`, end drive.
- **Action:** UPDATE `drives` row: `end_date`, `end_odometer`, `end_position_id`, `end_battery_level`, `duration_minutes`. Clear `ActiveDriveID`.

#### Parked -> Charging
- **Trigger:** `Charging == true`
- **Action:** INSERT into `charges` with `start_date=now`, `start_battery_level`, `start_position_id=LastPositionID`. Store `ActiveChargeID` and `ChargeStartKwh`.

#### Charging -> Parked
- **Trigger:** `Charging == false`
- **Action:** UPDATE `charges` row: `end_date`, `end_battery_level`, `energy_added = BatteryKwh - ChargeStartKwh`, `end_position_id`. Clear `ActiveChargeID`.

## 6. Startup Recovery (`db.go`)

On boot, before MQTT subscription:
1. Query for unclosed drive: `SELECT id, start_odometer, start_battery_level FROM drives WHERE end_date IS NULL ORDER BY start_date DESC LIMIT 1`
2. Query for unclosed charge: `SELECT id, start_battery_level FROM charges WHERE end_date IS NULL ORDER BY start_date DESC LIMIT 1`
3. If unclosed drive found -> set state to `Driving`, load `ActiveDriveID`.
4. If unclosed charge found -> set state to `Charging`, load `ActiveChargeID`.
5. Query last position: `SELECT id FROM positions ORDER BY date DESC LIMIT 1` -> set `LastPositionID`.

## 7. Graceful Shutdown (`main.go`)

```go
ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
defer cancel()
// Pass ctx to MQTT loop and DB operations
// On cancellation: disconnect MQTT, close DB pool
```

## 8. Logging

Use `log/slog` with JSON handler to stdout. Log:
- Startup: config loaded, DB connected, MQTT connected, state recovered
- State transitions: `Parked->Driving`, `Driving->Parked`, etc.
- Position inserts (debug level)
- Errors: DB write failures, MQTT disconnects

## 9. Docker (`Dockerfile`)

```dockerfile
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o saic-logger .

FROM scratch
COPY --from=builder /app/saic-logger .
ENTRYPOINT ["/saic-logger"]
```

## 10. Implementation Order

1. `go mod init` + `config.go` (env parsing)
2. `db.go` (connect, create tables, insert/update queries, startup recovery)
3. `state.go` (VehicleState struct, snapshot evaluation, state transitions)
4. `mqtt.go` (connect, subscribe, topic routing, trigger detection)
5. `main.go` (wire everything, signal handling)
6. `Dockerfile` + docker-compose snippet
7. End-to-end test against the live broker

## Verification

1. **Build:** `go build` succeeds with zero warnings
2. **Docker:** `docker build` produces a working image
3. **Integration:** Run against the live MQTT broker + a test PostgreSQL instance:
   - Verify positions are inserted on each refresh cycle
   - Verify deduplication works (no duplicate rows when car is idle)
   - Start/stop the car -> verify drives rows created and closed
   - Plug in charger -> verify charges rows created and closed
   - Kill and restart the container -> verify state recovery from DB
4. **Grafana:** Confirm basic queries work against the schema (SELECT from positions, drives, charges)


---

## Phase 2: OSRM Map Matching for Road-Snapped Drive Routes

### Problem

GPS data from the SAIC gateway arrives every ~30-40 seconds during drives, giving roughly 600m between points at highway speed. Grafana's geomap `route` layer connects these points with straight lines, which cuts corners on curves, interchanges, and winding roads. Additionally, some drives may have gaps in GPS data that create even longer straight-line artifacts.

### Solution: Self-hosted OSRM Map Matching

Use [OSRM (Open Source Routing Machine)](http://project-osrm.org/) to snap raw GPS traces to actual roads. OSRM's `/match/v1/driving/{coordinates}` API takes a series of GPS points with timestamps and returns the most likely road path.

### Architecture

```
Drive ends → Collect positions → OSRM /match API → Store matched path → Grafana queries matched path
```

### Database Changes

New table to store matched (road-snapped) coordinates per drive:

```sql
CREATE TABLE IF NOT EXISTS matched_positions (
    id SERIAL PRIMARY KEY,
    drive_id INTEGER REFERENCES drives(id) ON DELETE CASCADE,
    seq INTEGER NOT NULL,          -- ordering within the drive
    latitude DOUBLE PRECISION NOT NULL,
    longitude DOUBLE PRECISION NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_matched_positions_drive ON matched_positions(drive_id, seq);
```

### OSRM Container Setup

Add to docker-compose:

```yaml
osrm:
  image: osrm/osrm-backend
  restart: unless-stopped
  volumes:
    - ./osrm-data:/data
  command: osrm-routed --algorithm mld /data/norway-latest.osrm
  ports:
    - "5000:5000"
```

One-time data prep (Norway extract, ~500MB RAM):

```bash
mkdir osrm-data && cd osrm-data
wget https://download.geofabrik.de/europe/norway-latest.osm.pbf
docker run -t -v $(pwd):/data osrm/osrm-backend osrm-extract -p /data/car.lua /data/norway-latest.osm.pbf
docker run -t -v $(pwd):/data osrm/osrm-backend osrm-partition /data/norway-latest.osrm
docker run -t -v $(pwd):/data osrm/osrm-backend osrm-customize /data/norway-latest.osrm
```

### Go Implementation

New config env var:
- `OSRM_URL` — default `http://osrm:5000` (empty = disabled)

New file `osrm.go`:

1. **`MatchDrive(ctx, driveID, positions []Position) ([]MatchedPoint, error)`**
   - Build OSRM match request: `GET /match/v1/driving/{lon1},{lat1};{lon2},{lat2};...?timestamps={t1};{t2};...&geometries=geojson&overview=full`
   - Parse response GeoJSON geometry → extract coordinate array
   - Return matched coordinates with sequence numbers

2. **`StoreMatchedPath(ctx, pool, driveID, points []MatchedPoint) error`**
   - DELETE existing matched_positions for this drive_id (allow re-matching)
   - Batch INSERT the new matched coordinates

3. **Integration in `state.go`**: Call `MatchDrive` at the end of `transitionToParked` (when a drive ends), after `EndDrive` succeeds.

4. **Backfill command**: Add a `-backfill` CLI flag that:
   - Queries all drives that don't have matched_positions yet
   - Runs map matching for each
   - Useful for fixing historical data

### Grafana Dashboard Update

Replace the raw positions query with matched positions for the map:

```sql
-- Use matched path if available, fall back to raw positions
WITH drive_data AS (
  SELECT
    d.id as drive_id,
    d.start_date,
    d.end_date
  FROM drives d
  WHERE $__timeFilter(d.start_date)
),
matched AS (
  SELECT
    mp.drive_id,
    d.start_date + (mp.seq || ' seconds')::interval as time,
    mp.latitude,
    mp.longitude,
    true as is_matched
  FROM matched_positions mp
  JOIN drive_data d ON d.drive_id = mp.drive_id
),
raw AS (
  SELECT
    NULL as drive_id,
    p.date as time,
    p.latitude,
    p.longitude,
    false as is_matched
  FROM positions p
  JOIN drive_data d ON p.date BETWEEN d.start_date AND COALESCE(d.end_date, NOW())
  WHERE p.car_state = 'driving'
    AND p.latitude IS NOT NULL AND p.longitude IS NOT NULL
    AND p.latitude != 0 AND p.longitude != 0
    AND NOT EXISTS (
      SELECT 1 FROM matched_positions mp2 
      WHERE mp2.drive_id = d.drive_id
    )
)
SELECT time, latitude, longitude
FROM (
  SELECT * FROM matched
  UNION ALL
  SELECT * FROM raw
) combined
ORDER BY time
```

### Implementation Order

1. Add OSRM container to docker-compose + prep Norway map data
2. Create `matched_positions` table (add to `db.go` schema init)
3. Implement `osrm.go` with match + store functions
4. Integrate into drive-end transition in `state.go`
5. Add `-backfill` CLI command
6. Update Grafana dashboard query
7. Test end-to-end with a real drive
