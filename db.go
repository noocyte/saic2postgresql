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
