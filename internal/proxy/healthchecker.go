package proxy

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
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

// JSON-RPC response structure
type JSONRPCResponse struct {
	Result any           `json:"result"`
	Error  *JSONRPCError `json:"error,omitempty"`
}

type jsonRPCRequest[P any] struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  P      `json:"params"`
}

// makeJSONRPCCall makes an optimized JSON-RPC call (generic params via Go 1.27 methods).
func (h *HealthChecker) makeJSONRPCCall[P any](ctx context.Context, method string, params P) (*JSONRPCResponse, error) {
	bodyData, err := jsonv2.Marshal(jsonRPCRequest[P]{"2.0", 1, method, params}, jsontext.EscapeForHTML(false))
	if err != nil {
		return nil, fmt.Errorf("failed to encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.config.URL, bytes.NewReader(bodyData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)

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
		return nil, statusError(resp)
	}

	var out JSONRPCResponse
	if err := jsonv2.UnmarshalRead(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("RPC error: %s", out.Error.Message)
	}
	return &out, nil
}

// TaintConfig defines the taint behavior parameters
type TaintConfig struct {
	InitialWaitTime   time.Duration
	MaxWaitTime       time.Duration
	ResetWaitDuration time.Duration
	Reason            string
	Detail            string        // optional; logged at INFO when provider is tainted
	Wait              time.Duration // optional provider hint (Retry-After); overrides backoff
	Probe             bool          // on expiry, untaint only if a health probe succeeds
	Recheck           bool          // no timer: lifted only by a passing scheduled check after the wait
}

var (
	// Health check taint configuration
	healthCheckTaintConfig = TaintConfig{
		InitialWaitTime:   time.Second * 30,
		MaxWaitTime:       time.Minute * 20,
		ResetWaitDuration: time.Minute * 5,
		Reason:            "health check failure",
		Recheck:           true,
	}

	// HTTP request taint configuration (faster cycle)
	httpTaintConfig = TaintConfig{
		InitialWaitTime:   time.Second * 10,
		MaxWaitTime:       time.Second * 60,
		ResetWaitDuration: time.Second * 20,
		Reason:            "HTTP error",
	}

	// HTTP 429: most limits are per-second windows, so start short. Recovery is
	// confirmed by a background probe, so live requests never hit a provider that
	// is still limiting; each failed probe doubles the wait.
	rateLimitTaintConfig = TaintConfig{
		InitialWaitTime:   time.Second,
		MaxWaitTime:       2 * time.Minute,
		ResetWaitDuration: 20 * time.Second,
		Reason:            "rate limited",
		Probe:             true,
	}
)

// TaintState represents the current state of a health checker
type TaintState struct {
	lastRemoval  time.Time // last taint or removal, for backoff resets
	waitTime     time.Duration
	removalTimer *time.Timer
	cfg          TaintConfig // of the current taint
	until        time.Time   // Recheck taints: earliest scheduled check that may lift it
	gen          uint64      // bumped on every taint change; stale timers compare it
}

type HealthCheckerConfig struct {
	Logger         *slog.Logger
	URL            string
	Name           string
	Interval       time.Duration
	Timeout        time.Duration
	Path           string
	ChainType      string
	ConnectionType string
	Headers        map[string]string
	InitialDelay   time.Duration
}

// BlockNumberUpdateCallback is called after a health checker records a new block sample
type BlockNumberUpdateCallback func()

// blockSample is a block number, the chain ID reported with it (EVM only,
// 0 otherwise) and when the block number was received.
type blockSample struct {
	block   uint64
	chainID uint64
	at      time.Time
	rate    float64 // blocks/s from this endpoint's earlier samples, if rateOK
	rateOK  bool
}

// minRateSpan is the history an endpoint's block rate is measured over.
const minRateSpan = 10 * time.Second

// recordSample stores s, deriving the endpoint's block rate from the previous
// sample when at least minRateSpan apart (else keeping the previous rate).
func (h *HealthChecker) recordSample(s blockSample) {
	if prev := h.sample.Load(); prev != nil {
		if span := s.at.Sub(prev.at); span >= minRateSpan && s.block >= prev.block {
			s.rate, s.rateOK = float64(s.block-prev.block)/span.Seconds(), true
		} else if span < minRateSpan {
			s.rate, s.rateOK = prev.rate, prev.rateOK
		}
	}
	h.sample.Store(&s)
}

type HealthChecker struct {
	config              HealthCheckerConfig
	httpClient          *http.Client
	sample              atomic.Pointer[blockSample] // latest block number; nil until the first check
	chainID             atomic.Uint64               // EVM chain ID, fetched once (0 = not yet)
	ethCallCheckCounter atomic.Uint32
	mu                  sync.Mutex // Only for taint state
	isTainted           atomic.Bool
	wrongChain          atomic.Bool // chain ID differs from the path's majority: excluded, set by the manager

	// Taint state
	taint TaintState

	// Called after each successful block number update; set before Start
	onBlockNumberUpdate BlockNumberUpdateCallback
}

func NewHealthChecker(config HealthCheckerConfig) *HealthChecker {
	config.ChainType = cmp.Or(config.ChainType, "evm")
	config.ConnectionType = cmp.Or(config.ConnectionType, "http")

	// Share the same connection pool as user requests for this proxy path
	httpClient := CreateOptimizedHTTPClient("proxy-"+config.Path, config.Timeout)

	h := &HealthChecker{
		config:     config,
		httpClient: httpClient,
		taint: TaintState{
			waitTime: healthCheckTaintConfig.InitialWaitTime,
		},
	}
	h.config.Logger.Debug("Health checker created",
		"provider", config.Name,
		"url", config.URL,
		"path", config.Path,
		"chainType", config.ChainType,
		"connectionType", config.ConnectionType,
		"headers_count", len(config.Headers))
	return h
}

// checkSolanaSlotViaWebSocket performs Solana slot subscription via WebSocket
func (h *HealthChecker) checkSolanaSlotViaWebSocket(conn *websocket.Conn) (uint64, error) {
	if err := conn.WriteJSON(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "slotSubscribe", "params": []any{},
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
		if errors.Is(err, websocket.ErrCloseSent) {
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

// wsCall sends one JSON-RPC request on conn and returns its hex result as uint64.
func wsCall(conn *websocket.Conn, id int, method string) (uint64, error) {
	if err := conn.WriteJSON(jsonRPCRequest[[]any]{"2.0", id, method, []any{}}); err != nil {
		return 0, fmt.Errorf("%s request failed: %w", method, err)
	}
	var resp struct {
		Result string        `json:"result"`
		Error  *JSONRPCError `json:"error"`
	}
	if err := conn.ReadJSON(&resp); err != nil {
		return 0, fmt.Errorf("%s response failed: %w", method, err)
	}
	if resp.Error != nil {
		return 0, fmt.Errorf("%s RPC error: %s", method, resp.Error.Message)
	}
	v, err := parseHex(resp.Result)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid result %q: %w", method, resp.Result, err)
	}
	return v, nil
}

// checkEVMBlockNumberViaWebSocket fetches the block number, and the chain ID
// on the same connection when asked
func (h *HealthChecker) checkEVMBlockNumberViaWebSocket(conn *websocket.Conn, withChainID bool) (blockSample, error) {
	block, err := wsCall(conn, 1, "eth_blockNumber")
	if err != nil {
		return blockSample{}, err
	}
	at := time.Now()
	if withChainID {
		id, err := wsCall(conn, 2, "eth_chainId")
		if err != nil {
			return blockSample{}, err
		}
		h.chainID.Store(id)
	}
	return blockSample{block: block, chainID: h.chainID.Load(), at: at}, nil
}

// fetchHexResult makes an HTTP JSON-RPC call without params and parses its hex result.
func (h *HealthChecker) fetchHexResult(ctx context.Context, method string) (uint64, error) {
	rpcResp, err := h.makeJSONRPCCall(ctx, method, []any{})
	if err != nil {
		return 0, err
	}
	result, ok := rpcResp.Result.(string)
	if !ok {
		return 0, fmt.Errorf("invalid result type for %s", method)
	}
	v, err := parseHex(result)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid result %q: %w", method, result, err)
	}
	return v, nil
}

// blockCheckMethod names the request checkBlockNumber makes, for logs.
func (h *HealthChecker) blockCheckMethod() string {
	switch {
	case h.config.ChainType == "solana" && h.config.ConnectionType == "websocket":
		return "slotSubscribe"
	case h.config.ChainType == "solana":
		return "getSlot"
	case h.config.ChainType == "tron":
		return "wallet/getnowblock"
	}
	return "eth_blockNumber"
}

// checkBlockNumber fetches the endpoint's latest block. On EVM the sample
// carries the endpoint's chain ID, which is a constant: it is requested only
// when withChainID is set or it is not known yet, to spare rate limits.
func (h *HealthChecker) checkBlockNumber(ctx context.Context, withChainID bool) (blockSample, error) {
	withChainID = withChainID || h.chainID.Load() == 0
	var sample blockSample

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
			// The status says why a handshake failed (e.g. 403 IP blocked, 429 rate limited)
			if resp != nil {
				if resp.StatusCode == http.StatusTooManyRequests {
					return sample, fmt.Errorf("%w (%w)", err, statusError(resp))
				}
				return sample, fmt.Errorf("%w (HTTP %d)", err, resp.StatusCode)
			}
			return sample, err
		}
		defer conn.Close()

		// Set deadline based on context timeout
		deadline, ok := ctx.Deadline()
		if !ok {
			deadline = time.Now().Add(h.config.Timeout)
		}
		conn.SetReadDeadline(deadline)

		if h.config.ChainType == "solana" {
			sample.block, err = h.checkSolanaSlotViaWebSocket(conn)
			sample.at = time.Now()
		} else {
			sample, err = h.checkEVMBlockNumberViaWebSocket(conn, withChainID)
		}
		if err != nil {
			return blockSample{}, err
		}

	case h.config.ChainType == "solana":
		// Make optimized JSON-RPC call
		rpcResp, err := h.makeJSONRPCCall(ctx, "getSlot", []any{map[string]string{"commitment": "processed"}})
		if err != nil {
			return sample, err
		}

		// Parse result to uint64
		result, ok := rpcResp.Result.(float64)
		if !ok {
			return sample, fmt.Errorf("invalid result type for getSlot")
		}
		sample = blockSample{block: uint64(result), at: time.Now()}

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
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.config.URL+"/wallet/getnowblock", nil)
		if err != nil {
			return sample, fmt.Errorf("failed to create request: %w", err)
		}

		// Add custom headers if configured
		for key, value := range h.config.Headers {
			req.Header.Set(key, value)
		}

		// Send the request
		resp, err := h.httpClient.Do(req)
		if err != nil {
			return sample, fmt.Errorf("failed to send request: %w", err)
		}
		defer resp.Body.Close()

		// Check response status
		if resp.StatusCode != http.StatusOK {
			return sample, statusError(resp)
		}

		if err := jsonv2.UnmarshalRead(resp.Body, &response); err != nil {
			return sample, fmt.Errorf("failed to decode response: %w", err)
		}

		sample = blockSample{block: response.BlockHeader.RawData.Number, at: time.Now()}

	default:
		block, err := h.fetchHexResult(ctx, "eth_blockNumber")
		if err != nil {
			return sample, err
		}
		at := time.Now()
		if withChainID {
			id, err := h.fetchHexResult(ctx, "eth_chainId")
			if err != nil {
				return sample, err
			}
			h.chainID.Store(id)
		}
		sample = blockSample{block: block, chainID: h.chainID.Load(), at: at}
	}

	// Common debug log
	h.config.Logger.Debug("block number fetched",
		"connectionType", h.config.ConnectionType,
		"provider", h.config.Name,
		"blockNumber", sample.block,
		"chainId", sample.chainID,
		"path", h.config.Path)

	return sample, nil
}

