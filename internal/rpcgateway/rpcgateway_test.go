package rpcgateway

import (
	"testing"
	"time"

	"github.com/kewlfft/rpc-gateway/internal/metrics"
	"github.com/kewlfft/rpc-gateway/internal/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderRandomization(t *testing.T) {
	config := RPCGatewayConfig{
		Port: "3000",
		Metrics: metrics.Config{Port: 9010},
		Proxies: []ProxyConfig{
			{
				Path: "test",
				Timeout: "1s",
				HealthChecks: proxy.HealthCheckConfig{
					Interval: time.Second,
					BlockDiffThreshold: 2,
				},
				Targets: []proxy.NodeProviderConfig{
					{Name: "provider1", Connection: struct {
						HTTP struct {
							URL    string `yaml:"url"`
							APIKey string `yaml:"apiKey"`
						} `yaml:"http"`
						WebSocket struct {
							URL string `yaml:"url"`
						} `yaml:"websocket"`
					}{
						HTTP: struct {
							URL    string `yaml:"url"`
							APIKey string `yaml:"apiKey"`
						}{URL: "http://provider1"},
					}},
					{Name: "provider2", Connection: struct {
						HTTP struct {
							URL    string `yaml:"url"`
							APIKey string `yaml:"apiKey"`
						} `yaml:"http"`
						WebSocket struct {
							URL string `yaml:"url"`
						} `yaml:"websocket"`
					}{
						HTTP: struct {
							URL    string `yaml:"url"`
							APIKey string `yaml:"apiKey"`
						}{URL: "http://provider2"},
					}},
					{Name: "provider3", Connection: struct {
						HTTP struct {
							URL    string `yaml:"url"`
							APIKey string `yaml:"apiKey"`
						} `yaml:"http"`
						WebSocket struct {
							URL string `yaml:"url"`
						} `yaml:"websocket"`
					}{
						HTTP: struct {
							URL    string `yaml:"url"`
							APIKey string `yaml:"apiKey"`
						}{URL: "http://provider3"},
					}},
				},
			},
		},
	}

	gateway, err := NewRPCGateway(config)
	require.NoError(t, err)
	initialOrder := getProviderOrder(gateway, "test")

	changed := false
	for i := 0; i < 10; i++ {
		gateway.SetRandomizeProviders(true)
		newOrder := getProviderOrder(gateway, "test")
		if !equalStringSlices(initialOrder, newOrder) {
			changed = true
			break
		}
	}
	assert.True(t, changed, "Provider order should change after randomization")
}

func getProviderOrder(gateway *RPCGateway, path string) []string {
	proxy := gateway.proxies[path].(*proxy.Proxy)
	targets := proxy.GetTargets()
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.Name()
	}
	return names
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
} 