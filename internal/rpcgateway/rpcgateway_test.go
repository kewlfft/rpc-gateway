package rpcgateway

import (
	"slices"
	"testing"
	"time"

	"github.com/kewlfft/rpc-gateway/internal/metrics"
	"github.com/kewlfft/rpc-gateway/internal/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderRandomization(t *testing.T) {
	config := RPCGatewayConfig{
		Port:    "3000",
		Metrics: metrics.Config{Port: 9010},
		Proxies: []ProxyConfig{
			{
				Path:    "test",
				Timeout: "1s",
				HealthChecks: proxy.HealthCheckConfig{
					Interval:           time.Second,
					BlockDiffThreshold: 2,
				},
				Targets: []proxy.NodeProviderConfig{
					{Name: "provider1", Connection: proxy.ConnectionConfig{
						HTTP: proxy.HTTPConnection{URL: "http://provider1"},
					}},
					{Name: "provider2", Connection: proxy.ConnectionConfig{
						HTTP: proxy.HTTPConnection{URL: "http://provider2"},
					}},
					{Name: "provider3", Connection: proxy.ConnectionConfig{
						HTTP: proxy.HTTPConnection{URL: "http://provider3"},
					}},
				},
			},
		},
	}

	initialOrder := []string{"provider1", "provider2", "provider3"}

	changed := false
	for range 10 {
		gateway, err := NewRPCGateway(config)
		require.NoError(t, err)
		require.Equal(t, initialOrder, getProviderOrder(gateway, "test"))
		gateway.SetRandomizeProviders(true)
		if !slices.Equal(initialOrder, getProviderOrder(gateway, "test")) {
			changed = true
			break
		}
	}
	assert.True(t, changed, "Provider order should change after randomization")
}

func TestSetRandomizeProvidersShufflesOnlyOnce(t *testing.T) {
	config := RPCGatewayConfig{
		Port:               "3000",
		RandomizeProviders: true, // already shuffled by the constructor
		Proxies: []ProxyConfig{{
			Path:    "test",
			Timeout: "1s",
			Targets: []proxy.NodeProviderConfig{
				{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}, {Name: "e"},
			},
		}},
	}
	gateway, err := NewRPCGateway(config)
	require.NoError(t, err)
	order := getProviderOrder(gateway, "test")
	for range 5 {
		gateway.SetRandomizeProviders(true)
		assert.Equal(t, order, getProviderOrder(gateway, "test"), "already-randomized providers must not be reshuffled")
	}
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