// checkEthCall performs an `eth_call` against the SHA-256 precompile (0x02) to
// verify the node executes EVM logic, as blockNumber can be either cached or
// routed to a different service on the RPC provider's side.
func (h *HealthChecker) checkEthCall(c context.Context) error {
	// Skip eth_call check for non-EVM chains or WebSocket connections
	if h.config.ChainType != "evm" || h.config.ConnectionType == "websocket" {
		return nil
	}

	if err := performEthCallHealthCheck(c, h.httpClient, h.config.URL, h.config.Headers); err != nil {
		h.config.Logger.Error("eth_call health check failed",
			"connectionType", h.config.ConnectionType,
			"error", err,
			"provider", h.config.Name,
			"path", h.config.Path)
		return err
	}
	h.config.Logger.Debug("eth_call health check ok",
		"connectionType", h.config.ConnectionType,
		"provider", h.config.Name,
		"path", h.config.Path)
	return nil
}

// CheckAndSetHealth makes the following calls
//   - `eth_blockNumber` - latest block, judged by the manager's gates; on EVM
//     `eth_chainId` too, but only on the first check and when recovering
//   - `eth_call` - SHA-256 precompile probe (runs sequentially after block number check)
//
// And sets the health status based on the responses.
//
// A health-check taint is lifted only here, by a passing check once its backoff
// has elapsed, so a recovering endpoint is judged on the shared schedule
// alongside its peers instead of coming back unverified when a timer expires.
// Endpoints under a short HTTP taint are still checked, so a dead one gets the
// long health-check taint; rate-limited ones skip checks and recover by probe.
func (h *HealthChecker) CheckAndSetHealth() {
	if h.skipCheck(time.Now()) {
		return
	}
	recovering := h.IsTainted()

	// Run block number check first, then eth_call check sequentially
	// This reduces concurrent load and prevents wasting requests if block number fails
	if !h.checkAndSetBlockNumberHealth(recovering) || h.skipCheck(time.Now()) {
		return // failed, or a gate (block lag, chain ID) tainted it
	}
	if !h.checkAndSetEthCallHealth(recovering) {
		return
	}
	h.liftRecheckTaint()
}

