package rpcgateway

import (
	"github.com/kewlfft/rpc-gateway/internal/metrics"
	"github.com/kewlfft/rpc-gateway/internal/proxy"
)

type RPCGatewayConfig struct { //nolint:revive
	Metrics            metrics.Config `yaml:"metrics"`
	Port              string        `yaml:"port"`
	RandomizeProviders bool         `yaml:"randomizeProviders"` // If true, providers will be randomized at startup
	Proxies           []ProxyConfig `yaml:"proxies"`
}

type ProxyConfig struct {
	Path            string                    `yaml:"path"`
	ChainType       string                    `yaml:"chainType,omitempty"` // Default chain type for all targets in this proxy
	UpstreamTimeout string                    `yaml:"upstreamTimeout"`
	HealthChecks    proxy.HealthCheckConfig   `yaml:"healthChecks"`
	Targets         []proxy.NodeProviderConfig `yaml:"targets"`
}
