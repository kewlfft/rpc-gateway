package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
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

	lagMu         sync.Mutex  // serializes checkBlockLag
	chainConflict atomic.Bool // endpoints disagree on the chain ID: path not served
	logger        *slog.Logger
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
		logger:     config.Logger,
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
	// Non-blocking; one shared epoch keeps all checkers of the path in step,
	// staggered only by their InitialDelay
	epoch := time.Now()
	for _, checker := range h.checkers {
		checker.startAt(ctx, epoch)
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

// checkBlockLag taints every endpoint whose latest block is more than
// BlockDiffThreshold behind the highest block of the current check cycle.
//
// All checks of a path fire together, so an endpoint that answers before the
// leader must be judged again once the leader's block arrives: every fresh
// sample is re-evaluated on every update. Samples older than the freshness
// window (previous cycles, tainted endpoints that skip checks) are ignored.
// When a sample was taken before the top one, the blocks the chain produced
// in between (at the top endpoint's own measured block rate) are allowed, so
// fast chains don't taint an endpoint just for answering first. HTTP and
// WebSocket endpoints of the same provider are judged separately.
func (h *HealthCheckManager) checkBlockLag(now time.Time) {
	h.lagMu.Lock()
	defer h.lagMu.Unlock()

	if h.checkNetworks() {
		return // blocks of different chains can't be compared
	}
	window := h.timeout + time.Second // covers one cycle: 500 ms WS stagger + response time
	fresh := make([]*HealthChecker, 0, len(h.checkers))
	samples := make([]blockSample, 0, len(h.checkers))
	var top blockSample
	for _, c := range h.checkers {
		s := c.sample.Load()
		if s == nil || now.Sub(s.at) > window || c.wrongChain.Load() {
			continue // stale, or on another chain: its block can't be compared
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

	for i, c := range fresh {
		s := samples[i]
		var allowance uint64
		if skew := top.at.Sub(s.at); skew > 0 {
			if !top.rateOK {
				continue // can't tell lag from answering earlier yet
			}
			allowance = uint64(math.Ceil(top.rate * skew.Seconds()))
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

// checkNetworks compares the network fingerprints the path's endpoints report
// (chain ID; genesis hash on Solana and Tron), catching a misconfigured URL
// that serves another chain or a testnet. Each provider votes once for each
// network its endpoints report, so a provider with both HTTP and WebSocket
// doesn't outweigh one with HTTP only. Endpoints off the network reported by
// a strict majority of providers are excluded; this is recomputed on every
// update, so an early vote at startup corrects itself as endpoints answer.
// With no majority, rather than guess which is wrong, the path is not served
// until the wrong URL is removed from the config: it returns true then.
// Caller holds lagMu.
func (h *HealthCheckManager) checkNetworks() bool {
	voters := make(map[string]map[string]bool, 2) // network -> providers reporting it
	providers := make(map[string]bool, len(h.checkers))
	for _, c := range h.checkers {
		n := c.Network()
		if n == "" { // not fetched yet
			continue
		}
		providers[c.config.Name] = true
		if voters[n] == nil {
			voters[n] = make(map[string]bool, len(h.checkers))
		}
		voters[n][c.config.Name] = true
	}
	var majority string
	for n, ps := range voters {
		if len(ps)*2 > len(providers) {
			majority = n
		}
	}
	conflict := majority == "" && len(voters) > 1

	for _, c := range h.checkers {
		n := c.Network()
		wrong := majority != "" && n != "" && n != majority
		if wrong != c.wrongChain.Load() {
			c.wrongChain.Store(wrong)
			if wrong {
				h.logger.Warn("provider on another network excluded: remove its URL from the config",
					"path", h.path, "name", c.config.Name, "conn", c.config.ConnectionType,
					"network", n, "majorityNetwork", majority)
				if c.onUnhealthy != nil {
					c.onUnhealthy()
				}
			} else {
				h.logger.Info("provider back on the path's network", "path", h.path,
					"name", c.config.Name, "conn", c.config.ConnectionType, "network", n)
			}
		}
	}

	if conflict != h.chainConflict.Load() {
		h.chainConflict.Store(conflict)
		if conflict {
			ns := make([]string, 0, len(h.checkers))
			for _, c := range h.checkers {
				if n := c.Network(); n != "" {
					ns = append(ns, fmt.Sprintf("%s/%s=%s", c.config.Name, c.config.ConnectionType, n))
				}
			}
			h.logger.Error("providers report different networks with no majority: not serving this path until the wrong URL is removed from the config",
				"path", h.path, "networks", ns)
		} else {
			h.logger.Info("providers' networks settled: serving this path", "path", h.path)
		}
	}
	return conflict
}

// ChainConflict reports whether the path's providers disagree on the network
// with no majority.
func (h *HealthCheckManager) ChainConflict() bool {
	return h.chainConflict.Load()
}
