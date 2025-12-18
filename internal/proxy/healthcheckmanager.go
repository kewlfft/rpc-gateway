package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	metricRPCProviderInfo = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "rpc_provider_info",
			Help: "Information about the RPC provider",
		},
		[]string{"index", "name", "proxy"},
	)
	metricRPCProviderStatus = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "rpc_provider_status",
			Help: "Status of the RPC provider (1 = healthy, 0 = unhealthy)",
		},
		[]string{"name", "status", "proxy"},
	)
	metricRPCProviderBlockNumber = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "rpc_provider_block_number",
			Help: "Current block number of the RPC provider",
		},
		[]string{"name", "proxy"},
	)
)

// HealthCheckManager manages health checks for multiple node providers
type HealthCheckManager struct {
	config   HealthCheckConfig
	targets  []NodeProviderConfig
	logger   *slog.Logger
	checkers []*HealthChecker
	path     string
	mu       sync.RWMutex
	// Add initial delay for path-level staggering
	initialDelay time.Duration
	// Add map for O(1) health checker lookups
	checkerMap map[string]*HealthChecker // key: "name:connectionType"
}

// NewHealthCheckManager creates a new health check manager
func NewHealthCheckManager(config Config) (*HealthCheckManager, error) {
	checkers := make([]*HealthChecker, 0, len(config.Targets)*2) // *2 for both HTTP and WebSocket
	
	// Create the manager first so we can use it in the callbacks
	hcm := &HealthCheckManager{
		config:   config.HealthChecks,
		targets:  config.Targets,
		logger:   config.Logger,
		path:     config.Path,
		// Use incremental staggering
		initialDelay: time.Duration(config.PathIndex * 1000) * time.Millisecond,
		checkerMap: make(map[string]*HealthChecker),
	}

	for _, target := range config.Targets {
		config.Logger.Debug("Creating health checker",
			"provider", target.Name,
			"chainType", config.ChainType,
			"path", config.Path)

		// Create health checkers for both HTTP and WebSocket if configured
		connections := []struct {
			url  string
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

			checker, err := NewHealthChecker(HealthCheckerConfig{
				Logger:           config.Logger,
				URL:              conn.url,
				Name:             target.Name,
				Interval:         config.HealthChecks.Interval,
				Timeout:          config.Timeout,
				Path:            config.Path,
				ChainType:       config.ChainType,
				ConnectionType:  conn.connType,
				BlockDiffThreshold: uint(config.HealthChecks.BlockDiffThreshold),
				APIKey:            target.Connection.HTTP.APIKey,
				InitialDelay:      initialDelay,
			})
			if err != nil {
				// For WebSocket connections, log a warning and continue instead of failing
				if conn.connType == "websocket" {
					config.Logger.Warn("failed to create WebSocket health checker, skipping WebSocket health checks",
						"target", target.Name,
						"url", conn.url,
						"error", err)
					continue
				}
				return nil, fmt.Errorf("failed to create %s health checker for target %s: %w", conn.connType, target.Name, err)
			}

			// Set up block number update callback
			checker.SetBlockNumberUpdateCallback(func(blockNumber uint64) {
				hcm.checkBlockLagAndTaint(target.Name, blockNumber)
			})

			checkers = append(checkers, checker)
			
			// Store in map for O(1) lookups
			key := fmt.Sprintf("%s:%s", target.Name, conn.connType)
			hcm.checkerMap[key] = checker
		}
	}

	hcm.checkers = checkers
	return hcm, nil
}

// Start starts the health check manager
func (h *HealthCheckManager) Start(ctx context.Context) error {
	// Start all checkers for this path concurrently
	// The path-level staggering is handled by the proxy's path initialization
	for _, checker := range h.checkers {
		go checker.Start(ctx)
	}
	return nil
}

// Stop stops the health check manager
func (h *HealthCheckManager) Stop(ctx context.Context) error {
	for _, checker := range h.checkers {
		if err := checker.Stop(ctx); err != nil {
			h.logger.Error("failed to stop health checker", "error", err)
		}
	}
	return nil
}

// IsHealthy checks if a provider is healthy for a specific connection type
func (h *HealthCheckManager) IsHealthy(name string, connectionType string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	key := fmt.Sprintf("%s:%s", name, connectionType)
	if checker, exists := h.checkerMap[key]; exists {
		return checker.IsHealthy()
	}
	return false
}

// GetHealthChecker returns the health checker for a provider and connection type
func (h *HealthCheckManager) GetHealthChecker(name string, connectionType string) *HealthChecker {
	h.mu.RLock()
	defer h.mu.RUnlock()

	key := fmt.Sprintf("%s:%s", name, connectionType)
	return h.checkerMap[key]
}

// SetWebSocketProxyReferences sets the WebSocket proxy references for all WebSocket health checkers
func (h *HealthCheckManager) SetWebSocketProxyReferences(wsProxies map[string]*WebSocketProxy) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for name, wsProxy := range wsProxies {
		key := fmt.Sprintf("%s:websocket", name)
		if checker, exists := h.checkerMap[key]; exists {
			checker.SetBeforeTaintCallback(func() {
				wsProxy.UnsubscribeAll()
			})
			h.logger.Debug("WebSocket proxy reference set", 
				"provider", name, "path", h.path)
		}
	}
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
		h.logger.Info("provider block lag",
			"provider", updatedRPCName,
			"blockNumber", updatedBlockNumber,
			"maxBlock", maxBlock,
			"threshold", h.config.BlockDiffThreshold,
			"path", h.path)

		// Taint the specific provider (already found in loop above)
		if targetChecker != nil {
			targetChecker.TaintHealthCheck("block lag", nil)
		}
	}
}
