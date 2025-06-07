package proxy

import (
	"log/slog"
	"time"
)

// HealthCheckConfig defines the health check parameters
type HealthCheckConfig struct {
	Interval           time.Duration `yaml:"interval"`
	BlockDiffThreshold uint64       `yaml:"blockDiffThreshold"`
}

// NodeProviderConfig defines the configuration for a node provider
type NodeProviderConfig struct {
	Name       string `yaml:"name"`
	Connection struct {
		HTTP struct {
			URL         string `yaml:"url"`
			Compression bool   `yaml:"compression"`
		} `yaml:"http"`
	} `yaml:"connection"`
}

// Config defines the configuration for a proxy
type Config struct {
	Path            string            `yaml:"path"`
	ChainType       string            `yaml:"chainType,omitempty"` // Default chain type for all targets
	Timeout         time.Duration     `yaml:"timeout"`
	HealthChecks    HealthCheckConfig `yaml:"healthChecks"`
	Targets         []NodeProviderConfig `yaml:"targets"`
	Logger          *slog.Logger
	DisableHealthChecks bool          // If true, health checks will not be started
}
