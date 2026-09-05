package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// BackendConfig describes one backend as declared in config.json.
type BackendConfig struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Config is the full load balancer configuration.
// Every field has a sane default applied in Load(), so config.json
// can be as small as {"backends":[...]}.
type Config struct {
	ListenAddr string          `json:"listen_addr"`
	Backends   []BackendConfig `json:"backends"`

	HealthCheckPath       string `json:"health_check_path"`
	HealthCheckIntervalMs int    `json:"health_check_interval_ms"`
	HealthCheckTimeoutMs  int    `json:"health_check_timeout_ms"`
	UnhealthyThreshold    int    `json:"unhealthy_threshold"` // consecutive failures -> DOWN
	HealthyThreshold      int    `json:"healthy_threshold"`   // consecutive successes -> UP

	// Performance-based routing knobs.
	MaxActiveRequests int     `json:"max_active_requests"` // concurrency threshold per backend
	MaxLatencyMs      float64 `json:"max_latency_ms"`      // EWMA latency threshold per backend
	EwmaAlpha         float64 `json:"ewma_alpha"`          // smoothing factor for latency EWMA

	RequestTimeoutMs int `json:"request_timeout_ms"` // per upstream attempt
	MaxRetries       int `json:"max_retries"`        // extra backends to try on failure

	AdminToken string `json:"admin_token"` // optional bearer token to protect /stats
}

func defaultConfig() Config {
	return Config{
		ListenAddr:            ":8080",
		HealthCheckPath:       "/health",
		HealthCheckIntervalMs: 2000,
		HealthCheckTimeoutMs:  1500,
		UnhealthyThreshold:    3,
		HealthyThreshold:      2,
		MaxActiveRequests:     20,
		MaxLatencyMs:          300,
		EwmaAlpha:             0.3,
		RequestTimeoutMs:      4000,
		MaxRetries:            2,
	}
}

func LoadConfig(path string) (Config, error) {
	cfg := defaultConfig()

	f, err := os.Open(path)
	if err != nil {
		return cfg, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	// Decode into a struct with the same shape but we merge onto defaults
	// by only overwriting fields explicitly present. Simplicity over
	// cleverness: decode into cfg directly, but preserve defaults for any
	// zero-value numeric field the user didn't set by decoding twice.
	raw := defaultConfig()
	if err := json.NewDecoder(f).Decode(&raw); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	if len(raw.Backends) == 0 {
		return cfg, fmt.Errorf("config must declare at least one backend")
	}
	return raw, nil
}

func (c Config) healthInterval() time.Duration {
	return time.Duration(c.HealthCheckIntervalMs) * time.Millisecond
}
func (c Config) healthTimeout() time.Duration {
	return time.Duration(c.HealthCheckTimeoutMs) * time.Millisecond
}
func (c Config) requestTimeout() time.Duration {
	return time.Duration(c.RequestTimeoutMs) * time.Millisecond
}
