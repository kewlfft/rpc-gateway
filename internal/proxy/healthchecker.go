package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	userAgent = "rpc-gateway-health-check"
)

var bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// JSON-RPC response structure
type JSONRPCResponse struct {
	Result any `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// makeJSONRPCCall makes an optimized JSON-RPC call
func (h *HealthChecker) makeJSONRPCCall(ctx context.Context, method string, params []any) (*JSONRPCResponse, error) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		// Ensure buffer is always returned to pool, even on panic
		bufPool.Put(buf)
	}()

	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)

	if err := enc.Encode(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{"2.0", 1, method, params}); err != nil {
		return nil, fmt.Errorf("failed to encode request: %w", err)
	}

	bodyData := bytes.Clone(buf.Bytes())

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.config.URL, bytes.NewReader(bodyData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)

	// Add custom headers if configured
	for key, value := range h.config.Headers {
		req.Header.Set(key, value)
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var out JSONRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("RPC error: %s", out.Error.Message)
	}
	return &out, nil
}

// parseHex is defined in healthchecker_utils.go

// TaintConfig defines the taint behavior parameters
type TaintConfig struct {
	InitialWaitTime   time.Duration
	MaxWaitTime       time.Duration
	ResetWaitDuration time.Duration
	Reason            string
}

var (
	// Health check taint configuration
	healthCheckTaintConfig = TaintConfig{
		InitialWaitTime:   time.Second * 30,
		MaxWaitTime:       time.Minute * 20,
		ResetWaitDuration: time.Minute * 5,
		Reason:            "health check failure",
	}

	// HTTP request taint configuration (faster cycle)
	httpTaintConfig = TaintConfig{
		InitialWaitTime:   time.Second * 10,
		MaxWaitTime:       time.Second * 60,
		ResetWaitDuration: time.Second * 20,
		Reason:            "HTTP error",
	}
)

// TaintState represents the current state of a health checker
type TaintState struct {
	lastRemoval  time.Time
	waitTime     time.Duration
	removalTimer *time.Timer
	config       TaintConfig
}

type HealthCheckerConfig struct {
	Logger             *slog.Logger
	URL                string
	Name               string
	Interval           time.Duration
	Timeout            time.Duration
	Path               string
	ChainType          string
	ConnectionType     string
	BlockDiffThreshold uint
	Headers            map[string]string
	InitialDelay       time.Duration // Add initial delay to config
}

// BlockNumberUpdateCallback is called when a health checker successfully updates its block number
type BlockNumberUpdateCallback func(blockNumber uint64)

type HealthChecker struct {
	config          HealthCheckerConfig
	httpClient      *http.Client
	blockNumber     atomic.Uint64
	gasCheckCounter atomic.Uint32
	mu              sync.RWMutex // Only for taint state
	taintRemoveCh   chan struct{}
	isTainted       atomic.Bool

	// Taint state
	taint TaintState

	// callback function to be called when block number is updated
	onBlockNumberUpdate atomic.Value

	// callback function to be called before tainting (for cleanup)
	onBeforeTaint atomic.Value
}

func NewHealthChecker(config HealthCheckerConfig) (*HealthChecker, error) {
	// Set default chain type if not specified
	if config.ChainType == "" {
		config.ChainType = "evm"
	}

	// Set default connection type if not specified
	if config.ConnectionType == "" {
		config.ConnectionType = "http"
	}

	// Create optimized HTTP client for health checks using shared connection pool
	// All providers in the same proxy path share one HTTP client and connection pool
	httpClient := CreateHealthCheckHTTPClientForProxy(config.Path, config.Name, config.Timeout)

	healthchecker := &HealthChecker{
		config:        config,
		httpClient:    httpClient,
		taintRemoveCh: make(chan struct{}, 1),
		taint: TaintState{
			config:   healthCheckTaintConfig,
			waitTime: healthCheckTaintConfig.InitialWaitTime,
		},
	}

	healthchecker.config.Logger.Debug("Health checker created",
		"provider", config.Name,
		"url", config.URL,
		"path", config.Path,
		"chainType", config.ChainType,
		"connectionType", config.ConnectionType,
		"headers_count", len(config.Headers))

	return healthchecker, nil
}

func (h *HealthChecker) Name() string {
	return h.config.Name
}

// checkSolanaSlotViaWebSocket performs Solana slot subscription via WebSocket
func (h *HealthChecker) checkSolanaSlotViaWebSocket(conn *websocket.Conn) (uint64, error) {
	if err := conn.WriteJSON(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "slotSubscribe", "params": json.RawMessage("[]"),
	}); err != nil {
		return 0, fmt.Errorf("slotSubscribe failed: %w", err)
	}

	var subResp struct{ Result any }
	if err := conn.ReadJSON(&subResp); err != nil {
		return 0, fmt.Errorf("subscription response failed: %w", err)
	}
	subID := subResp.Result

	var msg map[string]any
	if err := conn.ReadJSON(&msg); err != nil {
		if websocket.IsUnexpectedCloseError(err) {
			return 0, fmt.Errorf("websocket closed unexpectedly: %w", err)
		}
		if err == websocket.ErrCloseSent {
			return 0, fmt.Errorf("websocket close sent: %w", err)
		}
		return 0, fmt.Errorf("slot notification failed: %w", err)
	}

	method, _ := msg["method"].(string)
	if method != "slotNotification" {
		return 0, fmt.Errorf("unexpected notification method: %s", method)
	}

	params, _ := msg["params"].(map[string]any)
	result, _ := params["result"].(map[string]any)
	slot, ok := result["slot"].(float64)
	if !ok {
		return 0, fmt.Errorf("invalid slot format")
	}

	// Unsubscribe
	_ = conn.WriteJSON(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "slotUnsubscribe", "params": []any{subID},
	})
	_ = conn.ReadJSON(&map[string]any{}) // discard response

	return uint64(slot), nil
}

// checkEVMBlockNumberViaWebSocket performs EVM block number check via WebSocket
func (h *HealthChecker) checkEVMBlockNumberViaWebSocket(conn *websocket.Conn) (uint64, error) {
	if err := conn.WriteJSON(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "eth_blockNumber", "params": []any{},
	}); err != nil {
		return 0, fmt.Errorf("eth_blockNumber request failed: %w", err)
	}

	var resp struct {
		Result string `json:"result"`
	}
	if err := conn.ReadJSON(&resp); err != nil {
		return 0, fmt.Errorf("eth_blockNumber response failed: %w", err)
	}

	blockNumber, err := parseHex(resp.Result)
	if err != nil {
		return 0, fmt.Errorf("failed to parse block number: %w", err)
	}
	return blockNumber, nil
}

func (h *HealthChecker) checkBlockNumber(ctx context.Context) (uint64, error) {
	var blockNumber uint64

	switch {
	case h.config.ConnectionType == "websocket":
		// Create a custom dialer with timeout from config
		dialer := websocket.Dialer{
			HandshakeTimeout:  h.config.Timeout,
			ReadBufferSize:    16384,
			WriteBufferSize:   16384,
			EnableCompression: true,
		}

		conn, resp, err := dialer.DialContext(ctx, h.config.URL, nil)
		if err != nil {
			// Log health check failure concisely - these are expected during provider outages
			var statusCode int
			if resp != nil {
				statusCode = resp.StatusCode
			}
			h.config.Logger.Info("WebSocket health check failed",
				"error", err,
				"provider", h.config.Name,
				"path", h.config.Path,
				"status", statusCode,
			)
			return 0, err
		}
		defer conn.Close()

		// Set deadline based on context timeout
		deadline, ok := ctx.Deadline()
		if !ok {
			deadline = time.Now().Add(h.config.Timeout)
		}
		conn.SetReadDeadline(deadline)

		if h.config.ChainType == "solana" {
			blockNumber, err = h.checkSolanaSlotViaWebSocket(conn)
			if err != nil {
				return 0, err
			}
		} else {
			blockNumber, err = h.checkEVMBlockNumberViaWebSocket(conn)
			if err != nil {
				return 0, err
			}
		}

	case h.config.ChainType == "solana":
		// Make optimized JSON-RPC call
		rpcResp, err := h.makeJSONRPCCall(ctx, "getSlot", []any{map[string]string{"commitment": "processed"}})
		if err != nil {
			return 0, err
		}

		// Parse result to uint64
		result, ok := rpcResp.Result.(float64)
		if !ok {
			return 0, fmt.Errorf("invalid result type for getSlot")
		}
		blockNumber = uint64(result)

	case h.config.ChainType == "tron":
		var response struct {
			BlockID     string `json:"blockID"`
			BlockHeader struct {
				RawData struct {
					Number uint64 `json:"number"`
				} `json:"raw_data"`
			} `json:"block_header"`
		}

		// Create a POST request with empty body
		req, err := http.NewRequestWithContext(ctx, "POST", h.config.URL+"/wallet/getnowblock", nil)
		if err != nil {
			return 0, fmt.Errorf("failed to create request: %w", err)
		}

		// Add custom headers if configured
		for key, value := range h.config.Headers {
			req.Header.Set(key, value)
		}

		// Send the request
		resp, err := h.httpClient.Do(req)
		if err != nil {
			return 0, fmt.Errorf("failed to send request: %w", err)
		}
		defer resp.Body.Close()

		// Check response status
		if resp.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
		}

		// Decode response
		if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
			return 0, fmt.Errorf("failed to decode response: %w", err)
		}

		blockNumber = response.BlockHeader.RawData.Number

	default:
		// Make optimized JSON-RPC call for EVM chains
		rpcResp, err := h.makeJSONRPCCall(ctx, "eth_blockNumber", []any{})
		if err != nil {
			return 0, err
		}

		// Parse hex result to uint64
		result, ok := rpcResp.Result.(string)
		if !ok {
			return 0, fmt.Errorf("invalid result type for eth_blockNumber")
		}

		blockNumber, err = parseHex(result)
		if err != nil {
			return 0, fmt.Errorf("failed to parse block number: %w", err)
		}
	}

	// Common debug log
	h.config.Logger.Debug("block number fetched",
		"connectionType", h.config.ConnectionType,
		"provider", h.config.Name,
		"blockNumber", blockNumber,
		"path", h.config.Path)

	return blockNumber, nil
}

// checkGasLeft performs an `eth_call` with a GasLeft.sol contract call. We also
// want to perform an eth_call to make sure eth_call requests are also succeding
// as blockNumber can be either cached or routed to a different service on the
// RPC provider's side.
func (h *HealthChecker) checkGasLeft(c context.Context) (uint64, error) {
	// Skip gas left check for non-EVM chains or WebSocket connections
	if h.config.ChainType != "evm" || h.config.ConnectionType == "websocket" {
		return 0, nil
	}

	gasLeft, err := performGasLeftCall(c, h.httpClient, h.config.URL)
	if err != nil {
		h.config.Logger.Error("gas call failed",
			"connectionType", h.config.ConnectionType,
			"error", err,
			"provider", h.config.Name,
			"path", h.config.Path)
		return gasLeft, err
	}
	h.config.Logger.Debug("gas left fetched",
		"connectionType", h.config.ConnectionType,
		"provider", h.config.Name,
		"gasLeft", gasLeft,
		"path", h.config.Path)
	return gasLeft, nil
}

// CheckAndSetHealth makes the following calls
// - `eth_blockNumber` - to get the latest block reported by the node
// - `eth_call` - to get the gas left (runs sequentially after block number check)
// And sets the health status based on the responses.
func (h *HealthChecker) CheckAndSetHealth() {
	// Fast path: if tainted, skip entirely (atomic read, very fast)
	if h.IsTainted() {
		return
	}

	// Run block number check first, then gas left check sequentially
	// This reduces concurrent load and prevents wasting requests if block number fails
	h.checkAndSetBlockNumberHealth()
	// Only run gas left check after block number completes successfully
	if !h.IsTainted() {
		h.checkAndSetGasLeftHealth()
	}
}

// SetBlockNumberUpdateCallback sets the callback function to be called when block number is updated.
func (h *HealthChecker) SetBlockNumberUpdateCallback(callback BlockNumberUpdateCallback) {
	h.onBlockNumberUpdate.Store(callback)
}

// SetBeforeTaintCallback sets the callback function to be called before tainting.
func (h *HealthChecker) SetBeforeTaintCallback(callback func()) {
	h.onBeforeTaint.Store(callback)
}

func (h *HealthChecker) checkAndSetBlockNumberHealth() {
	ctx, cancel := context.WithTimeout(context.Background(), h.config.Timeout)
	defer cancel()

	blockNumber, err := h.checkBlockNumber(ctx)
	if err != nil {
		// Detailed cause/error at DEBUG; state change is logged inside TaintHealthCheck.
		h.config.Logger.Debug("provider tainted due to block number check failure",
			"connectionType", h.config.ConnectionType,
			"provider", h.config.Name,
			"error", err,
			"path", h.config.Path)
		h.TaintHealthCheck()
		return
	}

	h.blockNumber.Store(blockNumber)

	if callback, ok := h.onBlockNumberUpdate.Load().(BlockNumberUpdateCallback); ok && callback != nil {
		callback(blockNumber)
	}
}

func (h *HealthChecker) checkAndSetGasLeftHealth() {
	// Skip gas left check for non-EVM chains
	if h.config.ChainType != "evm" {
		return
	}

	// Run gas check only on every second health cycle, starting from the second one.
	// Sequence per provider: 1st call -> skip, 2nd -> run, 3rd -> skip, 4th -> run, ...
	if h.gasCheckCounter.Add(1)%2 != 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), h.config.Timeout)
	defer cancel()

	_, err := h.checkGasLeft(ctx)
	if err != nil {
		// Detailed cause/error at DEBUG; state change is logged inside TaintHealthCheck.
		h.config.Logger.Debug("provider tainted due to gas left check failure",
			"connectionType", h.config.ConnectionType,
			"provider", h.config.Name,
			"error", err,
			"path", h.config.Path)
		h.TaintHealthCheck()
		return
	}
}

func (h *HealthChecker) Start(c context.Context) {
	// Use the provided initial delay from config
	h.config.Logger.Debug("starting health checker with initial delay",
		"initialDelayMs", h.config.InitialDelay.Milliseconds(),
		"connectionType", h.config.ConnectionType,
		"provider", h.config.Name,
		"path", h.config.Path)

	// Start the health checker in a separate goroutine to avoid blocking
	go h.runHealthChecker(c)
}

// runHealthChecker runs the main health checking loop in a separate goroutine
func (h *HealthChecker) runHealthChecker(c context.Context) {
	timer := time.NewTimer(h.config.InitialDelay)
	defer timer.Stop()

	for {
		select {
		case <-c.Done():
			h.config.Logger.Info("health checker shutting down gracefully",
				"provider", h.config.Name,
				"path", h.config.Path)
			return
		case <-timer.C:
			h.CheckAndSetHealth()
			timer.Reset(h.config.Interval)
		case <-h.taintRemoveCh:
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
	// Signal cleanup of taint removal timer
	select {
	case h.taintRemoveCh <- struct{}{}:
	default:
	}
	return nil
}

func (h *HealthChecker) IsHealthy() bool {
	return !h.IsTainted()
}

func (h *HealthChecker) IsTainted() bool {
	return h.isTainted.Load()
}

// Taint marks the provider as tainted with backoff.
func (h *HealthChecker) Taint(cfg TaintConfig) {
	// Fast path: if already tainted, skip repeated taint work and logs
	if h.isTainted.Load() {
		return
	}

	// Only call cleanup callback on first tainting
	if h.config.ConnectionType == "websocket" {
		if callback, ok := h.onBeforeTaint.Load().(func()); ok && callback != nil {
			callback()
		}
	}

	// Immediate atomic taint
	h.isTainted.Store(true)

	// Update taint state under lock
	h.mu.Lock()

	// Calculate timing values
	now := time.Now()
	var wait time.Duration
	if time.Since(h.taint.lastRemoval) <= cfg.ResetWaitDuration {
		wait = min(h.taint.waitTime*2, cfg.MaxWaitTime)
	} else {
		wait = cfg.InitialWaitTime
	}

	// Cancel old timer safely
	if oldTimer := h.taint.removalTimer; oldTimer != nil {
		if !oldTimer.Stop() {
			select {
			case <-oldTimer.C:
			default:
			}
		}
	}

	// Assign new timer and config
	h.taint.removalTimer = time.AfterFunc(wait, func() {
		h.RemoveTaint()
		select {
		case h.taintRemoveCh <- struct{}{}:
		default:
		}
	})
	h.taint.config = cfg
	h.taint.lastRemoval = now
	h.taint.waitTime = wait
	h.mu.Unlock()

	// Logging
	nextRetry := now.Add(wait)
	h.config.Logger.Info("provider tainted",
		"conn", h.config.ConnectionType,
		"name", h.config.Name,
		"path", h.config.Path,
		"reason", cfg.Reason,
		"retry_sec", wait.Seconds(),
		"next_retry", nextRetry,
	)
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
	// Update atomic state
	h.isTainted.Store(false)

	// Update taint state under lock
	h.mu.Lock()
	h.taint.lastRemoval = time.Now()
	h.taint.removalTimer = nil
	nextWait := h.taint.waitTime
	h.mu.Unlock()

	// Log after all state updates are complete, using the captured wait time
	h.config.Logger.Info("taint removed",
		"connectionType", h.config.ConnectionType,
		"path", h.config.Path,
		"name", h.config.Name,
		"nextTaintWait", nextWait.Seconds())
}

func (h *HealthChecker) BlockNumber() uint64 {
	return h.blockNumber.Load()
}
