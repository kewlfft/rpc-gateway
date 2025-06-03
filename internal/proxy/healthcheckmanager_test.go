package proxy

import (
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBlockLagAndTaint(t *testing.T) {
	// Create test health checkers
	hc1, err := NewHealthChecker(HealthCheckerConfig{
		URL:    "http://test1",
		Name:   "test1",
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	assert.NoError(t, err)

	hc2, err := NewHealthChecker(HealthCheckerConfig{
		URL:    "http://test2",
		Name:   "test2",
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	assert.NoError(t, err)

	// Create health check manager with test config
	hcm, err := NewHealthCheckManager(HealthCheckManagerConfig{
		Targets: []NodeProviderConfig{
			{Name: "test1", Connection: NodeProviderConnectionConfig{HTTP: NodeProviderConnectionHTTPConfig{URL: "http://test1"}}},
			{Name: "test2", Connection: NodeProviderConnectionConfig{HTTP: NodeProviderConnectionHTTPConfig{URL: "http://test2"}}},
		},
		Config: HealthCheckConfig{
			BlockDiffThreshold: 2,
		},
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	assert.NoError(t, err)

	// Replace health checkers with our test instances
	hcm.hcs = []*HealthChecker{hc1, hc2}

	// Test case 1: No lag, no taint
	hc1.blockNumber = 100
	hc2.blockNumber = 100
	hcm.checkBlockLagAndTaint("test1", 100)
	assert.False(t, hc1.IsTainted())
	assert.False(t, hc2.IsTainted())

	// Test case 2: Lag exceeds threshold, should taint
	hc1.blockNumber = 100
	hc2.blockNumber = 103
	hcm.checkBlockLagAndTaint("test1", 100)
	assert.True(t, hc1.IsTainted(), "Provider should be tainted when block difference exceeds threshold")
	assert.False(t, hc2.IsTainted())

	// Reset taint for next test
	hc1.RemoveTaint()

	// Test case 3: Lag within threshold, no taint
	hc1.blockNumber = 100
	hc2.blockNumber = 101
	hcm.checkBlockLagAndTaint("test1", 100)
	assert.False(t, hc1.IsTainted(), "Provider should not be tainted when block difference is within threshold")
	assert.False(t, hc2.IsTainted())
} 