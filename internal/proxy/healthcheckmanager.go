package proxy

import (
	"context"
	"log/slog"
	"strconv"
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
	logger *slog.Logger
	Config HealthCheckConfig

	metricRPCProviderInfo        *prometheus.GaugeVec
	metricRPCProviderStatus      *prometheus.GaugeVec
	metricRPCProviderBlockNumber *prometheus.GaugeVec
	metricRPCProviderGasLeft    *prometheus.GaugeVec
}

func NewHealthCheckManager(config HealthCheckManagerConfig) (*HealthCheckManager, error) {
	hcm := &HealthCheckManager{
		logger: config.Logger,
		Config: config.Config,
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
	for _, hc := range h.hcs {
		if hc.IsHealthy() {
			h.metricRPCProviderStatus.WithLabelValues(hc.Name(), "healthy").Set(1)
		} else {
			h.metricRPCProviderStatus.WithLabelValues(hc.Name(), "healthy").Set(0)
		}

		h.metricRPCProviderGasLeft.WithLabelValues(hc.Name()).Set(float64(hc.BlockNumber()))
		h.metricRPCProviderBlockNumber.WithLabelValues(hc.Name()).Set(float64(hc.BlockNumber()))
	}
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

	// Single pass: find max block
	for _, hc := range h.hcs {
		if bn := hc.BlockNumber(); bn > maxBlock {
			maxBlock = bn
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

	if diff > uint64(h.Config.BlockDiffThreshold) {
		// TODO: Implement taint functionality
		h.logger.Warn("RPC provider is lagging behind", 
			"name", updatedRPCName,
			"diff", diff,
			"threshold", h.Config.BlockDiffThreshold,
		)
	}
}
