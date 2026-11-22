package proxy

import (
	"context"
	"fmt"
	"time"
)

// HealthCheckManager manages health checks for multiple node providers
type HealthCheckManager struct {
	config   HealthCheckConfig
	checkers []*HealthChecker
	path     string
	// O(1) health checker lookups; written only during construction
	checkerMap map[string]*HealthChecker // key: "name:connectionType"
}

// NewHealthCheckManager creates a new health check manager
func NewHealthCheckManager(config Config) (*HealthCheckManager, error) {
	checkers := make([]*HealthChecker, 0, len(config.Targets)*2) // *2 for both HTTP and WebSocket

	// Create the manager first so we can use it in the callbacks
	hcm := &HealthCheckManager{
		config:     config.HealthChecks,
		path:       config.Path,
		checkerMap: make(map[string]*HealthChecker),
	}

	for _, target := range config.Targets {
		config.Logger.Debug("Creating health checker",
			"provider", target.Name,
			"chainType", config.ChainType,
			"path", config.Path)

		// Create health checkers for both HTTP and WebSocket if configured
		connections := []struct {
			url      string
			connType string
		}{
			{target.Connection.HTTP.URL, "http"},
			{target.Connection.WebSocket.URL, "websocket"},
		}

		for _, conn := range connections {
			if conn.url == "" {
				continue
			}

			// Stagger paths by 1 s each so their checks don't all fire at once
			initialDelay := time.Duration(config.PathIndex) * time.Second
			if conn.connType == "websocket" {
				initialDelay += 500 * time.Millisecond // Stagger WebSocket after HTTP checks
			}

			checker := NewHealthChecker(HealthCheckerConfig{
				Logger:         config.Logger,
				URL:            conn.url,
				Name:           target.Name,
				Interval:       config.HealthChecks.Interval,
				Timeout:        config.Timeout,
				Path:           config.Path,
				ChainType:      config.ChainType,
				ConnectionType: conn.connType,
				Headers:        target.Connection.HTTP.Headers,
				InitialDelay:   initialDelay,
			})

			checker.SetBlockNumberUpdateCallback(func(blockNumber uint64) {
				hcm.checkBlockLagAndTaint(checker, blockNumber)
			})

			checkers = append(checkers, checker)
			hcm.checkerMap[target.Name+":"+conn.connType] = checker
		}
	}

	hcm.checkers = checkers
	return hcm, nil
}

// Start starts all health checkers (non-blocking)
func (h *HealthCheckManager) Start(ctx context.Context) {
	// checker.Start is non-blocking; staggering is handled via each checker's InitialDelay
	for _, checker := range h.checkers {
		checker.Start(ctx)
	}
}

// Stop cancels pending taint removals of all health checkers
func (h *HealthCheckManager) Stop() {
	for _, checker := range h.checkers {
		checker.Stop()
	}
}

// GetHealthChecker returns the health checker for a provider and connection type
func (h *HealthCheckManager) GetHealthChecker(name string, connectionType string) *HealthChecker {
	return h.checkerMap[name+":"+connectionType]
}

// checkBlockLagAndTaint checks if a provider's block number is lagging behind others
// Optimized to find max block and target checker in a single pass
// The updated checker itself is tainted, so HTTP and WebSocket endpoints of the
// same provider are judged separately.
func (h *HealthCheckManager) checkBlockLagAndTaint(updated *HealthChecker, updatedBlockNumber uint64) {
	var maxBlock uint64
	for _, checker := range h.checkers {
		maxBlock = max(maxBlock, checker.BlockNumber())
	}

	// Equivalent to: maxBlock - updatedBlockNumber > threshold, without underflow
	if maxBlock > updatedBlockNumber+h.config.BlockDiffThreshold {
		updated.TaintHealthCheck(fmt.Sprintf(
			"block lag: block=%d max=%d threshold=%d",
			updatedBlockNumber, maxBlock, h.config.BlockDiffThreshold,
		))
	}
}