// skipCheck reports whether the scheduled check is skipped: a probing taint
// recovers on its own, and a recheck taint first waits out its backoff.
func (h *HealthChecker) skipCheck(now time.Time) bool {
	if !h.isTainted.Load() {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.isTainted.Load() && (h.taint.cfg.Probe || (h.taint.cfg.Recheck && now.Before(h.taint.until)))
}

// liftRecheckTaint removes a recheck taint whose backoff has elapsed; called
// after a passing check (a failed one re-arms the taint into the future).
func (h *HealthChecker) liftRecheckTaint() {
	h.mu.Lock()
	if !h.isTainted.Load() || !h.taint.cfg.Recheck || time.Now().Before(h.taint.until) {
		h.mu.Unlock()
		return
	}
	h.removeTaintLocked()
	h.mu.Unlock()
	h.logRemoved()
}

// SetBlockNumberUpdateCallback sets the function called after each block number update.
// It must be called before Start.
func (h *HealthChecker) SetBlockNumberUpdateCallback(callback BlockNumberUpdateCallback) {
	h.onBlockNumberUpdate = callback
}

// checkAndSetBlockNumberHealth records a new block sample and runs the
// manager's gates on it; false if the block number could not be fetched.
// A recovering endpoint re-reads its chain ID, in case its URL now serves another chain.
func (h *HealthChecker) checkAndSetBlockNumberHealth(recovering bool) bool {
	ctx, cancel := context.WithTimeout(context.Background(), h.config.Timeout)
	defer cancel()

	sample, err := h.checkBlockNumber(ctx, recovering)
	if err != nil {
		h.taintCheckFailure(fmt.Sprintf("%s: %v", h.blockCheckMethod(), err), err)
		return false
	}

	h.recordSample(sample)

	if h.onBlockNumberUpdate != nil {
		h.onBlockNumberUpdate()
	}
	return true
}

// checkAndSetEthCallHealth runs the eth_call probe every second cycle, and
// always for a recovering endpoint; false if it failed.
func (h *HealthChecker) checkAndSetEthCallHealth(force bool) bool {
	// Skip eth_call check for non-EVM chains
	if h.config.ChainType != "evm" {
		return true
	}

	// Run eth_call check only on every second health cycle, starting from the second one.
	// Sequence per provider: 1st call -> skip, 2nd -> run, 3rd -> skip, 4th -> run, ...
	if h.ethCallCheckCounter.Add(1)%2 != 0 && !force {
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), h.config.Timeout)
	defer cancel()

	if err := h.checkEthCall(ctx); err != nil {
		h.taintCheckFailure(fmt.Sprintf("eth_call sha256: %v", err), err)
		return false
	}
	return true
}

