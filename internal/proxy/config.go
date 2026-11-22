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

// HTTPConnection is the HTTP RPC endpoint config for a provider.
type HTTPConnection struct {
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
}

// WebSocketConnection is the WebSocket RPC endpoint config for a provider.
type WebSocketConnection struct {
	URL string `yaml:"url"`
}

// ConnectionConfig holds HTTP and/or WebSocket endpoints for a provider.
// Named types (vs anonymous nested structs) keep YAML tags clear and allow
// Go 1.27 promoted-field struct literals if these are embedded later.
type ConnectionConfig struct {
	HTTP      HTTPConnection      `yaml:"http"`
	WebSocket WebSocketConnection `yaml:"websocket"`
}

// NodeProviderConfig defines the configuration for a single RPC provider
type NodeProviderConfig struct {
	Name       string           `yaml:"name"`
	Connection ConnectionConfig `yaml:"connection"`
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
	// Index of the path for incremental staggering of health checks
	PathIndex int
	// When false, skip Prometheus recording on the request hot path
	MetricsEnabled bool
}
