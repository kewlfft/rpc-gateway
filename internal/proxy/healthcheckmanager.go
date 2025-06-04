package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

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
	metricRPCProviderGasLeft = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "rpc_provider_gas_left",
			Help: "Gas left in the RPC provider",
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
}

// NewHealthCheckManager creates a new health check manager
func NewHealthCheckManager(config Config) (*HealthCheckManager, error) {
	checkers := make([]*HealthChecker, 0, len(config.Targets))
	for _, target := range config.Targets {
		checker, err := NewHealthChecker(HealthCheckerConfig{
			Logger:           config.Logger,
			URL:              target.Connection.HTTP.URL,
			Name:             target.Name,
			Interval:         config.HealthChecks.Interval,
			Timeout:          config.HealthChecks.Timeout,
			FailureThreshold: config.HealthChecks.FailureThreshold,
			SuccessThreshold: config.HealthChecks.SuccessThreshold,
			Path:            config.Path,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create health checker for target %s: %w", target.Name, err)
		}
		checkers = append(checkers, checker)
	}

	return &HealthCheckManager{
		config:   config.HealthChecks,
		targets:  config.Targets,
		logger:   config.Logger,
		checkers: checkers,
		path:     config.Path,
	}, nil
}

// Start starts the health check manager
func (h *HealthCheckManager) Start(ctx context.Context) error {
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

// IsHealthy checks if a provider is healthy
func (h *HealthCheckManager) IsHealthy(name string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, checker := range h.checkers {
		if checker.Name() == name {
			return checker.IsHealthy()
		}
	}
	return false
}

// GetHealthChecker returns the health checker for a provider
func (h *HealthCheckManager) GetHealthChecker(name string) *HealthChecker {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, checker := range h.checkers {
		if checker.Name() == name {
			return checker
		}
	}
	return nil
}

// reportStatusMetrics reports the current status of all providers
func (h *HealthCheckManager) reportStatusMetrics() {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for i, checker := range h.checkers {
		name := checker.Name()
		status := "healthy"
		if !checker.IsHealthy() {
			status = "unhealthy"
		}

		metricRPCProviderInfo.WithLabelValues(fmt.Sprintf("%d", i), name, h.path).Set(1)
		metricRPCProviderStatus.WithLabelValues(name, status, h.path).Set(1)
		metricRPCProviderBlockNumber.WithLabelValues(name, h.path).Set(float64(checker.BlockNumber()))
		metricRPCProviderGasLeft.WithLabelValues(name, h.path).Set(float64(checker.GasLeft()))
	}
}