// Start starts the check loop, with the schedule counted from now.
func (h *HealthChecker) Start(c context.Context) {
	h.startAt(c, time.Now())
}

// startAt starts the check loop on a schedule counted from epoch. Checkers of
// one path share an epoch so their checks stay in the same cycle.
func (h *HealthChecker) startAt(c context.Context, epoch time.Time) {
	h.config.Logger.Debug("starting health checker with initial delay",
		"initialDelayMs", h.config.InitialDelay.Milliseconds(),
		"connectionType", h.config.ConnectionType,
		"provider", h.config.Name,
		"path", h.config.Path)

	// Start the health checker in a separate goroutine to avoid blocking
	go h.runHealthChecker(c, epoch.Add(h.config.InitialDelay))
}

// checkTime is when check k (from 0) is due. The schedule is fixed, not
// restarted after each check, so endpoints with slower checks don't drift out
// of the cycle their peers are judged in. The second check comes after
// minRateSpan rather than a whole interval, so every endpoint has a block rate
// early and the lag gate can judge early answers from the second cycle on.
func checkTime(first time.Time, interval time.Duration, k int) time.Time {
	if k == 0 {
		return first
	}
	boot := min(minRateSpan, interval)
	return first.Add(boot + time.Duration(k-1)*interval)
}

