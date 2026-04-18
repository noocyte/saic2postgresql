package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// HealthCheck tracks MQTT liveness and exposes an HTTP /healthz endpoint.
type HealthCheck struct {
	mu              sync.RWMutex
	lastMessageTime time.Time
	staleThreshold  time.Duration
	startTime       time.Time
}

// HealthResponse is returned as JSON from /healthz.
type HealthResponse struct {
	Status             string `json:"status"`
	LastMessageReceived string `json:"last_message_received"`
	SinceLastMessage   string `json:"since_last_message"`
	Uptime             string `json:"uptime"`
}

// NewHealthCheck creates a new health checker with the given stale threshold.
func NewHealthCheck(staleMinutes int) *HealthCheck {
	return &HealthCheck{
		staleThreshold: time.Duration(staleMinutes) * time.Minute,
		startTime:      time.Now(),
	}
}

// RecordMessage updates the last message timestamp.
func (h *HealthCheck) RecordMessage() {
	h.mu.Lock()
	h.lastMessageTime = time.Now()
	h.mu.Unlock()
}

// IsHealthy returns true if a message was received within the stale threshold.
// Also returns true if no messages have ever been received (still starting up).
func (h *HealthCheck) IsHealthy() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.lastMessageTime.IsZero() {
		// No messages yet — give it time to connect
		return time.Since(h.startTime) < h.staleThreshold
	}
	return time.Since(h.lastMessageTime) < h.staleThreshold
}

// ServeHTTP handles the /healthz endpoint.
func (h *HealthCheck) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	lastMsg := h.lastMessageTime
	h.mu.RUnlock()

	resp := HealthResponse{
		Uptime: time.Since(h.startTime).Truncate(time.Second).String(),
	}

	if lastMsg.IsZero() {
		resp.LastMessageReceived = "never"
		resp.SinceLastMessage = "n/a"
	} else {
		resp.LastMessageReceived = lastMsg.Format(time.RFC3339)
		resp.SinceLastMessage = time.Since(lastMsg).Truncate(time.Second).String()
	}

	if h.IsHealthy() {
		resp.Status = "ok"
		w.WriteHeader(http.StatusOK)
	} else {
		resp.Status = "stale"
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// StartHealthServer starts the HTTP health endpoint in a goroutine.
func (h *HealthCheck) StartHealthServer(ctx context.Context, port int) {
	mux := http.NewServeMux()
	mux.Handle("/healthz", h)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	go func() {
		slog.Info("health endpoint started", "port", port, "path", "/healthz")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("health server error", "error", err)
		}
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
}

// StartWatchdog periodically checks for stale MQTT and logs warnings.
func (h *HealthCheck) StartWatchdog(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.mu.RLock()
				lastMsg := h.lastMessageTime
				h.mu.RUnlock()

				if lastMsg.IsZero() {
					slog.Warn("watchdog: no MQTT messages received since startup",
						"uptime", time.Since(h.startTime).Truncate(time.Second).String())
				} else {
					silence := time.Since(lastMsg)
					if silence >= h.staleThreshold {
						slog.Warn("watchdog: no MQTT messages received, subscription may be dead",
							"since_last_message", silence.Truncate(time.Second).String(),
							"threshold", h.staleThreshold.String())
					} else {
						slog.Debug("watchdog: MQTT healthy",
							"since_last_message", silence.Truncate(time.Second).String())
					}
				}
			}
		}
	}()
}
