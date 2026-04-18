package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
)

// StartMQTT connects to the MQTT broker, subscribes to the vehicle topic tree,
// routes incoming messages to the VehicleState, and blocks until ctx is cancelled.
func StartMQTT(ctx context.Context, cfg *Config, vs *VehicleState, hc *HealthCheck) error {
	serverURL, err := url.Parse(cfg.MQTTBroker)
	if err != nil {
		return fmt.Errorf("parsing MQTT broker URL: %w", err)
	}

	topicFilter := cfg.MQTTTopicPrefix + "/#"
	prefix := cfg.MQTTTopicPrefix + "/"

	cliCfg := autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{serverURL},
		KeepAlive:                     30,
		CleanStartOnInitialConnection: true,
		SessionExpiryInterval:         0,
		OnConnectionUp: func(cm *autopaho.ConnectionManager, connAck *paho.Connack) {
			slog.Info("mqtt connected, subscribing", "topic", topicFilter)
			if _, err := cm.Subscribe(ctx, &paho.Subscribe{
				Subscriptions: []paho.SubscribeOptions{
					{Topic: topicFilter, QoS: 0},
				},
			}); err != nil {
				slog.Error("mqtt subscribe failed", "error", err)
				return
			}
			slog.Info("mqtt subscribed successfully", "topic", topicFilter)
		},
		OnConnectError: func(err error) {
			slog.Error("mqtt connection error", "error", err)
		},
		ClientConfig: paho.ClientConfig{
			ClientID: "saic-postgresql-logger",
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){
				func(pr paho.PublishReceived) (bool, error) {
					topic := pr.Packet.Topic
					payload := string(pr.Packet.Payload)

					// Only process messages under our prefix
					if !strings.HasPrefix(topic, prefix) {
						return true, nil
					}

					// Record that we received a message
					hc.RecordMessage()

					suffix := strings.TrimPrefix(topic, prefix)

					// Check if this is the snapshot trigger
					if suffix == "refresh/lastVehicleState" {
						slog.Debug("snapshot trigger received", "timestamp", payload)
						vs.EvaluateSnapshot(ctx)
						return true, nil
					}

					// Update field in vehicle state
					vs.UpdateField(suffix, payload)
					return true, nil
				},
			},
		},
	}

	cm, err := autopaho.NewConnection(ctx, cliCfg)
	if err != nil {
		return fmt.Errorf("creating MQTT connection: %w", err)
	}

	slog.Info("waiting for MQTT connection", "broker", cfg.MQTTBroker)
	if err := cm.AwaitConnection(ctx); err != nil {
		return fmt.Errorf("waiting for MQTT connection: %w", err)
	}

	// Block until context is cancelled (signal received)
	<-ctx.Done()

	// Disconnect cleanly
	disconnectCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cm.Disconnect(disconnectCtx); err != nil {
		slog.Warn("mqtt disconnect error", "error", err)
	}

	return nil
}
