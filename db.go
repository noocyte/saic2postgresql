package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `
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

CREATE TABLE IF NOT EXISTS matched_positions (
    id SERIAL PRIMARY KEY,
    drive_id INTEGER REFERENCES drives(id) ON DELETE CASCADE,
    seq INTEGER NOT NULL,
    latitude DOUBLE PRECISION NOT NULL,
    longitude DOUBLE PRECISION NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_matched_positions_drive ON matched_positions(drive_id, seq);
`

// RecoveredState holds state recovered from the database on startup.
type RecoveredState struct {
	CurrentState   State
	ActiveDriveID  *int
	ActiveChargeID *int
	LastPositionID int
	StartOdometer  float64
	StartBattery   float64
	ChargeStartKwh float64
}

// InitDB connects to PostgreSQL and ensures the schema exists.
func InitDB(ctx context.Context, uri string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, uri)
	if err != nil {
		return nil, fmt.Errorf("connecting to database: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("creating schema: %w", err)
	}

	slog.Info("database connected and schema ensured")
	return pool, nil
}

// InsertPosition inserts a snapshot into the positions table and returns the new row ID.
func InsertPosition(ctx context.Context, pool *pgxpool.Pool, snap Snapshot) (int, error) {
	var id int
	err := pool.QueryRow(ctx,
		`INSERT INTO positions (date, latitude, longitude, speed, power, charging_power, odometer, battery_level, outside_temp, car_state)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 RETURNING id`,
		time.Now(), snap.Latitude, snap.Longitude, snap.Speed, snap.Power,
		snap.ChargingPower, snap.Odometer, snap.BatteryLevel, snap.OutsideTemp, snap.CarState,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("inserting position: %w", err)
	}
	return id, nil
}

// StartDrive inserts a new drive row and returns its ID.
func StartDrive(ctx context.Context, pool *pgxpool.Pool, startDate time.Time, startOdo, startBattery float64, startPosID int) (int, error) {
	var id int
	err := pool.QueryRow(ctx,
		`INSERT INTO drives (start_date, start_odometer, start_position_id, start_battery_level)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id`,
		startDate, startOdo, startPosID, startBattery,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("starting drive: %w", err)
	}
	return id, nil
}

// EndDrive closes out a drive row with end data and computed duration.
func EndDrive(ctx context.Context, pool *pgxpool.Pool, id int, endDate time.Time, endOdo, endBattery float64, endPosID int) error {
	_, err := pool.Exec(ctx,
		`UPDATE drives
		 SET end_date = $2, end_odometer = $3, end_battery_level = $4, end_position_id = $5,
		     duration_minutes = EXTRACT(EPOCH FROM ($2 - start_date))::integer / 60
		 WHERE id = $1`,
		id, endDate, endOdo, endBattery, endPosID,
	)
	if err != nil {
		return fmt.Errorf("ending drive: %w", err)
	}
	return nil
}

// StartCharge inserts a new charge row and returns its ID.
func StartCharge(ctx context.Context, pool *pgxpool.Pool, startDate time.Time, startBattery float64, startPosID int) (int, error) {
	var id int
	err := pool.QueryRow(ctx,
		`INSERT INTO charges (start_date, start_battery_level, start_position_id)
		 VALUES ($1, $2, $3)
		 RETURNING id`,
		startDate, startBattery, startPosID,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("starting charge: %w", err)
	}
	return id, nil
}

// EndCharge closes out a charge row with end data.
func EndCharge(ctx context.Context, pool *pgxpool.Pool, id int, endDate time.Time, endBattery, energyAdded float64, endPosID int) error {
	_, err := pool.Exec(ctx,
		`UPDATE charges
		 SET end_date = $2, end_battery_level = $3, energy_added = $4, end_position_id = $5
		 WHERE id = $1`,
		id, endDate, endBattery, energyAdded, endPosID,
	)
	if err != nil {
		return fmt.Errorf("ending charge: %w", err)
	}
	return nil
}

