package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// HealthCheckManager manages health checks for multiple node providers
type HealthCheckManager struct {
	config   HealthCheckConfig
	logger   *slog.Logger
	checkers []*HealthChecker
	path     string
	// Add initial delay for path-level staggering
	initialDelay time.Duration
	// O(1) health checker lookups; written only during construction
	checkerMap map[string]*HealthChecker // key: "name:connectionType"
}

// NewHealthCheckManager creates a new health check manager
func NewHealthCheckManager(config Config) (*HealthCheckManager, error) {
	checkers := make([]*HealthChecker, 0, len(config.Targets)*2) // *2 for both HTTP and WebSocket

	// Create the manager first so we can use it in the callbacks
	hcm := &HealthCheckManager{
		config: config.HealthChecks,
		logger: config.Logger,
		path:   config.Path,
		// Use incremental staggering
		initialDelay: time.Duration(config.PathIndex*1000) * time.Millisecond,
		checkerMap:   make(map[string]*HealthChecker),
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

			initialDelay := hcm.initialDelay
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
				hcm.checkBlockLagAndTaint(target.Name, blockNumber)
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
func (h *HealthCheckManager) Stop(ctx context.Context) {
	for _, checker := range h.checkers {
		_ = checker.Stop(ctx)
	}
}

// GetHealthChecker returns the health checker for a provider and connection type
func (h *HealthCheckManager) GetHealthChecker(name string, connectionType string) *HealthChecker {
	return h.checkerMap[name+":"+connectionType]
}

// checkBlockLagAndTaint checks if a provider's block number is lagging behind others
// Optimized to find max block and target checker in a single pass
func (h *HealthCheckManager) checkBlockLagAndTaint(updatedRPCName string, updatedBlockNumber uint64) {
	var maxBlock uint64
	var targetChecker *HealthChecker

	// Find the maximum block number and target checker in a single pass
	for _, checker := range h.checkers {
		bn := checker.BlockNumber()
		if bn > maxBlock {
			maxBlock = bn
		}
		// Also find the target checker while we're iterating
		if checker.Name() == updatedRPCName {
			targetChecker = checker
		}
	}

	// If the updated provider is too far behind, taint it
	// Equivalent to: maxBlock > updatedBlockNumber && (maxBlock-updatedBlockNumber) > threshold
	if maxBlock > updatedBlockNumber+uint64(h.config.BlockDiffThreshold) {
		// Taint the specific provider (already found in loop above)
		if targetChecker != nil {
			targetChecker.TaintHealthCheck(fmt.Sprintf(
				"block lag: block=%d max=%d threshold=%d",
				updatedBlockNumber, maxBlock, h.config.BlockDiffThreshold,
			))
		}
	}
}