// runHealthChecker runs the main health checking loop in a separate goroutine
func (h *HealthChecker) runHealthChecker(c context.Context, first time.Time) {
	timer := time.NewTimer(time.Until(first))
	defer timer.Stop()

	for k := 0; ; {
		select {
		case <-c.Done():
			h.config.Logger.Info("health checker shutting down gracefully",
				"provider", h.config.Name,
				"path", h.config.Path)
			return
		case <-timer.C:
			h.CheckAndSetHealth()
			// Next slot still ahead; one missed by a check that overran is skipped
			k++
			for now := time.Now(); h.config.Interval > 0 && !checkTime(first, h.config.Interval, k).After(now); {
				k++
			}
			timer.Reset(time.Until(checkTime(first, h.config.Interval, k)))
		}
	}
}

// Stop cancels a pending taint removal. The check loop itself ends with Start's context.
func (h *HealthChecker) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.taint.gen++ // invalidates a probe in flight
	if h.taint.removalTimer != nil {
		h.taint.removalTimer.Stop()
		h.taint.removalTimer = nil
	}
}

// IsHealthy reports whether the endpoint may serve: not tainted, and on the
// path's chain.
func (h *HealthChecker) IsHealthy() bool {
	return !h.IsTainted() && !h.wrongChain.Load()
}

func (h *HealthChecker) IsTainted() bool {
	return h.isTainted.Load()
}

// Taint marks the provider as tainted with backoff.
//
// An endpoint that is already tainted is left alone, except that a recheck
// (health-check) taint takes over a timer taint, and re-arms a recheck taint
// whose recheck just failed; both double the wait.
func (h *HealthChecker) Taint(cfg TaintConfig) {
	// Fast path: repeated live-request failures while tainted, no lock
	if !cfg.Recheck && h.isTainted.Load() {
		return
	}

	now := time.Now()
	h.mu.Lock()
	var wait time.Duration
	switch {
	case !h.isTainted.Load():
		if now.Sub(h.taint.lastRemoval) <= cfg.ResetWaitDuration {
			wait = h.taint.waitTime * 2
		} else {
			wait = cfg.InitialWaitTime
		}
	case cfg.Recheck && (!h.taint.cfg.Recheck || !now.Before(h.taint.until)):
		wait = h.taint.waitTime * 2 // escalate a timer taint, or recheck failed
	default:
		h.mu.Unlock()
		return
	}
	wait = min(max(wait, cfg.InitialWaitTime), cfg.MaxWaitTime)
	if cfg.Wait > 0 {
		wait = min(max(cfg.Wait, cfg.InitialWaitTime), cfg.MaxWaitTime)
	}

	// Cancel old timer (AfterFunc timers have no channel to drain); gen
	// guards against one that already fired
	if h.taint.removalTimer != nil {
		h.taint.removalTimer.Stop()
		h.taint.removalTimer = nil
	}
	h.taint.gen++
	if cfg.Recheck {
		h.taint.until = now.Add(wait)
	} else {
		gen := h.taint.gen
		h.taint.removalTimer = time.AfterFunc(wait, func() { h.expireTaint(cfg, gen) })
	}
	h.taint.cfg = cfg
	h.taint.lastRemoval = now
	h.taint.waitTime = wait
	h.isTainted.Store(true)
	h.mu.Unlock()

	// Logging
	logArgs := []any{
		"conn", h.config.ConnectionType,
		"name", h.config.Name,
		"path", h.config.Path,
		"reason", cfg.Reason,
		"retry_sec", wait.Seconds(),
	}
	if cfg.Detail != "" {
		logArgs = append(logArgs, "detail", cfg.Detail)
	}
	h.config.Logger.Info("provider tainted", logArgs...)
}

