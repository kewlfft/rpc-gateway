package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/caitlinelfring/go-env-default"
	"github.com/stretchr/testify/assert"
)

// TestBasicHealthchecker checks if it runs with default options.
func TestBasicHealthchecker(t *testing.T) {
	// Create a mock server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			// Mock eth_blockNumber response
			if r.Method == "POST" {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1234"}`))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	healtcheckConfig := HealthCheckerConfig{
		URL:              server.URL,
		Name:             "test",
		Interval:         time.Millisecond * 100, // Much shorter interval for testing
		Timeout:          time.Second,
		FailureThreshold: 1,
		SuccessThreshold: 1,
		Logger:           slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	healthchecker, err := NewHealthChecker(healtcheckConfig)
	assert.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	healthchecker.Start(ctx)

	// Wait for a health check cycle
	time.Sleep(time.Millisecond * 150)

	assert.NotZero(t, healthchecker.BlockNumber())
	assert.True(t, healthchecker.IsHealthy())

	// Test unhealthy states
	healthchecker.blockNumber = 0
	assert.False(t, healthchecker.IsHealthy())

	healthchecker.blockNumber = 1
	healthchecker.gasLeft = 0
	assert.False(t, healthchecker.IsHealthy())

	healthchecker.blockNumber = 1
	healthchecker.gasLeft = 1
	assert.True(t, healthchecker.IsHealthy())
}

func TestHealthCheckerTaint(t *testing.T) {
	healtcheckConfig := HealthCheckerConfig{
		URL:              env.GetDefault("RPC_GATEWAY_NODE_URL_1", "https://ethereum.publicnode.com"),
		Interval:         1 * time.Second,
		Timeout:          2 * time.Second,
		FailureThreshold: 1,
		SuccessThreshold: 1,
		Logger:           slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	healthchecker, err := NewHealthChecker(healtcheckConfig)
	assert.NoError(t, err)

	// Test initial state
	assert.False(t, healthchecker.IsTainted())
	assert.True(t, healthchecker.IsHealthy())

	// Test tainting
	healthchecker.TaintHealthCheck()
	assert.True(t, healthchecker.IsTainted())
	assert.False(t, healthchecker.IsHealthy())

	// Test taint removal
	healthchecker.RemoveTaint()
	assert.False(t, healthchecker.IsTainted())
	assert.True(t, healthchecker.IsHealthy())

	// Test multiple taints with exponential backoff
	healthchecker.TaintHealthCheck()
	firstWaitTime := healthchecker.taint.waitTime
	healthchecker.RemoveTaint()

	// Taint again within reset period
	healthchecker.TaintHealthCheck()
	secondWaitTime := healthchecker.taint.waitTime
	assert.Greater(t, secondWaitTime, firstWaitTime)

	// Test max wait time
	for i := 0; i < 5; i++ {
		healthchecker.TaintHealthCheck()
		healthchecker.RemoveTaint()
	}
	healthchecker.TaintHealthCheck()
	finalWaitTime := healthchecker.taint.waitTime
	assert.LessOrEqual(t, finalWaitTime, healthCheckTaintConfig.MaxWaitTime)
}

func TestHealthCheckerTaintHTTP(t *testing.T) {
	healthchecker, err := NewHealthChecker(HealthCheckerConfig{
		URL:    "http://localhost:8545",
		Name:   "test",
		Logger: slog.Default(),
	})
	assert.NoError(t, err)

	// Test initial state
	assert.False(t, healthchecker.IsTainted())
	assert.True(t, healthchecker.IsHealthy())

	// Test HTTP taint
	healthchecker.TaintHTTP()
	assert.True(t, healthchecker.IsTainted())
	assert.False(t, healthchecker.IsHealthy())

	// Test taint removal
	healthchecker.RemoveTaint()
	assert.False(t, healthchecker.IsTainted())
	assert.True(t, healthchecker.IsHealthy())

	// Test multiple HTTP taints with exponential backoff
	healthchecker.TaintHTTP()
	firstWaitTime := healthchecker.taint.waitTime
	healthchecker.RemoveTaint()

	// Taint again within reset period
	healthchecker.TaintHTTP()
	secondWaitTime := healthchecker.taint.waitTime
	assert.Greater(t, secondWaitTime, firstWaitTime)

	// Test max wait time
	for i := 0; i < 5; i++ {
		healthchecker.TaintHTTP()
		healthchecker.RemoveTaint()
	}
	healthchecker.TaintHTTP()
	finalWaitTime := healthchecker.taint.waitTime
	assert.LessOrEqual(t, finalWaitTime, httpTaintConfig.MaxWaitTime)
}
