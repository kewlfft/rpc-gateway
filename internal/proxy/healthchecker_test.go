package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		Logger:           slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	healthchecker, err := NewHealthChecker(healtcheckConfig)
	assert.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	healthchecker.Start(ctx)

	// Wait for a health check cycle
	time.Sleep(time.Millisecond * 150)

	// Verify that the health checker is not tainted initially
	assert.False(t, healthchecker.IsTainted())
	assert.True(t, healthchecker.IsHealthy())

	// Taint the health checker
	healthchecker.TaintHealthCheck()
	assert.True(t, healthchecker.IsTainted())
	assert.False(t, healthchecker.IsHealthy())

	// Remove taint
	healthchecker.RemoveTaint()
	assert.False(t, healthchecker.IsTainted())
	assert.True(t, healthchecker.IsHealthy())
}

func TestHealthCheckerTaint(t *testing.T) {
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

	// Create a health checker with short intervals for testing
	config := HealthCheckerConfig{
		URL:    server.URL,
		Name:   "test",
		Path:   "test",
		Interval: time.Millisecond * 100,
		Timeout: time.Millisecond * 50,
		Logger:  slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	checker, err := NewHealthChecker(config)
	require.NoError(t, err)

	// Start the health checker
	ctx, cancel := context.WithCancel(context.Background())
	go checker.Start(ctx)
	defer cancel()

	t.Logf("Test started at %v", time.Now())

	// Test initial state
	assert.False(t, checker.IsTainted(), "should not be tainted initially")

	// Taint the checker with a short wait time
	waitTime := time.Millisecond * 200
	t.Logf("Applying first taint at %v with wait time %v", time.Now(), waitTime)
	checker.Taint(TaintConfig{
		InitialWaitTime:   waitTime,
		MaxWaitTime:       time.Second,
		ResetWaitDuration: time.Second,
		Reason:           "test taint",
	})

	// Verify it's tainted
	assert.True(t, checker.IsTainted(), "should be tainted after taint call")

	// Wait for taint to be removed with a buffer
	t.Logf("Waiting for first taint removal at %v", time.Now())
	time.Sleep(waitTime + time.Millisecond*100)
	t.Logf("Checking taint state at %v", time.Now())

	// Verify taint is removed
	assert.False(t, checker.IsTainted(), "taint should be removed after wait time")

	// Test exponential backoff
	t.Logf("Applying second taint at %v", time.Now())
	checker.Taint(TaintConfig{
		InitialWaitTime:   waitTime,
		MaxWaitTime:       time.Second,
		ResetWaitDuration: time.Millisecond * 50, // Short reset duration to ensure we're within it
		Reason:           "test taint",
	})

	// Verify it's tainted again
	assert.True(t, checker.IsTainted(), "should be tainted after second taint call")

	// Wait for taint to be removed
	t.Logf("Waiting for second taint removal at %v", time.Now())
	time.Sleep(waitTime + time.Millisecond*100)
	t.Logf("Checking taint state at %v", time.Now())

	// Verify taint is removed
	assert.False(t, checker.IsTainted(), "taint should be removed after wait time")

	// Test max wait time
	t.Logf("Applying third taint at %v with max wait time 300ms", time.Now())
	checker.Taint(TaintConfig{
		InitialWaitTime:   waitTime,
		MaxWaitTime:       time.Millisecond * 300,
		ResetWaitDuration: time.Millisecond * 50,
		Reason:           "test taint",
	})

	// Verify it's tainted again
	assert.True(t, checker.IsTainted(), "should be tainted after third taint call")

	// Wait for taint to be removed
	t.Logf("Waiting for third taint removal at %v", time.Now())
	time.Sleep(waitTime + time.Millisecond*100)
	t.Logf("Checking final taint state at %v", time.Now())

	// Verify taint is removed
	assert.False(t, checker.IsTainted(), "taint should be removed after wait time")
}

func TestHealthCheckerTaintRemoval(t *testing.T) {
	// Create a health checker with short intervals for testing
	config := HealthCheckerConfig{
		URL:    "http://localhost:8545", // Doesn't matter for this test
		Name:   "test",
		Path:   "test",
		Interval: time.Millisecond * 100,
		Timeout: time.Millisecond * 50,
		Logger:  slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	checker, err := NewHealthChecker(config)
	require.NoError(t, err)

	// Start the health checker
	ctx, cancel := context.WithCancel(context.Background())
	go checker.Start(ctx)
	defer cancel()

	// Taint the checker
	waitTime := time.Millisecond * 200
	checker.Taint(TaintConfig{
		InitialWaitTime:   waitTime,
		MaxWaitTime:       time.Second,
		ResetWaitDuration: time.Second,
		Reason:           "test taint",
	})

	// Verify it's tainted
	assert.True(t, checker.IsTainted(), "should be tainted after taint call")

	// Stop the checker
	err = checker.Stop(context.Background())
	require.NoError(t, err)

	// Verify taint removal timer is cleaned up
	time.Sleep(time.Millisecond * 100)
	assert.True(t, checker.IsTainted(), "taint should remain after stop")
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
