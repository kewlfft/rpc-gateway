package rpcgateway

import (
	"github.com/kewlfft/rpc-gateway/internal/metrics"
	"github.com/kewlfft/rpc-gateway/internal/proxy"
)

// RPCGatewayConfig defines the top-level configuration for the RPC gateway
type RPCGatewayConfig struct {
	// Metrics server configuration
	Metrics metrics.Config `yaml:"metrics"`
	// Port to listen on for RPC requests
	Port string `yaml:"port"`
	// If true, providers will be randomized at startup
	RandomizeProviders bool `yaml:"randomizeProviders"`
	// List of proxy configurations
	Proxies []ProxyConfig `yaml:"proxies"`
}

// ProxyConfig defines the configuration for a single proxy instance
type ProxyConfig struct {
	// Path prefix for this proxy (e.g., "eth", "bsc")
	Path string `yaml:"path"`
	// Chain type (e.g., "evm", "solana", "tron")
	ChainType string `yaml:"chainType,omitempty"`
	// Request timeout for upstream calls
	Timeout string `yaml:"timeout"`
	// Health check configuration
	HealthChecks proxy.HealthCheckConfig `yaml:"healthChecks"`
	// List of RPC providers to use
	Targets []proxy.NodeProviderConfig `yaml:"targets"`
}
