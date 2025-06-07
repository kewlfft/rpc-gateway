package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

const (
	userAgent = "rpc-gateway-health-check"
)

// TaintConfig defines the taint behavior parameters
type TaintConfig struct {
	InitialWaitTime    time.Duration
	MaxWaitTime        time.Duration
	ResetWaitDuration  time.Duration
	Reason            string
}

var (
	// Health check taint configuration
	healthCheckTaintConfig = TaintConfig{
		InitialWaitTime:   time.Second * 30,
		MaxWaitTime:       time.Minute * 10,
		ResetWaitDuration: time.Minute * 5,
		Reason:           "health check failure",
	}

	// HTTP request taint configuration (faster cycle)
	httpTaintConfig = TaintConfig{
		InitialWaitTime:   time.Second * 2,
		MaxWaitTime:       time.Second * 30,
		ResetWaitDuration: time.Second * 15,
		Reason:           "HTTP error",
	}
)

// TaintState represents the current state of a health checker
type TaintState struct {
	isTainted bool
	lastRemoval time.Time
	waitTime time.Duration
	removalTimer *time.Timer
	config TaintConfig
}

type HealthCheckerConfig struct {
	URL    string
	Name   string // identifier imported from RPC gateway config
	Logger *slog.Logger

	// How often to check health.
	Interval time.Duration `yaml:"healthcheckInterval"`

	// How long to wait for responses before failing
	Timeout time.Duration `yaml:"healthcheckTimeout"`

	// Maximum allowed block difference between providers
	BlockDiffThreshold uint `yaml:"blockDiffThreshold"`

	// Path information
	Path string
}

// BlockNumberUpdateCallback is called when a health checker successfully updates its block number
type BlockNumberUpdateCallback func(blockNumber uint64)

type HealthChecker struct {
	config              HealthCheckerConfig
	client              *rpc.Client
	httpClient          *http.Client
	blockNumber        uint64
	gasLeft            uint64
	mu                 sync.RWMutex // Only for blockNumber, gasLeft, and taint state
	timer              *time.Timer
	postponeCh         chan struct{}
	stopCh            chan struct{}
	taintRemoveCh      chan struct{}
	stopped           bool

	// Taint state
	taint TaintState

	// callback function to be called when block number is updated
	onBlockNumberUpdate BlockNumberUpdateCallback
}

func NewHealthChecker(config HealthCheckerConfig) (*HealthChecker, error) {
	client, err := rpc.Dial(config.URL)
	if err != nil {
		return nil, err
	}

	client.SetHeader("User-Agent", userAgent)

	healthchecker := &HealthChecker{
		config:     config,
		client:     client,
		httpClient: &http.Client{},
		postponeCh: make(chan struct{}, 1),
		stopCh:     make(chan struct{}),
		taintRemoveCh: make(chan struct{}, 1),
		taint: TaintState{
			waitTime: healthCheckTaintConfig.InitialWaitTime,
			config:   healthCheckTaintConfig,
		},
	}

	healthchecker.config.Logger.Debug("Health checker created", 
		"provider", config.Name, 
		"url", config.URL,
		"path", config.Path)

	return healthchecker, nil
}

func (h *HealthChecker) Name() string {
	return h.config.Name
}

func (h *HealthChecker) checkBlockNumber(c context.Context) (uint64, error) {
	// First we check the block number reported by the node. This is later
	// used to evaluate a single RPC node against others
	var blockNumber hexutil.Uint64

	err := h.client.CallContext(c, &blockNumber, "eth_blockNumber")
	if err != nil {
		h.config.Logger.Error("could not fetch block number", 
			"error", err,
			"provider", h.config.Name,
			"path", h.config.Path)
		return 0, err
	}
	h.config.Logger.Debug("fetch block number completed", 
		"nodeprovider", h.config.Name, 
		"blockNumber", uint64(blockNumber), 
		"path", h.config.Path)

	return uint64(blockNumber), nil
}

// checkGasLeft performs an `eth_call` with a GasLeft.sol contract call. We also
// want to perform an eth_call to make sure eth_call requests are also succeding
// as blockNumber can be either cached or routed to a different service on the
// RPC provider's side.
func (h *HealthChecker) checkGasLeft(c context.Context) (uint64, error) {
	gasLeft, err := performGasLeftCall(c, h.httpClient, h.config.URL)
	if err != nil {
		h.config.Logger.Error("could not fetch gas left", 
			"error", err,
			"provider", h.config.Name,
			"path", h.config.Path)
		return gasLeft, err
	}
	h.config.Logger.Debug("fetch gas left completed", 
		"nodeprovider", h.config.Name, 
		"gasLeft", gasLeft, 
		"path", h.config.Path)
	return gasLeft, nil
}

// CheckAndSetHealth makes the following calls
// - `eth_blockNumber` - to get the latest block reported by the node
// - `eth_call` - to get the gas left
// And sets the health status based on the responses.
func (h *HealthChecker) CheckAndSetHealth() {
	if h.IsTainted() {
		return
	}
	go h.checkAndSetBlockNumberHealth()
	go h.checkAndSetGasLeftHealth()
}

