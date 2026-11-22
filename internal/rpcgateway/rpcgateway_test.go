package rpcgateway

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kewlfft/rpc-gateway/internal/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderRandomization(t *testing.T) {
	config := RPCGatewayConfig{
		Port: "3000",
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
	targets := gateway.proxies[path].GetTargets()
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.Name()
	}
	return names
}

func TestConfigRejectsUnknownKeys(t *testing.T) {
	write := func(content string) string {
		path := t.TempDir() + "/config.yml"
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}
	valid := `
port: "3000"
proxies:
  - path: eth
    timeout: 1s
    targets:
      - name: p1
        connection:
          http:
            url: http://p1
`
	gateway, err := NewRPCGatewayFromConfigFile(write(valid))
	require.NoError(t, err)
	assert.Equal(t, []string{"p1"}, getProviderOrder(gateway, "eth"))

	// The removed metrics section is now an error
	_, err = NewRPCGatewayFromConfigFile(write(valid + "metrics:\n  port: 9010\n"))
	assert.ErrorContains(t, err, "field metrics not found")

	// So is a typo in a nested key
	_, err = NewRPCGatewayFromConfigFile(write(strings.Replace(valid, "timeout:", "timout:", 1)))
	assert.ErrorContains(t, err, "field timout not found")
}
