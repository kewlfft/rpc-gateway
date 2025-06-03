package proxy

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type HealthCheckManagerConfig struct {
	Targets []NodeProviderConfig
	Config  HealthCheckConfig
	Logger  *slog.Logger
}

type HealthCheckManager struct {
	hcs    []*HealthChecker
	hcMap  map[string]*HealthChecker // Map for O(1) lookup by name
	logger *slog.Logger
	Config HealthCheckConfig

	metricRPCProviderInfo        *prometheus.GaugeVec
	metricRPCProviderStatus      *prometheus.GaugeVec
	metricRPCProviderBlockNumber *prometheus.GaugeVec
	metricRPCProviderGasLeft    *prometheus.GaugeVec

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
	hcm := &HealthCheckManager{
		logger: config.Logger,
		Config: config.Config,
		hcMap:  make(map[string]*HealthChecker),
		metricRPCProviderInfo: promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "zeroex_rpc_gateway_provider_info",
				Help: "Gas limit of a given provider",
			}, []string{
				"index",
				"provider",
			}),
		metricRPCProviderStatus: promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "zeroex_rpc_gateway_provider_status",
				Help: "Current status of a given provider by type. Type can be either healthy or tainted.",
			}, []string{
				"provider",
				"type",
			}),
		metricRPCProviderBlockNumber: promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "zeroex_rpc_gateway_provider_block_number",
				Help: "Block number of a given provider",
			}, []string{
				"provider",
			}),
		metricRPCProviderGasLeft: promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "zeroex_rpc_gateway_provider_gasLeft_number",
				Help: "Gas left of a given provider",
			}, []string{
				"provider",
			}),
	}

	// Initialize the cache maps
	hcm.lastReportedValues.healthy = make(map[string]bool)
	hcm.lastReportedValues.tainted = make(map[string]bool)
	hcm.lastReportedValues.block = make(map[string]uint64)
	hcm.lastReportedValues.gas = make(map[string]uint64)

	for _, target := range config.Targets {
		hc, err := NewHealthChecker(
			HealthCheckerConfig{
				Logger:           config.Logger,
				URL:              target.Connection.HTTP.URL,
				Name:             target.Name,
				Interval:         config.Config.Interval,
				Timeout:          config.Config.Timeout,
				FailureThreshold: config.Config.FailureThreshold,
				SuccessThreshold: config.Config.SuccessThreshold,
			})
		if err != nil {
			return nil, err
		}

		// Create a closure that captures the provider name
		hc.SetBlockNumberUpdateCallback(func(blockNumber uint64) {
			hcm.checkBlockLagAndTaint(target.Name, blockNumber)
		})

		hcm.hcs = append(hcm.hcs, hc)
		hcm.hcMap[target.Name] = hc
	}

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
	for _, hc := range h.hcs {
		if hc.Name() == name && hc.IsHealthy() {
			return true
		}
	}

	return false
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
	for _, hc := range h.hcs {
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
			h.metricRPCProviderStatus.WithLabelValues(name, "healthy").Set(boolToFloat64(update.healthy))
			h.metricRPCProviderStatus.WithLabelValues(name, "tainted").Set(boolToFloat64(update.tainted))
			h.lastReportedValues.healthy[name] = update.healthy
			h.lastReportedValues.tainted[name] = update.tainted

			// Update block and gas metrics if healthy and not tainted
			if update.healthy && !update.tainted {
				h.metricRPCProviderBlockNumber.WithLabelValues(name).Set(float64(update.block))
				h.metricRPCProviderGasLeft.WithLabelValues(name).Set(float64(update.gas))
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
	for i, hc := range h.hcs {
		h.metricRPCProviderInfo.WithLabelValues(strconv.Itoa(i), hc.Name()).Set(1)
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
	
	for _, checker := range h.hcs {
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
	for _, hc := range h.hcs {
		if bn := hc.BlockNumber(); bn > maxBlock {
			maxBlock = bn
		}
		if hc.Name() == updatedRPCName {
			targetHC = hc
		}
	}

	diff := maxBlock - updatedBlockNumber
	h.logger.Debug("RPC block comparison",
		"name", updatedRPCName,
		"blockNumber", updatedBlockNumber,
		"maxBlock", maxBlock,
		"diff", diff,
		"threshold", h.Config.BlockDiffThreshold,
	)

	if diff > uint64(h.Config.BlockDiffThreshold) && targetHC != nil {
		targetHC.Taint()
		h.logger.Warn("RPC provider tainted due to block lag", 
			"name", updatedRPCName,
			"blockNumber", updatedBlockNumber,
			"maxBlock", maxBlock,
			"diff", diff,
			"threshold", h.Config.BlockDiffThreshold,
		)
	}
}
