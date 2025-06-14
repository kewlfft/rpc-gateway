package proxy

import (
	"log/slog"
	"time"
)

// HealthCheckConfig defines the health check parameters for a proxy
type HealthCheckConfig struct {
	// Interval between health checks
	Interval time.Duration `yaml:"interval"`
	// Maximum allowed block difference between providers before tainting
	BlockDiffThreshold uint64 `yaml:"blockDiffThreshold"`
}

// NodeProviderConfig defines the configuration for a single RPC provider
type NodeProviderConfig struct {
	// Name of the provider (e.g., "infura", "alchemy")
	Name string `yaml:"name"`
	// Connection details for the provider
	Connection struct {
		// HTTP connection details
		HTTP struct {
			// RPC endpoint URL
			URL string `yaml:"url"`
			// Optional API key for the provider
			APIKey string `yaml:"apiKey"`
		} `yaml:"http"`
		// WebSocket connection details
		WebSocket struct {
			// WebSocket endpoint URL
			URL string `yaml:"url"`
		} `yaml:"websocket"`
	} `yaml:"connection"`
}

// Config defines the configuration for a proxy instance
type Config struct {
	// Path prefix for this proxy (e.g., "eth", "bsc")
	Path string `yaml:"path"`
	// Chain type (e.g., "evm", "solana", "tron")
	ChainType string `yaml:"chainType,omitempty"`
	// Request timeout for upstream calls
	Timeout time.Duration `yaml:"timeout"`
	// Health check configuration
	HealthChecks HealthCheckConfig `yaml:"healthChecks"`
	// List of RPC providers to use
	Targets []NodeProviderConfig `yaml:"targets"`
	// Logger instance
	Logger *slog.Logger
	// If true, health checks will not be started
	DisableHealthChecks bool
	// Index of the path for incremental staggering of health checks
	PathIndex int
}
