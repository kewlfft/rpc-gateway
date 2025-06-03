package proxy

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/caitlinelfring/go-env-default"
	"github.com/stretchr/testify/assert"
)

// TestBasicHealthchecker checks if it runs with default options.
func TestBasicHealthchecker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

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

	healthchecker.Start(ctx)

	// Wait for a health check cycle
	time.Sleep(2 * time.Second)

	assert.NotZero(t, healthchecker.BlockNumber())
	assert.True(t, healthchecker.IsHealthy())

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
	healthchecker.Taint()
	assert.True(t, healthchecker.IsTainted())
	assert.False(t, healthchecker.IsHealthy())

	// Test taint removal
	healthchecker.RemoveTaint()
	assert.False(t, healthchecker.IsTainted())
	assert.True(t, healthchecker.IsHealthy())

	// Test multiple taints with exponential backoff
	healthchecker.Taint()
	firstWaitTime := healthchecker.taint.waitTime
	healthchecker.RemoveTaint()

	// Taint again within reset period
	healthchecker.Taint()
	secondWaitTime := healthchecker.taint.waitTime
	assert.Greater(t, secondWaitTime, firstWaitTime)

	// Test max wait time
	for i := 0; i < 5; i++ {
		healthchecker.Taint()
		healthchecker.RemoveTaint()
	}
	healthchecker.Taint()
	finalWaitTime := healthchecker.taint.waitTime
	assert.LessOrEqual(t, finalWaitTime, maxTaintWaitTime)
}
