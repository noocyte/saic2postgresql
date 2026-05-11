package main

import (
	"fmt"
	"strings"

	"github.com/caarlos0/env/v11"
)

// Config holds all configuration parsed from environment variables.
type Config struct {
	MQTTBroker              string `env:"MQTT_BROKER" envDefault:"tcp://mqtt:1883"`
	MQTTTopicPrefix         string `env:"MQTT_TOPIC_PREFIX,required"`
	DBURI                   string `env:"DB_URI,required"`
	DriveEndDebounceSeconds int    `env:"DRIVE_END_DEBOUNCE_SECONDS" envDefault:"180"`
	HealthPort              int    `env:"HEALTH_PORT" envDefault:"8080"`
	StaleMinutes            int    `env:"STALE_MINUTES" envDefault:"30"`
	OSRMURL                 string `env:"OSRM_URL" envDefault:""`
}

// LoadConfig parses environment variables into a Config struct.
func LoadConfig() (*Config, error) {
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	// Ensure prefix doesn't end with /
	cfg.MQTTTopicPrefix = strings.TrimRight(cfg.MQTTTopicPrefix, "/")
	return cfg, nil
}
