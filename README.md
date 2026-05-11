# saic2postgresql

A lightweight Go microservice that bridges [SAIC iSmart](https://github.com/SAIC-iSmart-API/saic-python-mqtt-gateway) MQTT telemetry into PostgreSQL — designed to run alongside a [TeslaMate](https://github.com/teslamate-org/teslamate) stack so you can reuse Grafana dashboards for your MG/SAIC vehicle.

## What it does

The SAIC MQTT gateway publishes ~80 individual values per refresh cycle on separate MQTT topics. This service:

- **Subscribes** to all topics under your vehicle's MQTT prefix
- **Assembles** a snapshot each time `refresh/lastVehicleState` fires
- **Inserts** a position row into PostgreSQL (with deduplication when the car is idle)
- **Tracks drives** — automatically opens/closes drive sessions based on `running` + `speed`
- **Tracks charges** — automatically opens/closes charge sessions based on `charging` state
- **Recovers** on restart by checking the database for unclosed drives/charges
- **Health monitoring** — HTTP `/healthz` endpoint, internal watchdog, and Docker `HEALTHCHECK` for automatic restart on stale MQTT

## Prerequisites

- A running **TeslaMate stack** (PostgreSQL, Mosquitto, Grafana) via Docker Compose
- The **[saic-python-mqtt-gateway](https://github.com/SAIC-iSmart-API/saic-python-mqtt-gateway)** publishing to the same Mosquitto broker
- Your vehicle's MQTT topic prefix (e.g. `saic/you@email.com/vehicles/YOUR_VIN`)

## Setup

### 1. Create the database

The service creates its own tables on startup, but the database itself must exist first.

If you hit a collation version mismatch error, refresh the template first:

```sh
docker compose exec database psql -U teslamate -c "ALTER DATABASE template1 REFRESH COLLATION VERSION;"
docker compose exec database psql -U teslamate -c "ALTER DATABASE teslamate REFRESH COLLATION VERSION;"
```

Then create the database:

```sh
docker compose exec database psql -U teslamate -c "CREATE DATABASE mg_ismart;"
```

### 2. Add to your docker-compose.yml

Add this service block to your existing TeslaMate `docker-compose.yml`:

```yaml
  saic2postgresql:
    image: noocyte/saic2postgresql:latest
    container_name: saic2postgresql
    restart: unless-stopped
    depends_on:
      - mosquitto
      - database
    environment:
      - MQTT_BROKER=tcp://mosquitto:1883
      - MQTT_TOPIC_PREFIX=saic/you@email.com/vehicles/YOUR_VIN
      - DB_URI=postgres://teslamate:YOUR_PASSWORD@database:5432/mg_ismart?sslmode=disable
      - DRIVE_END_DEBOUNCE_SECONDS=180
      - HEALTH_PORT=8080
      - STALE_MINUTES=30
```

Replace the placeholders:

| Placeholder | Example |
|---|---|
| `you@email.com` | The email used for your SAIC/iSmart account |
| `YOUR_VIN` | Your vehicle's VIN (visible in the MQTT topic tree) |
| `YOUR_PASSWORD` | The `POSTGRES_PASSWORD` from your `database` service |

### 3. Start the service

```sh
docker compose up -d saic2postgresql
```

Check that it connected:

```sh
docker compose logs -f saic2postgresql
```

You should see:

```
{"level":"INFO","msg":"config loaded", ...}
{"level":"INFO","msg":"database connected and schema ensured"}
{"level":"INFO","msg":"state recovered", ...}
{"level":"INFO","msg":"health endpoint started", "port":8080, ...}
{"level":"INFO","msg":"mqtt connected, subscribing", ...}
{"level":"INFO","msg":"mqtt subscribed successfully", ...}
```

## Configuration

| Environment Variable | Required | Default | Description |
|---|---|---|---|
| `MQTT_BROKER` | No | `tcp://mqtt:1883` | MQTT broker address |
| `MQTT_TOPIC_PREFIX` | **Yes** | — | Vehicle topic prefix, e.g. `saic/user@email.com/vehicles/VIN` |
| `DB_URI` | **Yes** | — | PostgreSQL connection string (include `?sslmode=disable` for local containers) |
| `DRIVE_END_DEBOUNCE_SECONDS` | No | `180` | Seconds at speed=0 before a drive is ended (avoids splitting drives at traffic lights) |
| `HEALTH_PORT` | No | `8080` | Port for the HTTP health endpoint |
| `STALE_MINUTES` | No | `30` | Minutes without MQTT messages before health reports unhealthy |
| `OSRM_URL` | No | *(empty = disabled)* | OSRM server URL for map matching (e.g. `http://osrm:5000`) |

## OSRM Map Matching (optional)

When GPS data is sparse (points every 30-40 seconds), the map in Grafana shows straight lines between points instead of following actual roads. OSRM map matching solves this by snapping your GPS traces to the real road network.

### How it works

1. When a drive ends, the service sends the GPS trace to a local OSRM server
2. OSRM returns road-snapped coordinates that follow actual roads
3. The matched path is stored in `matched_positions` and used by the Grafana dashboard
4. Drives without matched data fall back to raw GPS positions automatically

### Setup

#### 1. Prepare the road network data (one-time, ~15 minutes)

Copy `osrm-setup.sh` to your Docker Compose directory and run it:

```sh
chmod +x osrm-setup.sh
./osrm-setup.sh
```

This downloads the Norway map (~1 GB) and processes it for OSRM. The resulting `osrm-data/` directory uses ~1.5 GB of disk space.

> **Other countries:** Change the download URL in the script. Find your region at [download.geofabrik.de](https://download.geofabrik.de/).

#### 2. Add the OSRM service to docker-compose.yml

```yaml
  osrm:
    image: osrm/osrm-backend
    restart: unless-stopped
    volumes:
      - ./osrm-data:/data
    command: osrm-routed --algorithm mld /data/norway-latest.osrm
```

#### 3. Add OSRM_URL to your saic2postgresql service

```yaml
  saic2postgresql:
    # ... existing config ...
    environment:
      # ... existing env vars ...
      - OSRM_URL=http://osrm:5000
    depends_on:
      - mosquitto
      - database
      - osrm
```

#### 4. Start the services

```sh
docker compose up -d osrm saic2postgresql
```

#### 5. Backfill historical drives (optional)

To map-match all your existing drives:

```sh
docker compose exec saic2postgresql /saic-logger --backfill
```

Check the logs:

```sh
docker compose logs saic2postgresql | grep backfill
```

### Updating map data

OSRM uses a static extract of the road network. To update it (e.g., after new roads are built):

```sh
docker compose stop osrm
rm -rf osrm-data/
./osrm-setup.sh
docker compose up -d osrm

# Re-match all drives with the new road data
docker compose exec saic2postgresql /saic-logger --backfill
```

### Resource usage

- **Disk:** ~1.5 GB for Norway road network data
- **RAM:** ~500 MB for the OSRM server (Norway)
- **CPU:** Negligible during normal operation; brief spike during map matching

## Health monitoring

The service includes three layers of monitoring:

### HTTP health endpoint

```sh
# From inside the Docker network
curl http://saic2postgresql:8080/healthz

# Returns 200 when healthy, 503 when stale:
{"status":"ok","last_message_received":"2026-04-16T22:01:30Z","since_last_message":"45s","uptime":"1h23m"}
```

### Docker HEALTHCHECK

Built into the image — Docker will automatically mark the container as `unhealthy` if no MQTT messages arrive within `STALE_MINUTES`. Combined with `restart: unless-stopped`, the container will be restarted automatically.

Check health status:

```sh
docker inspect --format='{{.State.Health.Status}}' saic2postgresql
```

### Watchdog logging

Every 5 minutes, the service logs its MQTT status. If messages stop arriving, you'll see warnings in the logs:

```
{"level":"WARN","msg":"watchdog: no MQTT messages received, subscription may be dead","since_last_message":"35m0s","threshold":"30m0s"}
```

## Database schema

Three tables are created automatically in the `mg_ismart` database:

- **`positions`** — one row per telemetry snapshot (lat, lon, speed, power, SoC, odometer, temp, car state)
- **`drives`** — one row per drive session (start/end time, odometer, battery level, duration)
- **`charges`** — one row per charge session (start/end time, battery level, energy added)

## State machine

```
         Running && Speed > 0          Charging == true
  Parked ───────────────────► Driving   Parked ──────────────────► Charging
         ◄───────────────────           ◄──────────────────────
          !Running (immediate)           Charging == false
          Speed=0 for debounce
```

## Building from source

### With Podman

```sh
# Login to Docker Hub
podman login docker.io

# Build and push (multi-arch: amd64 + arm64)
./build.sh v1.0.0
```

### Locally with Go

```sh
go mod tidy
go build -o saic-logger .
```

## Backup and Restore

Since `mg_ismart` lives in the same PostgreSQL container as TeslaMate, you can back up both databases in one go — or separately.

### Backup

Back up only the `mg_ismart` database:

```sh
docker compose exec -T database pg_dump -U teslamate mg_ismart > ./mg_ismart.bck
```

Back up both databases together (recommended):

```sh
docker compose exec -T database pg_dump -U teslamate teslamate > ./teslamate.bck
docker compose exec -T database pg_dump -U teslamate mg_ismart > ./mg_ismart.bck
```

> **Tip:** Use `-T` so the command works in cron jobs (no TTY required). Store backups somewhere safe — some Docker Compose GUIs delete the working directory on updates.

#### Automated daily backup via cron

```sh
crontab -e
```

Add a line like:

```
0 3 * * * cd /path/to/your/compose && docker compose exec -T database pg_dump -U teslamate mg_ismart > ./backups/mg_ismart_$(date +\%Y\%m\%d).bck
```

### Restore

```sh
# Stop saic2postgresql to avoid write conflicts
docker compose stop saic2postgresql

# Drop and recreate the database
docker compose exec -T database psql -U teslamate -c "DROP DATABASE mg_ismart;"
docker compose exec -T database psql -U teslamate -c "CREATE DATABASE mg_ismart;"

# Restore from backup
docker compose exec -T database psql -U teslamate -d mg_ismart < ./mg_ismart.bck

# Restart saic2postgresql
docker compose start saic2postgresql
```

## License

MIT