// RecoverState queries the database for unclosed drives/charges and the last position
// to restore the in-memory state after a restart.
func RecoverState(ctx context.Context, pool *pgxpool.Pool) (*RecoveredState, error) {
	rs := &RecoveredState{
		CurrentState: StateParked,
	}

	// Check for unclosed drive
	var driveID int
	var startOdo, startBattery float64
	err := pool.QueryRow(ctx,
		`SELECT id, start_odometer, start_battery_level FROM drives WHERE end_date IS NULL ORDER BY start_date DESC LIMIT 1`,
	).Scan(&driveID, &startOdo, &startBattery)
	if err == nil {
		rs.CurrentState = StateDriving
		rs.ActiveDriveID = &driveID
		rs.StartOdometer = startOdo
		rs.StartBattery = startBattery
		slog.Info("recovered unclosed drive", "drive_id", driveID)
	} else if err != pgx.ErrNoRows {
		slog.Warn("error querying unclosed drives", "error", err)
	}

	// Check for unclosed charge
	var chargeID int
	var chargeBattery float64
	err = pool.QueryRow(ctx,
		`SELECT id, start_battery_level FROM charges WHERE end_date IS NULL ORDER BY start_date DESC LIMIT 1`,
	).Scan(&chargeID, &chargeBattery)
	if err == nil {
		rs.CurrentState = StateCharging
		rs.ActiveChargeID = &chargeID
		rs.StartBattery = chargeBattery
		slog.Info("recovered unclosed charge", "charge_id", chargeID)
	} else if err != pgx.ErrNoRows {
		slog.Warn("error querying unclosed charges", "error", err)
	}

	// Get last position ID
	var lastPosID int
	err = pool.QueryRow(ctx,
		`SELECT id FROM positions ORDER BY date DESC LIMIT 1`,
	).Scan(&lastPosID)
	if err == nil {
		rs.LastPositionID = lastPosID
	} else if err != pgx.ErrNoRows {
		slog.Warn("error querying last position", "error", err)
	}

	return rs, nil
}

// GetDrivePositions retrieves all GPS positions for a specific drive, ordered by time.
func GetDrivePositions(ctx context.Context, pool *pgxpool.Pool, driveID int) ([]DrivePosition, error) {
	rows, err := pool.Query(ctx,
		`SELECT p.latitude, p.longitude, p.date
		 FROM positions p
		 JOIN drives d ON p.date BETWEEN d.start_date AND COALESCE(d.end_date, NOW())
		 WHERE d.id = $1
		   AND p.car_state = 'driving'
		   AND p.latitude IS NOT NULL AND p.longitude IS NOT NULL
		   AND p.latitude != 0 AND p.longitude != 0
		 ORDER BY p.date`,
		driveID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying drive positions: %w", err)
	}
	defer rows.Close()

	var positions []DrivePosition
	for rows.Next() {
		var dp DrivePosition
		if err := rows.Scan(&dp.Latitude, &dp.Longitude, &dp.Timestamp); err != nil {
			return nil, fmt.Errorf("scanning drive position: %w", err)
		}
		positions = append(positions, dp)
	}
	return positions, rows.Err()
}

// StoreMatchedPath stores OSRM-matched coordinates for a drive.
// It replaces any existing matched path for the drive (allowing re-matching).
func StoreMatchedPath(ctx context.Context, pool *pgxpool.Pool, driveID int, points []MatchedPoint) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Delete existing matched positions for this drive
	if _, err := tx.Exec(ctx, `DELETE FROM matched_positions WHERE drive_id = $1`, driveID); err != nil {
		return fmt.Errorf("deleting old matched positions: %w", err)
	}

	// Batch insert new matched positions
	for i, p := range points {
		if _, err := tx.Exec(ctx,
			`INSERT INTO matched_positions (drive_id, seq, latitude, longitude) VALUES ($1, $2, $3, $4)`,
			driveID, i, p.Latitude, p.Longitude,
		); err != nil {
			return fmt.Errorf("inserting matched position %d: %w", i, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing matched positions: %w", err)
	}

	slog.Debug("stored matched path", "drive_id", driveID, "points", len(points))
	return nil
}

// GetUnmatchedDrives returns IDs of completed drives that don't have matched positions yet.
func GetUnmatchedDrives(ctx context.Context, pool *pgxpool.Pool) ([]int, error) {
	rows, err := pool.Query(ctx,
		`SELECT d.id FROM drives d
		 WHERE d.end_date IS NOT NULL
		   AND NOT EXISTS (SELECT 1 FROM matched_positions mp WHERE mp.drive_id = d.id)
		 ORDER BY d.start_date`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying unmatched drives: %w", err)
	}
	defer rows.Close()

	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning drive id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
