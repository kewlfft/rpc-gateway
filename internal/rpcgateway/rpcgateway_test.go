package rpcgateway

import (
	"testing"
	"time"

	"github.com/kewlfft/rpc-gateway/internal/metrics"
	"github.com/kewlfft/rpc-gateway/internal/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRandomizeProviders(t *testing.T) {
	// Create a test configuration
	config := RPCGatewayConfig{
		Port: "3000",
		Metrics: metrics.Config{
			Port: 9010,
		},
		Proxies: []ProxyConfig{
			{
				Path: "test",
				UpstreamTimeout: "1s",
				HealthChecks: proxy.HealthCheckConfig{
					Interval: time.Second,
					Timeout:  time.Second,
				},
				Targets: []proxy.NodeProviderConfig{
					{
						Name: "provider1",
						Connection: struct {
							HTTP struct {
								URL string `yaml:"url"`
							} `yaml:"http"`
						}{
							HTTP: struct {
								URL string `yaml:"url"`
							}{
								URL: "http://provider1",
							},
						},
					},
					{
						Name: "provider2",
						Connection: struct {
							HTTP struct {
								URL string `yaml:"url"`
							} `yaml:"http"`
						}{
							HTTP: struct {
								URL string `yaml:"url"`
							}{
								URL: "http://provider2",
							},
						},
					},
					{
						Name: "provider3",
						Connection: struct {
							HTTP struct {
								URL string `yaml:"url"`
							} `yaml:"http"`
						}{
							HTTP: struct {
								URL string `yaml:"url"`
							}{
								URL: "http://provider3",
							},
						},
					},
				},
			},
		},
	}

	t.Run("randomization from config", func(t *testing.T) {
		// Test with randomization enabled in config
		config.RandomizeProviders = true
		gateway, err := NewRPCGateway(config)
		require.NoError(t, err)

		// Get initial order of providers
		initialOrder := getProviderOrder(gateway, "test")
		require.Len(t, initialOrder, 3)

		// Create a new gateway with the same config
		gateway2, err := NewRPCGateway(config)
		require.NoError(t, err)

		// Get order of providers in the new gateway
		newOrder := getProviderOrder(gateway2, "test")
		require.Len(t, newOrder, 3)

		// Verify that the orders are different
		assert.NotEqual(t, initialOrder, newOrder, "Providers should be in different orders")
	})

	t.Run("randomization from CLI flag", func(t *testing.T) {
		// Test with randomization disabled in config
		config.RandomizeProviders = false
		gateway, err := NewRPCGateway(config)
		require.NoError(t, err)

		// Get initial order of providers
		initialOrder := getProviderOrder(gateway, "test")
		require.Len(t, initialOrder, 3)

		// Enable randomization via CLI flag
		gateway.SetRandomizeProviders(true)

		// Get new order of providers
		newOrder := getProviderOrder(gateway, "test")
		require.Len(t, newOrder, 3)

		// Verify that the orders are different
		assert.NotEqual(t, initialOrder, newOrder, "Providers should be in different orders after CLI flag")
	})

	t.Run("no randomization", func(t *testing.T) {
		// Test with randomization disabled
		config.RandomizeProviders = false
		gateway, err := NewRPCGateway(config)
		require.NoError(t, err)

		// Get initial order of providers
		initialOrder := getProviderOrder(gateway, "test")
		require.Len(t, initialOrder, 3)

		// Create a new gateway with the same config
		gateway2, err := NewRPCGateway(config)
		require.NoError(t, err)

		// Get order of providers in the new gateway
		newOrder := getProviderOrder(gateway2, "test")
		require.Len(t, newOrder, 3)

		// Verify that the orders are the same
		assert.Equal(t, initialOrder, newOrder, "Providers should be in the same order when randomization is disabled")
	})
}

// getProviderOrder returns the order of providers for a given path
func getProviderOrder(gateway *RPCGateway, path string) []string {
	proxy := gateway.proxies[path]
	// Use the public interface to get provider names
	order := make([]string, 0)
	for _, target := range proxy.GetTargets() {
		order = append(order, target.Name())
	}
	return order
} 