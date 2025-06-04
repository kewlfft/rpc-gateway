package rpcgateway

import (
	"github.com/kewlfft/rpc-gateway/internal/metrics"
	"github.com/kewlfft/rpc-gateway/internal/proxy"
)

type RPCGatewayConfig struct { //nolint:revive
	Metrics metrics.Config `yaml:"metrics"`
	Port    string        `yaml:"port"`
	Proxies []ProxyConfig `yaml:"proxies"`
}

type ProxyConfig struct {
	Path            string                    `yaml:"path"`
	UpstreamTimeout string                    `yaml:"upstreamTimeout"`
	HealthChecks    proxy.HealthCheckConfig   `yaml:"healthChecks"`
	Targets         []proxy.NodeProviderConfig `yaml:"targets"`
}
