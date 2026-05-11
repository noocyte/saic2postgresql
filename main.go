package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	// Health check mode: saic-logger --health [port]
	if len(os.Args) > 1 && os.Args[1] == "--health" {
		port := "8080"
		if len(os.Args) > 2 {
			port = os.Args[2]
		}
		resp, err := http.Get(fmt.Sprintf("http://localhost:%s/healthz", port))
		if err != nil {
			os.Exit(1)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Structured JSON logging to stdout
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))

	cfg, err := LoadConfig()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	slog.Info("config loaded",
		"mqtt_broker", cfg.MQTTBroker,
		"mqtt_topic_prefix", cfg.MQTTTopicPrefix,
		"drive_end_debounce_seconds", cfg.DriveEndDebounceSeconds,
		"health_port", cfg.HealthPort,
		"stale_minutes", cfg.StaleMinutes,
		"osrm_url", cfg.OSRMURL,
	)

	// Backfill mode: saic-logger --backfill
	// Runs OSRM map matching on all historical drives that don't have matched positions.
	if len(os.Args) > 1 && os.Args[1] == "--backfill" {
		ctx := context.Background()

		if cfg.OSRMURL == "" {
			slog.Error("OSRM_URL must be set for backfill mode")
			os.Exit(1)
		}

		pool, err := InitDB(ctx, cfg.DBURI)
		if err != nil {
			slog.Error("failed to initialize database", "error", err)
			os.Exit(1)
		}
		defer pool.Close()

		osrm := NewOSRMClient(cfg.OSRMURL)

		drives, err := GetUnmatchedDrives(ctx, pool)
		if err != nil {
			slog.Error("failed to get unmatched drives", "error", err)
			os.Exit(1)
		}

		if len(drives) == 0 {
			slog.Info("no unmatched drives found, nothing to do")
			os.Exit(0)
		}

		slog.Info("starting backfill", "unmatched_drives", len(drives))
		succeeded, failed := 0, 0
		for _, driveID := range drives {
			if err := MatchAndStoreDrive(ctx, pool, osrm, driveID); err != nil {
				slog.Error("backfill failed for drive", "drive_id", driveID, "error", err)
				failed++
			} else {
				succeeded++
			}
		}
		slog.Info("backfill complete", "succeeded", succeeded, "failed", failed, "total", len(drives))
		os.Exit(0)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	pool, err := InitDB(ctx, cfg.DBURI)
	if err != nil {
		slog.Error("failed to initialize database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	// Create OSRM client (nil if OSRM_URL is not set)
	osrm := NewOSRMClient(cfg.OSRMURL)
	if osrm != nil {
		slog.Info("OSRM map matching enabled", "url", cfg.OSRMURL)
	} else {
		slog.Info("OSRM map matching disabled (OSRM_URL not set)")
	}

	vs := NewVehicleState(pool, cfg, osrm)
	if err := vs.Recover(ctx); err != nil {
		slog.Warn("state recovery encountered an issue (continuing)", "error", err)
	}

	// Start health monitoring
	hc := NewHealthCheck(cfg.StaleMinutes)
	hc.StartHealthServer(ctx, cfg.HealthPort)
	hc.StartWatchdog(ctx, 5*time.Minute)

	slog.Info("starting MQTT connection")
	if err := StartMQTT(ctx, cfg, vs, hc); err != nil {
		if ctx.Err() != nil {
			slog.Info("shutting down gracefully")
		} else {
			slog.Error("mqtt error", "error", err)
			os.Exit(1)
		}
	}

	slog.Info("shutdown complete")
}
