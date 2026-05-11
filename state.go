package main

import (
	"context"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// State represents the current vehicle state.
type State int

const (
	StateParked   State = iota
	StateDriving
	StateCharging
)

func (s State) String() string {
	switch s {
	case StateParked:
		return "parked"
	case StateDriving:
		return "driving"
	case StateCharging:
		return "charging"
	default:
		return "unknown"
	}
}

// Snapshot holds the telemetry fields that get inserted into the positions table.
type Snapshot struct {
	Latitude      float64
	Longitude     float64
	Speed         float64
	Power         float64
	ChargingPower float64
	Odometer      float64
	BatteryLevel  float64
	OutsideTemp   float64
	CarState      string
}

// Equal returns true if all tracked fields match (used for deduplication).
func (s Snapshot) Equal(o Snapshot) bool {
	return s.Latitude == o.Latitude &&
		s.Longitude == o.Longitude &&
		s.Speed == o.Speed &&
		s.Power == o.Power &&
		s.ChargingPower == o.ChargingPower &&
		s.Odometer == o.Odometer &&
		s.BatteryLevel == o.BatteryLevel &&
		s.OutsideTemp == o.OutsideTemp &&
		s.CarState == o.CarState
}

// VehicleState holds the current telemetry and state machine for one vehicle.
type VehicleState struct {
	mu sync.Mutex

	// Current field values (updated per MQTT message)
	Latitude            float64
	Longitude           float64
	Speed               float64
	Odometer            float64
	Power               float64
	ChargingPowerSingle float64
	ChargingPowerThree  float64
	BatteryLevel        float64
	BatteryKwh          float64
	BatteryCapacity     float64
	OutsideTemp         float64
	Running             bool
	Charging            bool
	ChargerConnected    bool
	Available           string
	Heading             int
	Elevation           int

	// State machine
	CurrentState   State
	ActiveDriveID  *int
	ActiveChargeID *int
	LastPositionID int
	LastSnapshot   Snapshot
	HasSnapshot    bool
	SpeedZeroSince *time.Time
	ChargeStartKwh float64

	// Dependencies
	db   *pgxpool.Pool
	cfg  *Config
	osrm *OSRMClient
}

// NewVehicleState creates a new VehicleState wired to the database and config.
func NewVehicleState(pool *pgxpool.Pool, cfg *Config, osrm *OSRMClient) *VehicleState {
	return &VehicleState{
		db:           pool,
		cfg:          cfg,
		osrm:         osrm,
		CurrentState: StateParked,
	}
}

// Recover restores state from the database after a restart.
func (vs *VehicleState) Recover(ctx context.Context) error {
	rs, err := RecoverState(ctx, vs.db)
	if err != nil {
		return err
	}
	vs.CurrentState = rs.CurrentState
	vs.ActiveDriveID = rs.ActiveDriveID
	vs.ActiveChargeID = rs.ActiveChargeID
	vs.LastPositionID = rs.LastPositionID
	vs.ChargeStartKwh = rs.ChargeStartKwh
	slog.Info("state recovered", "state", vs.CurrentState.String(), "lastPositionID", vs.LastPositionID)
	return nil
}

// UpdateField routes an MQTT topic suffix to the corresponding struct field.
func (vs *VehicleState) UpdateField(suffix, value string) {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	switch suffix {
	case "location/latitude":
		vs.Latitude = parseFloat(value)
	case "location/longitude":
		vs.Longitude = parseFloat(value)
	case "location/speed":
		vs.Speed = parseFloat(value)
	case "location/heading":
		vs.Heading = parseInt(value)
	case "location/elevation":
		vs.Elevation = parseInt(value)
	case "drivetrain/mileage":
		vs.Odometer = parseFloat(value)
	case "drivetrain/soc":
		vs.BatteryLevel = parseFloat(value)
	case "drivetrain/soc_kwh":
		vs.BatteryKwh = parseFloat(value)
	case "drivetrain/power":
		vs.Power = parseFloat(value)
	case "drivetrain/running":
		vs.Running = parseBool(value)
	case "drivetrain/charging":
		vs.Charging = parseBool(value)
	case "drivetrain/chargerConnected":
		vs.ChargerConnected = parseBool(value)
	case "drivetrain/totalBatteryCapacity":
		vs.BatteryCapacity = parseFloat(value)
	case "climate/exteriorTemperature":
		vs.OutsideTemp = parseFloat(value)
	case "obc/powerSinglePhase":
		vs.ChargingPowerSingle = parseFloat(value)
	case "obc/powerThreePhase":
		vs.ChargingPowerThree = parseFloat(value)
	case "available":
		vs.Available = strings.TrimSpace(value)
	}
}

// EvaluateSnapshot is called on the refresh/lastVehicleState trigger.
// It builds a snapshot, deduplicates, inserts a position, and evaluates state transitions.
func (vs *VehicleState) EvaluateSnapshot(ctx context.Context) {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	// Determine car_state
	carState := vs.determineCarState()

	// Compute charging power as max of single and three phase
	chargingPower := math.Max(vs.ChargingPowerSingle, vs.ChargingPowerThree)

	snap := Snapshot{
		Latitude:      vs.Latitude,
		Longitude:     vs.Longitude,
		Speed:         vs.Speed,
		Power:         vs.Power,
		ChargingPower: chargingPower,
		Odometer:      vs.Odometer,
		BatteryLevel:  vs.BatteryLevel,
		OutsideTemp:   vs.OutsideTemp,
		CarState:      carState,
	}

	// Deduplication check
	if vs.HasSnapshot && snap.Equal(vs.LastSnapshot) {
		slog.Debug("snapshot unchanged, skipping insert")
		// Still evaluate transitions (e.g., debounce timer may expire)
		vs.evaluateTransitions(ctx)
		return
	}

	// Insert position
	posID, err := InsertPosition(ctx, vs.db, snap)
	if err != nil {
		slog.Error("failed to insert position", "error", err)
		return
	}
	vs.LastPositionID = posID
	vs.LastSnapshot = snap
	vs.HasSnapshot = true

	slog.Debug("position inserted", "id", posID, "state", carState, "soc", vs.BatteryLevel, "speed", vs.Speed)

	// Evaluate state transitions
	vs.evaluateTransitions(ctx)
}

func (vs *VehicleState) determineCarState() string {
	if vs.Running && vs.Speed > 0 {
		return "driving"
	}
	if vs.Charging {
		return "charging"
	}
	if vs.Available == "online" {
		return "online"
	}
	return "offline"
}

func (vs *VehicleState) evaluateTransitions(ctx context.Context) {
	now := time.Now()

	switch vs.CurrentState {
	case StateParked:
		// Parked -> Driving
		if vs.Running && vs.Speed > 0 {
			vs.transitionToDriving(ctx, now)
			return
		}
		// Parked -> Charging
		if vs.Charging {
			vs.transitionToCharging(ctx, now)
			return
		}

	case StateDriving:
		// Driving -> Parked (immediate: engine off)
		if !vs.Running {
			vs.transitionToParked(ctx, now)
			return
		}
		// Driving -> Parked (debounced: speed == 0 but still running)
		if vs.Speed == 0 {
			if vs.SpeedZeroSince == nil {
				t := now
				vs.SpeedZeroSince = &t
				slog.Info("speed zero, starting debounce timer")
			} else {
				elapsed := now.Sub(*vs.SpeedZeroSince)
				debounce := time.Duration(vs.cfg.DriveEndDebounceSeconds) * time.Second
				if elapsed >= debounce {
					slog.Info("speed zero debounce expired, ending drive", "elapsed_seconds", int(elapsed.Seconds()))
					vs.transitionToParked(ctx, *vs.SpeedZeroSince)
					return
				}
			}
		} else {
			// Speed > 0 again, reset debounce
			if vs.SpeedZeroSince != nil {
				slog.Debug("speed resumed, clearing debounce timer")
				vs.SpeedZeroSince = nil
			}
		}

	case StateCharging:
		// Charging -> Parked
		if !vs.Charging {
			vs.transitionToParked(ctx, now)
			return
		}
	}
}

func (vs *VehicleState) transitionToDriving(ctx context.Context, now time.Time) {
	driveID, err := StartDrive(ctx, vs.db, now, vs.Odometer, vs.BatteryLevel, vs.LastPositionID)
	if err != nil {
		slog.Error("failed to start drive", "error", err)
		return
	}
	vs.ActiveDriveID = &driveID
	vs.SpeedZeroSince = nil
	vs.CurrentState = StateDriving
	slog.Info("state transition", "from", "parked", "to", "driving", "drive_id", driveID)
}

func (vs *VehicleState) transitionToCharging(ctx context.Context, now time.Time) {
	chargeID, err := StartCharge(ctx, vs.db, now, vs.BatteryLevel, vs.LastPositionID)
	if err != nil {
		slog.Error("failed to start charge", "error", err)
		return
	}
	vs.ActiveChargeID = &chargeID
	vs.ChargeStartKwh = vs.BatteryKwh
	vs.CurrentState = StateCharging
	slog.Info("state transition", "from", "parked", "to", "charging", "charge_id", chargeID)
}

func (vs *VehicleState) transitionToParked(ctx context.Context, endTime time.Time) {
	prevState := vs.CurrentState

	if vs.ActiveDriveID != nil {
		driveID := *vs.ActiveDriveID
		err := EndDrive(ctx, vs.db, driveID, endTime, vs.Odometer, vs.BatteryLevel, vs.LastPositionID)
		if err != nil {
			slog.Error("failed to end drive", "error", err)
		} else if vs.osrm != nil {
			// Run OSRM map matching asynchronously so it doesn't block state transitions
			pool := vs.db
			osrm := vs.osrm
			go func() {
				matchCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				if err := MatchAndStoreDrive(matchCtx, pool, osrm, driveID); err != nil {
					slog.Error("failed to map-match drive", "drive_id", driveID, "error", err)
				}
			}()
		}
		slog.Info("state transition", "from", "driving", "to", "parked", "drive_id", driveID)
		vs.ActiveDriveID = nil
	}

	if vs.ActiveChargeID != nil {
		energyAdded := vs.BatteryKwh - vs.ChargeStartKwh
		err := EndCharge(ctx, vs.db, *vs.ActiveChargeID, endTime, vs.BatteryLevel, energyAdded, vs.LastPositionID)
		if err != nil {
			slog.Error("failed to end charge", "error", err)
		}
		slog.Info("state transition", "from", "charging", "to", "parked", "charge_id", *vs.ActiveChargeID)
		vs.ActiveChargeID = nil
	}

	vs.SpeedZeroSince = nil
	vs.CurrentState = StateParked
	_ = prevState // used for logging above
}

// --- Parsing helpers ---

func parseFloat(s string) float64 {
	s = strings.TrimSpace(s)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		slog.Debug("failed to parse float", "value", s, "error", err)
		return 0
	}
	return f
}

func parseInt(s string) int {
	s = strings.TrimSpace(s)
	i, err := strconv.Atoi(s)
	if err != nil {
		slog.Debug("failed to parse int", "value", s, "error", err)
		return 0
	}
	return i
}

// parseBool handles Python-style True/False as published by the SAIC gateway.
func parseBool(s string) bool {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}
