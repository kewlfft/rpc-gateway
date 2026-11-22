package proxy

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

// HealthCheckManager manages health checks for multiple node providers
type HealthCheckManager struct {
	config   HealthCheckConfig
	checkers []*HealthChecker
	path     string
	// O(1) health checker lookups; written only during construction
	checkerMap map[string]*HealthChecker // key: "name:connectionType"
	timeout    time.Duration

	lagMu     sync.Mutex  // serializes checkBlockLag
	rateStart blockSample // first max block seen, for the chain's block rate
}

// NewHealthCheckManager creates a new health check manager
func NewHealthCheckManager(config Config) (*HealthCheckManager, error) {
	checkers := make([]*HealthChecker, 0, len(config.Targets)*2) // *2 for both HTTP and WebSocket

	// Create the manager first so we can use it in the callbacks
	hcm := &HealthCheckManager{
		config:     config.HealthChecks,
		path:       config.Path,
		checkerMap: make(map[string]*HealthChecker),
		timeout:    config.Timeout,
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

			checker.SetBlockNumberUpdateCallback(func() { hcm.checkBlockLag(time.Now()) })

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

// minRateSpan is the history of the max block needed before its rate is trusted.
const minRateSpan = 10 * time.Second

// checkBlockLag taints every endpoint whose latest block is more than
// BlockDiffThreshold behind the highest block of the current check cycle.
//
// All checks of a path fire together, so an endpoint that answers before the
// leader must be judged again once the leader's block arrives: every fresh
// sample is re-evaluated on every update. Samples older than the freshness
// window (previous cycles, tainted endpoints that skip checks) are ignored.
// When a sample was taken before the top one, the blocks the chain produced
// in between (estimated from the max block's observed rate) are allowed, so
// fast chains don't taint an endpoint just for answering first. HTTP and
// WebSocket endpoints of the same provider are judged separately.
func (h *HealthCheckManager) checkBlockLag(now time.Time) {
	h.lagMu.Lock()
	defer h.lagMu.Unlock()

	window := h.timeout + time.Second // covers one cycle: 500 ms WS stagger + response time
	fresh := make([]*HealthChecker, 0, len(h.checkers))
	samples := make([]blockSample, 0, len(h.checkers))
	var top blockSample
	for _, c := range h.checkers {
		s := c.sample.Load()
		if s == nil || now.Sub(s.at) > window {
			continue
		}
		fresh = append(fresh, c)
		samples = append(samples, *s)
		if s.block > top.block || (s.block == top.block && s.at.Before(top.at)) {
			top = *s
		}
	}
	if len(fresh) == 0 {
		return
	}
	rate, rateKnown := h.blockRate(top)

	for i, c := range fresh {
		s := samples[i]
		var allowance uint64
		if skew := top.at.Sub(s.at); skew > 0 {
			if !rateKnown {
				continue // can't tell lag from answering earlier yet
			}
			allowance = uint64(math.Ceil(rate * skew.Seconds()))
		}
		// Equivalent to: top - block > threshold + allowance, without underflow
		if top.block > s.block+h.config.BlockDiffThreshold+allowance {
			c.TaintHealthCheck(fmt.Sprintf(
				"block lag: block=%d max=%d threshold=%d allowance=%d",
				s.block, top.block, h.config.BlockDiffThreshold, allowance,
			))
		}
	}
}

// blockRate returns blocks per second, measured from the first max block seen
// to top. Caller holds lagMu.
func (h *HealthCheckManager) blockRate(top blockSample) (float64, bool) {
	if h.rateStart.at.IsZero() || top.block < h.rateStart.block {
		h.rateStart = top // first sample, or the start was bogus high
		return 0, false
	}
	span := top.at.Sub(h.rateStart.at)
	if span < minRateSpan {
		return 0, false
	}
	return float64(top.block-h.rateStart.block) / span.Seconds(), true
}