// expireTaint ends a timer taint, or for probing configs keeps it (doubling
// the wait) until a health probe succeeds. gen identifies the taint it was
// armed for; a superseded or stopped taint is left alone.
func (h *HealthChecker) expireTaint(cfg TaintConfig, gen uint64) {
	if cfg.Probe {
		ctx, cancel := context.WithTimeout(context.Background(), h.config.Timeout)
		_, err := h.checkBlockNumber(ctx, false)
		cancel()
		if err != nil {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.taint.gen != gen {
				return
			}
			h.taint.waitTime = min(h.taint.waitTime*2, cfg.MaxWaitTime)
			h.taint.removalTimer = time.AfterFunc(h.taint.waitTime, func() { h.expireTaint(cfg, gen) })
			h.config.Logger.Debug("still rate limited",
				"name", h.config.Name, "path", h.config.Path,
				"error", err, "retry_sec", h.taint.waitTime.Seconds())
			return
		}
	}
	h.mu.Lock()
	if h.taint.gen != gen {
		h.mu.Unlock()
		return
	}
	h.removeTaintLocked()
	h.mu.Unlock()
	h.logRemoved()
}

// TaintRateLimited taints after an HTTP 429; wait is the provider's hint (0 if none).
func (h *HealthChecker) TaintRateLimited(wait time.Duration) {
	cfg := rateLimitTaintConfig
	cfg.Wait = wait
	h.Taint(cfg)
}

// taintCheckFailure taints after a failed health check. A 429 means the
// endpoint is up but over its limit, which health checks count against: it
// gets the short, probed rate-limit taint, not the long health-check backoff.
func (h *HealthChecker) taintCheckFailure(detail string, err error) {
	if rl, ok := errors.AsType[*rateLimitedError](err); ok {
		cfg := rateLimitTaintConfig
		cfg.Wait, cfg.Detail = rl.wait, detail
		h.Taint(cfg)
		return
	}
	h.TaintHealthCheck(detail)
}

// TaintHTTP is a convenience method that uses the HTTP-specific taint configuration
func (h *HealthChecker) TaintHTTP() {
	h.Taint(httpTaintConfig)
}

// TaintHealthCheck is a convenience method that uses the health check taint configuration.
// An optional detail string is included in the INFO log when the provider is tainted.
func (h *HealthChecker) TaintHealthCheck(detail ...string) {
	cfg := healthCheckTaintConfig
	if len(detail) > 0 && detail[0] != "" {
		cfg.Detail = detail[0]
	}
	h.Taint(cfg)
}

// RemoveTaint lifts any taint at once and cancels its pending removal.
func (h *HealthChecker) RemoveTaint() {
	h.mu.Lock()
	h.removeTaintLocked()
	h.mu.Unlock()
	h.logRemoved()
}

func (h *HealthChecker) removeTaintLocked() {
	if h.taint.removalTimer != nil {
		h.taint.removalTimer.Stop()
		h.taint.removalTimer = nil
	}
	h.taint.gen++
	h.taint.lastRemoval = time.Now()
	h.isTainted.Store(false)
}

func (h *HealthChecker) logRemoved() {
	h.mu.Lock()
	nextWait := h.taint.waitTime
	h.mu.Unlock()
	h.config.Logger.Info("taint removed",
		"connectionType", h.config.ConnectionType,
		"path", h.config.Path,
		"name", h.config.Name,
		"nextTaintWait", nextWait.Seconds())
}

func (h *HealthChecker) BlockNumber() uint64 {
	if s := h.sample.Load(); s != nil {
		return s.block
	}
	return 0
}

func (h *HealthChecker) setBlockNumber(block uint64, at time.Time) {
	h.recordSample(blockSample{block: block, at: at})
}
