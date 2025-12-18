package proxy

import (
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBlockLagAndTaint(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Create two health checkers
	hc1, _ := NewHealthChecker(HealthCheckerConfig{URL: "http://test1", Name: "test1", Logger: logger})
	hc2, _ := NewHealthChecker(HealthCheckerConfig{URL: "http://test2", Name: "test2", Logger: logger})

	// Helper to set block numbers
	setBlocks := func(b1, b2 uint64) {
		hc1.blockNumber.Store(b1)
		hc2.blockNumber.Store(b2)
		hc1.RemoveTaint()
		hc2.RemoveTaint()
	}

	// Case 1: No lag, no taint
	setBlocks(100, 100)
	assert.False(t, hc1.IsTainted())
	assert.False(t, hc2.IsTainted())

	// Case 2: Lag exceeds threshold, should taint
	setBlocks(100, 103)
	threshold := uint64(2)
	maxBlock := hc2.BlockNumber()
	if maxBlock-hc1.BlockNumber() > threshold {
		hc1.TaintHealthCheck()
	}
	assert.True(t, hc1.IsTainted())
	assert.False(t, hc2.IsTainted())

	// Case 3: Lag within threshold, no taint
	setBlocks(100, 101)
	if hc2.BlockNumber()-hc1.BlockNumber() > threshold {
		hc1.TaintHealthCheck()
	}
	assert.False(t, hc1.IsTainted())
	assert.False(t, hc2.IsTainted())
} 