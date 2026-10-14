// Package config reads the process configuration from the environment.
//
// Everything is environment-only. No secret, and no credential file, ever lives
// inside this repository.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	DatabaseURL  string
	Interval     time.Duration
	ListenAddr   string
	AllowOrigins []string
	RunOnce      bool
	DryRun       bool
}

func Load() (*Config, error) {
	cfg := &Config{
		DatabaseURL: os.Getenv("FIREBASE_DATABASE_URL"),
		ListenAddr:  envOr("LISTEN_ADDR", ":8080"),
	}

	hours, err := strconv.Atoi(envOr("SYNC_INTERVAL_HOURS", "12"))
	if err != nil || hours <= 0 {
		return nil, fmt.Errorf("SYNC_INTERVAL_HOURS must be a positive integer")
	}
	cfg.Interval = time.Duration(hours) * time.Hour

	for _, arg := range os.Args[1:] {
		switch arg {
		case "--once":
			cfg.RunOnce = true
		case "--dry-run":
			cfg.DryRun = true
		default:
			return nil, fmt.Errorf("unknown argument %q", arg)
		}
	}

	if raw := os.Getenv("ALLOW_ORIGINS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.AllowOrigins); err != nil {
			return nil, fmt.Errorf("ALLOW_ORIGINS must be a JSON array of strings: %w", err)
		}
	}

	return cfg, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
