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

	// Test case 1: No lag, no taint
	hc1.blockNumber = 100
	hc2.blockNumber = 100
	assert.False(t, hc1.IsTainted())
	assert.False(t, hc2.IsTainted())

	// Test case 2: Lag exceeds threshold, should taint
	hc1.blockNumber = 100
	hc2.blockNumber = 103
	hc1.TaintHealthCheck()
	assert.True(t, hc1.IsTainted(), "Provider should be tainted when block difference exceeds threshold")
	assert.False(t, hc2.IsTainted())

	// Reset taint for next test
	hc1.RemoveTaint()

	// Test case 3: Lag within threshold, no taint
	hc1.blockNumber = 100
	hc2.blockNumber = 101
	assert.False(t, hc1.IsTainted(), "Provider should not be tainted when block difference is within threshold")
	assert.False(t, hc2.IsTainted())
} 