// SetBlockNumberUpdateCallback sets the callback function to be called when block number is updated.
func (h *HealthChecker) SetBlockNumberUpdateCallback(callback BlockNumberUpdateCallback) {
	h.mu.Lock()
	h.onBlockNumberUpdate = callback
	h.mu.Unlock()
}

func (h *HealthChecker) checkAndSetBlockNumberHealth() {
	ctx, cancel := context.WithTimeout(context.Background(), h.config.Timeout)
	defer cancel()

	blockNumber, err := h.checkBlockNumber(ctx)
	if err != nil {
		h.config.Logger.Info("provider tainted due to block number check failure",
			"provider", h.config.Name,
			"error", err,
			"path", h.config.Path)
		h.TaintHealthCheck()
		return
	}

	h.mu.Lock()
	h.blockNumber = blockNumber
	callback := h.onBlockNumberUpdate
	h.mu.Unlock()

	if callback != nil {
		callback(blockNumber)
	}
}

func (h *HealthChecker) checkAndSetGasLeftHealth() {
	c, cancel := context.WithTimeout(context.Background(), h.config.Timeout)
	defer cancel()

	gasLeft, err := h.checkGasLeft(c)
	if err != nil {
		h.config.Logger.Info("provider tainted due to gas left check failure",
			"provider", h.config.Name,
			"error", err,
			"path", h.config.Path)
			h.TaintHealthCheck()
			return
	}
	h.mu.Lock()
	h.gasLeft = gasLeft
	h.mu.Unlock()
}

// PostponeCheck resets the health check timer when a request is made
func (h *HealthChecker) PostponeCheck() {
	select {
	case h.postponeCh <- struct{}{}:
	default:
	}
}

func (h *HealthChecker) Start(c context.Context) {
	// Do an immediate health check on startup
	h.CheckAndSetHealth()

	// Create a timer that will fire when we should do the next check
	timer := time.NewTimer(h.config.Interval)
	defer timer.Stop()

	for {
		select {
		case <-c.Done():
			h.mu.Lock()
			if !h.stopped {
				close(h.stopCh)
				h.stopped = true
			}
			h.mu.Unlock()
			return
		case <-timer.C:
			h.CheckAndSetHealth()
			timer.Reset(h.config.Interval)
		case <-h.postponeCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(h.config.Interval)
		case <-h.taintRemoveCh:
			// Clean up taint removal timer
			h.mu.Lock()
			if h.taint.removalTimer != nil {
				h.taint.removalTimer.Stop()
				h.taint.removalTimer = nil
			}
			h.mu.Unlock()
		}
	}
}

func (h *HealthChecker) Stop(_ context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if !h.stopped {
		close(h.stopCh)
		h.stopped = true
		// Signal cleanup of taint removal timer
		select {
		case h.taintRemoveCh <- struct{}{}:
		default:
		}
	}
	return nil
}

func (h *HealthChecker) IsHealthy() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	// Being tainted is equivalent to unhealthy
	return !h.taint.isTainted
}

func (h *HealthChecker) IsTainted() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.taint.isTainted
}

// Taint marks the provider as unhealthy for a configurable duration
func (h *HealthChecker) Taint(config TaintConfig) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.taint.isTainted {
		return
	}

	// Update taint configuration
	h.taint.config = config

	// Calculate new wait time
	if time.Since(h.taint.lastRemoval) <= config.ResetWaitDuration {
		h.taint.waitTime *= 2
		if h.taint.waitTime > config.MaxWaitTime {
			h.taint.waitTime = config.MaxWaitTime
		}
	} else {
		h.taint.waitTime = config.InitialWaitTime
	}

	// Stop any existing removal timer
	if h.taint.removalTimer != nil {
		h.taint.removalTimer.Stop()
	}

	// Set taint state
	h.taint.isTainted = true
	h.taint.removalTimer = time.AfterFunc(h.taint.waitTime, func() {
		h.RemoveTaint()
		// Signal cleanup
		select {
		case h.taintRemoveCh <- struct{}{}:
		default:
		}
	})

	h.config.Logger.Info("provider tainted", 
		"path", h.config.Path,
		"name", h.config.Name,
		"reason", config.Reason,
		"waitTime", h.taint.waitTime.Seconds(),
		"nextRemoval", time.Now().Add(h.taint.waitTime))
}

// TaintHTTP is a convenience method that uses the HTTP-specific taint configuration
func (h *HealthChecker) TaintHTTP() {
	h.Taint(httpTaintConfig)
}

// TaintHealthCheck is a convenience method that uses the health check taint configuration
func (h *HealthChecker) TaintHealthCheck() {
	h.Taint(healthCheckTaintConfig)
}

func (h *HealthChecker) RemoveTaint() {
	h.mu.Lock()
	defer h.mu.Unlock()
	
	if !h.taint.isTainted {
		return
	}

	h.taint.isTainted = false
	h.taint.lastRemoval = time.Now()
	h.taint.removalTimer = nil
	
	h.config.Logger.Info("taint removed", 
		"path", h.config.Path,
		"name", h.config.Name,
		"nextTaintWait", h.taint.waitTime.Seconds())
}

func (h *HealthChecker) BlockNumber() uint64 {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return h.blockNumber
}

func (h *HealthChecker) GasLeft() uint64 {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return h.gasLeft
}
