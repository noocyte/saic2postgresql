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
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	pool, err := InitDB(ctx, cfg.DBURI)
	if err != nil {
		slog.Error("failed to initialize database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	vs := NewVehicleState(pool, cfg)
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
