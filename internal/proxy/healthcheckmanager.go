package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
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
	metricRPCProviderGasLeft = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "rpc_provider_gas_left",
			Help: "Gas left in the RPC provider",
		},
		[]string{"name", "proxy"},
	)
)

type Target struct {
	Name       string `yaml:"name"`
	Connection struct {
		HTTP struct {
			URL string `yaml:"url"`
		} `yaml:"http"`
	} `yaml:"connection"`
}

// HealthCheckManagerConfig is the configuration for the health check manager
type HealthCheckManagerConfig struct {
	Targets []NodeProviderConfig `yaml:"targets"`
	Config  HealthCheckConfig    `yaml:"healthChecks"`
	Logger  *slog.Logger
	Path    string
}

type HealthCheckManager struct {
	Config    HealthCheckConfig
	Targets   []NodeProviderConfig
	Logger    *slog.Logger
	checkers  []*HealthChecker
	ctx       context.Context
	Path      string // Add Path field to store the proxy path

	// Cache for last reported values with RWMutex for better read performance
	lastReportedValues struct {
		sync.RWMutex
		healthy map[string]bool
		tainted map[string]bool
		block   map[string]uint64
		gas     map[string]uint64
	}
}

func NewHealthCheckManager(config HealthCheckManagerConfig) (*HealthCheckManager, error) {
	// Create health checkers for each target
	checkers := make([]*HealthChecker, 0, len(config.Targets))
	for _, target := range config.Targets {
		checker, err := NewHealthChecker(HealthCheckerConfig{
			Logger:           config.Logger,
			URL:              target.Connection.HTTP.URL,
			Name:             target.Name,
			Interval:         config.Config.Interval,
			Timeout:          config.Config.Timeout,
			FailureThreshold: config.Config.FailureThreshold,
			SuccessThreshold: config.Config.SuccessThreshold,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create health checker for target %s: %w", target.Name, err)
		}
		checkers = append(checkers, checker)
	}

	hcm := &HealthCheckManager{
		Config:   config.Config,
		Targets:  config.Targets,
		Logger:   config.Logger,
		checkers: checkers,
		Path:     config.Path, // Set the Path field from config
	}

	hcm.lastReportedValues.healthy = make(map[string]bool)
	hcm.lastReportedValues.tainted = make(map[string]bool)
	hcm.lastReportedValues.block = make(map[string]uint64)
	hcm.lastReportedValues.gas = make(map[string]uint64)

	return hcm, nil
}

func (h *HealthCheckManager) runLoop(c context.Context) error {
	ticker := time.NewTicker(time.Second * 1)
	defer ticker.Stop()

	for {
		select {
		case <-c.Done():
			return nil
		case <-ticker.C:
			h.reportStatusMetrics()
		}
	}
}

func (h *HealthCheckManager) IsHealthy(name string) bool {
	for _, hc := range h.checkers {
		if hc.Name() == name && hc.IsHealthy() {
			return true
		}
	}

	return false
}

// GetHealthChecker returns the health checker for a given provider name
func (h *HealthCheckManager) GetHealthChecker(name string) *HealthChecker {
	for _, hc := range h.checkers {
		if hc.Name() == name {
			return hc
		}
	}
	return nil
}

func (h *HealthCheckManager) reportStatusMetrics() {
	// Use read lock for initial value checks
	h.lastReportedValues.RLock()
	updates := make(map[string]struct {
		healthy bool
		tainted bool
		block   uint64
		gas     uint64
		changed bool
	})

	// First pass: collect all values and check what needs updating
	for _, hc := range h.checkers {
		name := hc.Name()
		isHealthy := hc.IsHealthy()
		isTainted := hc.IsTainted()
		blockNumber := hc.BlockNumber()
		gasLeft := hc.GasLeft()

		needsUpdate := false
		if h.lastReportedValues.healthy[name] != isHealthy ||
			h.lastReportedValues.tainted[name] != isTainted {
			needsUpdate = true
		}

		// Only check block and gas if healthy and not tainted
		if isHealthy && !isTainted {
			if h.lastReportedValues.block[name] != blockNumber ||
				h.lastReportedValues.gas[name] != gasLeft {
				needsUpdate = true
			}
		}

		if needsUpdate {
			updates[name] = struct {
				healthy bool
				tainted bool
				block   uint64
				gas     uint64
				changed bool
			}{
				healthy: isHealthy,
				tainted: isTainted,
				block:   blockNumber,
				gas:     gasLeft,
				changed: true,
			}
		}
	}
	h.lastReportedValues.RUnlock()

	// If no updates needed, return early
	if len(updates) == 0 {
		return
	}

	// Use write lock only for actual updates
	h.lastReportedValues.Lock()
	defer h.lastReportedValues.Unlock()

	// Apply all updates
	for name, update := range updates {
		if update.changed {
			// Update status metrics
			metricRPCProviderStatus.WithLabelValues(name, "healthy", h.Path).Set(boolToFloat64(update.healthy))
			metricRPCProviderStatus.WithLabelValues(name, "tainted", h.Path).Set(boolToFloat64(update.tainted))
			h.lastReportedValues.healthy[name] = update.healthy
			h.lastReportedValues.tainted[name] = update.tainted

			// Update block and gas metrics if healthy and not tainted
			if update.healthy && !update.tainted {
				metricRPCProviderBlockNumber.WithLabelValues(name, h.Path).Set(float64(update.block))
				metricRPCProviderGasLeft.WithLabelValues(name, h.Path).Set(float64(update.gas))
				h.lastReportedValues.block[name] = update.block
				h.lastReportedValues.gas[name] = update.gas
			}
		}
	}
}

// boolToFloat64 converts a boolean to float64 (1.0 for true, 0.0 for false)
func boolToFloat64(b bool) float64 {
	if b {
		return 1.0
	}
	return 0.0
}

func (h *HealthCheckManager) Start(c context.Context) error {
	for i, hc := range h.checkers {
		metricRPCProviderInfo.WithLabelValues(strconv.Itoa(i), hc.Name(), h.Path).Set(1)
		go hc.Start(c)
	}

	// Run the main loop in a goroutine
	go func() {
		ticker := time.NewTicker(time.Second * 1)
		defer ticker.Stop()

		for {
			select {
			case <-c.Done():
				return
			case <-ticker.C:
				h.reportStatusMetrics()
			}
		}
	}()

	return nil
}

func (h *HealthCheckManager) Stop(c context.Context) error {
	slog.Info("stopping health check manager")
	
	for _, checker := range h.checkers {
		if err := checker.Stop(c); err != nil {
			slog.Error("error stopping health checker", "name", checker.Name(), "error", err)
		}
	}

	return nil
}

// checkBlockLagAndTaint checks if a provider's block number is lagging behind others
func (h *HealthCheckManager) checkBlockLagAndTaint(updatedRPCName string, updatedBlockNumber uint64) {
	var maxBlock uint64
	var targetHC *HealthChecker

	// Single pass: find max block and target health checker
	for _, hc := range h.checkers {
		if bn := hc.BlockNumber(); bn > maxBlock {
			maxBlock = bn
		}
		if hc.Name() == updatedRPCName {
			targetHC = hc
		}
	}

	diff := maxBlock - updatedBlockNumber
	h.Logger.Debug("RPC block comparison",
		"name", updatedRPCName,
		"blockNumber", updatedBlockNumber,
		"maxBlock", maxBlock,
		"diff", diff,
		"threshold", h.Config.BlockDiffThreshold,
	)

	if diff > uint64(h.Config.BlockDiffThreshold) && targetHC != nil {
		targetHC.TaintHealthCheck()
		h.Logger.Warn("Provider tainted due to block difference", 
			"provider", targetHC.Name(),
			"blockDiff", diff,
		)
	}
}
