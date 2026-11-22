package proxy

import (
	"bytes"
	"context"
	jsonv2 "encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockEVMHealthRPC responds to eth_blockNumber and the SHA-256 precompile eth_call probe.
func mockEVMHealthRPC(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Method string `json:"method"`
	}
	_ = jsonv2.Unmarshal(body, &req)

	w.Header().Set("Content-Type", "application/json")
	switch req.Method {
	case "eth_call":
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x5f78c33274e43fa9de5659265c1d917e25c03722dcb0b8d27db8d5feaa813953"}`))
	default:
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1234"}`))
	}
}

// TestBasicHealthchecker checks if it runs with default options.
func TestBasicHealthchecker(t *testing.T) {
	// Create a mock server
	server := httptest.NewServer(http.HandlerFunc(mockEVMHealthRPC))
	defer server.Close()

	healthcheckConfig := HealthCheckerConfig{
		URL:      server.URL,
		Name:     "test",
		Interval: time.Millisecond * 100, // Much shorter interval for testing
		Timeout:  time.Second,
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	healthchecker := NewHealthChecker(healthcheckConfig)

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	healthchecker.Start(ctx)

	// Wait for a health check cycle
	time.Sleep(time.Millisecond * 150)

	// Verify that the health checker is not tainted initially
	assert.False(t, healthchecker.IsTainted())
	assert.True(t, healthchecker.IsHealthy())

	// Taint the health checker
	healthchecker.TaintHealthCheck()
	assert.True(t, healthchecker.IsTainted())
	assert.False(t, healthchecker.IsHealthy())

	// Remove taint
	healthchecker.RemoveTaint()
	assert.False(t, healthchecker.IsTainted())
	assert.True(t, healthchecker.IsHealthy())
}

func TestHealthCheckerTaint(t *testing.T) {
	// Create a mock server
	server := httptest.NewServer(http.HandlerFunc(mockEVMHealthRPC))
	defer server.Close()

	// Create a health checker with short intervals for testing
	config := HealthCheckerConfig{
		URL:      server.URL,
		Name:     "test",
		Path:     "test",
		Interval: time.Millisecond * 100,
		Timeout:  time.Millisecond * 50,
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	checker := NewHealthChecker(config)

	// Start the health checker
	go checker.Start(t.Context())

	t.Logf("Test started at %v", time.Now())

	// Test initial state
	assert.False(t, checker.IsTainted(), "should not be tainted initially")

	// Taint the checker with a short wait time
	waitTime := time.Millisecond * 200
	t.Logf("Applying first taint at %v with wait time %v", time.Now(), waitTime)
	checker.Taint(TaintConfig{
		InitialWaitTime:   waitTime,
		MaxWaitTime:       time.Second,
		ResetWaitDuration: time.Second,
		Reason:            "test taint",
	})

	// Verify it's tainted
	assert.True(t, checker.IsTainted(), "should be tainted after taint call")

	// Wait for taint to be removed with a buffer
	t.Logf("Waiting for first taint removal at %v", time.Now())
	time.Sleep(waitTime + time.Millisecond*100)
	t.Logf("Checking taint state at %v", time.Now())

	// Verify taint is removed
	assert.False(t, checker.IsTainted(), "taint should be removed after wait time")

	// Test exponential backoff
	t.Logf("Applying second taint at %v", time.Now())
	checker.Taint(TaintConfig{
		InitialWaitTime:   waitTime,
		MaxWaitTime:       time.Second,
		ResetWaitDuration: time.Millisecond * 50, // Short reset duration to ensure we're within it
		Reason:            "test taint",
	})

	// Verify it's tainted again
	assert.True(t, checker.IsTainted(), "should be tainted after second taint call")

	// Wait for taint to be removed
	t.Logf("Waiting for second taint removal at %v", time.Now())
	time.Sleep(waitTime + time.Millisecond*100)
	t.Logf("Checking taint state at %v", time.Now())

	// Verify taint is removed
	assert.False(t, checker.IsTainted(), "taint should be removed after wait time")

	// Test max wait time
	t.Logf("Applying third taint at %v with max wait time 300ms", time.Now())
	checker.Taint(TaintConfig{
		InitialWaitTime:   waitTime,
		MaxWaitTime:       time.Millisecond * 300,
		ResetWaitDuration: time.Millisecond * 50,
		Reason:            "test taint",
	})

	// Verify it's tainted again
	assert.True(t, checker.IsTainted(), "should be tainted after third taint call")

	// Wait for taint to be removed
	t.Logf("Waiting for third taint removal at %v", time.Now())
	time.Sleep(waitTime + time.Millisecond*100)
	t.Logf("Checking final taint state at %v", time.Now())

	// Verify taint is removed
	assert.False(t, checker.IsTainted(), "taint should be removed after wait time")
}

func TestHealthCheckerTaintRemoval(t *testing.T) {
	// Create a health checker with short intervals for testing
	config := HealthCheckerConfig{
		URL:      "http://localhost:8545", // Doesn't matter for this test
		Name:     "test",
		Path:     "test",
		Interval: time.Millisecond * 100,
		Timeout:  time.Millisecond * 50,
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	checker := NewHealthChecker(config)

	// Start the health checker
	go checker.Start(t.Context())

	// Taint the checker
	waitTime := time.Millisecond * 200
	checker.Taint(TaintConfig{
		InitialWaitTime:   waitTime,
		MaxWaitTime:       time.Second,
		ResetWaitDuration: time.Second,
		Reason:            "test taint",
	})

	// Verify it's tainted
	assert.True(t, checker.IsTainted(), "should be tainted after taint call")

	// Stop the checker
	checker.Stop()

	// Verify taint removal timer is cleaned up
	time.Sleep(time.Millisecond * 100)
	assert.True(t, checker.IsTainted(), "taint should remain after stop")
}

func TestHealthCheckerTaintHTTP(t *testing.T) {
	healthchecker := NewHealthChecker(HealthCheckerConfig{
		URL:    "http://localhost:8545",
		Name:   "test",
		Logger: slog.Default(),
	})

	// Test initial state
	assert.False(t, healthchecker.IsTainted())
	assert.True(t, healthchecker.IsHealthy())

	// Test HTTP taint
	healthchecker.TaintHTTP()
	assert.True(t, healthchecker.IsTainted())
	assert.False(t, healthchecker.IsHealthy())

	// Test taint removal
	healthchecker.RemoveTaint()
	assert.False(t, healthchecker.IsTainted())
	assert.True(t, healthchecker.IsHealthy())

	// Test multiple HTTP taints with exponential backoff
	healthchecker.TaintHTTP()
	firstWaitTime := healthchecker.taint.waitTime
	healthchecker.RemoveTaint()

	// Taint again within reset period
	healthchecker.TaintHTTP()
	secondWaitTime := healthchecker.taint.waitTime
	assert.Greater(t, secondWaitTime, firstWaitTime)

	// Test max wait time
	for range 5 {
		healthchecker.TaintHTTP()
		healthchecker.RemoveTaint()
	}
	healthchecker.TaintHTTP()
	finalWaitTime := healthchecker.taint.waitTime
	assert.LessOrEqual(t, finalWaitTime, httpTaintConfig.MaxWaitTime)
}

// TestHealthCheckerConcurrentTaint verifies that tainting operations are safe under concurrency.
// This test is most valuable when run with the race detector:
//
//	go test -race ./internal/proxy -run TestHealthCheckerConcurrentTaint
func TestHealthCheckerConcurrentTaint(t *testing.T) {
	healthchecker := NewHealthChecker(HealthCheckerConfig{
		URL:      "http://localhost:8545",
		Name:     "test",
		Path:     "test",
		Interval: time.Millisecond * 10,
		Timeout:  time.Millisecond * 50,
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})

	ctx := t.Context()
	go healthchecker.Start(ctx)

	var wg sync.WaitGroup
	const workers = 8
	const iterations = 100

	for range workers {
		wg.Go(func() {
			for range iterations {
				// Interleave tainting and checks to exercise concurrent access paths.
				healthchecker.TaintHTTP()
				_ = healthchecker.IsTainted()
				_ = healthchecker.IsHealthy()
				healthchecker.RemoveTaint()
			}
		})
	}

	wg.Wait()
}

func TestHealthCheckTaintDetailNamesTheRealRequest(t *testing.T) {
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer forbidden.Close()
	wsURL := "ws" + strings.TrimPrefix(forbidden.URL, "http")

	for _, tc := range []struct {
		chain, conn, url, want string
	}{
		{"solana", "websocket", wsURL, "slotSubscribe: websocket: bad handshake (HTTP 403)"},
		{"solana", "http", forbidden.URL, "getSlot: "},
		{"tron", "http", forbidden.URL, "wallet/getblock: "},
		{"evm", "http", forbidden.URL, "eth_blockNumber: "},
		{"evm", "websocket", wsURL, "eth_blockNumber: websocket: bad handshake (HTTP 403)"},
	} {
		var logs bytes.Buffer
		hc := NewHealthChecker(HealthCheckerConfig{
			Logger: slog.New(slog.NewTextHandler(&logs, nil)), URL: tc.url, Name: "p",
			Timeout: time.Second, Path: t.Name() + tc.chain + tc.conn, ChainType: tc.chain, ConnectionType: tc.conn,
		})
		hc.CheckAndSetHealth()
		assert.True(t, hc.IsTainted(), "%s/%s", tc.chain, tc.conn)
		assert.Contains(t, logs.String(), tc.want, "%s/%s", tc.chain, tc.conn)
		if tc.chain != "evm" {
			assert.NotContains(t, logs.String(), "eth_", "%s/%s must not mention EVM methods", tc.chain, tc.conn)
		}
	}
}

// A WebSocket EVM endpoint answering a JSON-RPC error or a null result must
// fail its check, not record block 0.
func TestWebSocketEVMCheckRejectsErrorsAndEmptyResults(t *testing.T) {
	for name, reply := range map[string]string{
		"rpc error":   `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"header not found"}}`,
		"null result": `{"jsonrpc":"2.0","id":1,"result":null}`,
		"empty":       `{"jsonrpc":"2.0","id":1,"result":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			upgrader := websocket.Upgrader{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
					_ = conn.WriteMessage(websocket.TextMessage, []byte(reply))
				}
			}))
			t.Cleanup(srv.Close)
			hc := NewHealthChecker(HealthCheckerConfig{
				Logger: quietLogger, URL: "ws" + strings.TrimPrefix(srv.URL, "http"), Name: "p",
				Timeout: time.Second, Path: t.Name(), ConnectionType: "websocket",
			})
			t.Cleanup(hc.Stop)
			hc.CheckAndSetHealth()
			assert.True(t, hc.IsTainted())
			assert.Nil(t, hc.sample.Load(), "no block recorded")
		})
	}
}

func TestParseHexRejectsEmpty(t *testing.T) {
	for _, s := range []string{"", "0x", "0X"} {
		_, err := parseHex(s)
		assert.Error(t, err, "%q", s)
	}
	v, err := parseHex("0x10")
	require.NoError(t, err)
	assert.Equal(t, uint64(16), v)
}

// countingUpstream serves healthy EVM health checks, counting them; failing
// makes every request answer 500.
func countingUpstream(t *testing.T) (url string, requests *atomic.Int32, failing *atomic.Bool) {
	requests, failing = new(atomic.Int32), new(atomic.Bool)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		mockEVMHealthRPC(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, requests, failing
}

func newCountingChecker(t *testing.T, url string) *HealthChecker {
	hc := NewHealthChecker(HealthCheckerConfig{Logger: quietLogger, URL: url, Name: "p", Path: t.Name(), Timeout: time.Second})
	t.Cleanup(hc.Stop)
	return hc
}

// A health-check taint has no timer: it holds until a scheduled check after
// its backoff passes, and checks during the backoff send nothing.
func TestHealthCheckTaintLiftsOnlyByPassingRecheck(t *testing.T) {
	url, requests, failing := countingUpstream(t)
	hc := newCountingChecker(t, url)

	failing.Store(true)
	hc.CheckAndSetHealth()
	require.True(t, hc.IsTainted())
	hc.mu.Lock()
	assert.Nil(t, hc.taint.removalTimer, "no timer can lift it unverified")
	firstWait := hc.taint.waitTime
	hc.mu.Unlock()

	n := requests.Load()
	failing.Store(false)
	hc.CheckAndSetHealth()
	assert.Equal(t, n, requests.Load(), "no request while backing off")
	assert.True(t, hc.IsTainted())

	failing.Store(true)
	makeDue(hc)
	hc.CheckAndSetHealth()
	assert.True(t, hc.IsTainted(), "failed recheck keeps it out")
	hc.mu.Lock()
	assert.Equal(t, 2*firstWait, hc.taint.waitTime, "and doubles the wait")
	hc.mu.Unlock()

	failing.Store(false)
	makeDue(hc)
	hc.CheckAndSetHealth()
	assert.False(t, hc.IsTainted(), "passing recheck lifts it")
}

// A live-request (HTTP) taint keeps its fast timer, but a failing scheduled
// check turns it into a health-check taint, and the old timer can't lift it.
func TestFailingCheckEscalatesHTTPTaint(t *testing.T) {
	url, _, failing := countingUpstream(t)
	hc := newCountingChecker(t, url)

	hc.TaintHTTP()
	hc.mu.Lock()
	httpGen := hc.taint.gen
	hc.mu.Unlock()

	failing.Store(true)
	hc.CheckAndSetHealth()
	hc.mu.Lock()
	assert.True(t, hc.taint.cfg.Recheck, "escalated")
	assert.GreaterOrEqual(t, hc.taint.waitTime, healthCheckTaintConfig.InitialWaitTime)
	hc.mu.Unlock()

	hc.expireTaint(httpTaintConfig, httpGen) // the HTTP timer firing late
	assert.True(t, hc.IsTainted(), "a superseded timer must not lift the new taint")
}

// A passing check leaves an HTTP taint to its timer, and rate-limited
// endpoints are not checked at all: their probe handles recovery.
func TestScheduledCheckRespectsTimerTaints(t *testing.T) {
	url, requests, _ := countingUpstream(t)

	hc := newCountingChecker(t, url)
	hc.TaintHTTP()
	hc.CheckAndSetHealth()
	assert.True(t, hc.IsTainted(), "HTTP taint lasts until its timer")
	assert.NotZero(t, requests.Load(), "but the endpoint is still checked")

	limited := newCountingChecker(t, url)
	limited.TaintRateLimited(time.Minute)
	n := requests.Load()
	limited.CheckAndSetHealth()
	assert.Equal(t, n, requests.Load(), "no check while rate limited")
}

// The schedule is fixed from the first check, with the second one early so
// block rates are known from the second cycle.
func TestCheckTimeSchedule(t *testing.T) {
	first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		interval time.Duration
		want     []time.Duration // offsets from first, k = 0..3
	}{
		{time.Minute, []time.Duration{0, minRateSpan, minRateSpan + time.Minute, minRateSpan + 2*time.Minute}},
		{5 * time.Second, []time.Duration{0, 5 * time.Second, 10 * time.Second, 15 * time.Second}},
	} {
		for k, want := range tc.want {
			assert.Equal(t, first.Add(want), checkTime(first, tc.interval, k), "interval %v, k=%d", tc.interval, k)
		}
	}
}

// Checks of one path stay in step however long each endpoint takes to
// answer: the old loop restarted its timer after each check, so a slower
// endpoint fell a little further behind every cycle.
func TestChecksStayInStepDespiteSlowEndpoint(t *testing.T) {
	arrivals := func(delay time.Duration) (string, func() []time.Time) {
		var mu sync.Mutex
		var at []time.Time
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "eth_blockNumber") {
				mu.Lock()
				at = append(at, time.Now())
				mu.Unlock()
			}
			time.Sleep(delay)
			r.Body = io.NopCloser(bytes.NewReader(body))
			mockEVMHealthRPC(w, r)
		}))
		t.Cleanup(srv.Close)
		return srv.URL, func() []time.Time { mu.Lock(); defer mu.Unlock(); return slices.Clone(at) }
	}
	slowURL, slowAt := arrivals(60 * time.Millisecond)
	fastURL, fastAt := arrivals(0)

	config := createConfig()
	config.Path = t.Name()
	config.Logger = quietLogger
	config.HealthChecks.Interval = 200 * time.Millisecond
	config.Targets = []NodeProviderConfig{httpTarget("slow", slowURL), httpTarget("fast", fastURL)}
	hcm, err := NewHealthCheckManager(config)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	hcm.Start(ctx)
	time.Sleep(1100 * time.Millisecond) // ~6 cycles: drift would reach 6 × 60-120 ms
	cancel()
	hcm.Stop()

	slow, fast := slowAt(), fastAt()
	n := min(len(slow), len(fast))
	require.GreaterOrEqual(t, n, 4)
	for k := range n {
		assert.InDelta(t, 0, slow[k].Sub(fast[k]).Seconds(), 0.04, "cycle %d", k)
	}
}

// A health check answered with 429 gets the short, probed rate-limit taint
// (honouring Retry-After), not the long health-check backoff.
func TestHealthCheck429GetsRateLimitTaint(t *testing.T) {
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(limited.Close)
	for _, conn := range []string{"http", "websocket"} {
		t.Run(conn, func(t *testing.T) {
			url := limited.URL
			if conn == "websocket" {
				url = "ws" + strings.TrimPrefix(url, "http")
			}
			hc := NewHealthChecker(HealthCheckerConfig{Logger: quietLogger, URL: url, Name: "p", Path: t.Name(), Timeout: time.Second, ConnectionType: conn})
			t.Cleanup(hc.Stop)
			hc.CheckAndSetHealth()
			require.True(t, hc.IsTainted())
			hc.mu.Lock()
			defer hc.mu.Unlock()
			assert.True(t, hc.taint.cfg.Probe, "rate-limit taint")
			assert.Equal(t, 7*time.Second, hc.taint.waitTime, "Retry-After honoured")
		})
	}
}

// The Tron check asks for the latest block header only, and a response
// without a block number fails instead of recording block 0. The network
// fingerprint is the hash of block 0, fetched on the first check only.
func TestTronCheckFetchesHeaderOnly(t *testing.T) {
	const genesis = "00000000000000001ebf88508a03865c71d452e25f4d51194196a1d22b6653dc"
	for name, tc := range map[string]struct {
		reply string
		ok    bool
	}{
		"header": {`{"blockID":"00","block_header":{"raw_data":{"number":88236685}}}`, true},
		"empty":  {`{}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			requests := map[string][]string{} // path -> bodies
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				requests[r.URL.Path] = append(requests[r.URL.Path], string(b))
				mu.Unlock()
				if r.URL.Path == "/wallet/getblockbynum" {
					_, _ = w.Write([]byte(`{"blockID":"` + genesis + `"}`))
					return
				}
				_, _ = w.Write([]byte(tc.reply))
			}))
			t.Cleanup(srv.Close)
			hc := NewHealthChecker(HealthCheckerConfig{Logger: quietLogger, URL: srv.URL, Name: "p", Path: t.Name(), Timeout: time.Second, ChainType: "tron"})
			t.Cleanup(hc.Stop)
			hc.CheckAndSetHealth()

			mu.Lock()
			defer mu.Unlock()
			require.Len(t, requests["/wallet/getblock"], 1)
			assert.JSONEq(t, `{"detail":false}`, requests["/wallet/getblock"][0])
			assert.Equal(t, !tc.ok, hc.IsTainted())
			if !tc.ok {
				return
			}
			assert.Equal(t, uint64(88236685), hc.BlockNumber())
			require.Len(t, requests["/wallet/getblockbynum"], 1)
			assert.JSONEq(t, `{"num":0}`, requests["/wallet/getblockbynum"][0])
			assert.Equal(t, genesis, hc.Network())

			mu.Unlock()
			hc.CheckAndSetHealth()
			mu.Lock()
			assert.Len(t, requests["/wallet/getblockbynum"], 1, "fingerprint fetched once")
		})
	}
}

// Over WebSocket, the Solana genesis hash is read on the check's connection
// before the slot subscription, and only on the first check.
func TestSolanaWebSocketCheckReadsGenesisOnce(t *testing.T) {
	var genesisCalls atomic.Int32
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var req struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			if err := conn.ReadJSON(&req); err != nil {
				return
			}
			switch req.Method {
			case "getGenesisHash":
				genesisCalls.Add(1)
				_ = conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"})
			case "slotSubscribe":
				_ = conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": 7})
				_ = conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "method": "slotNotification",
					"params": map[string]any{"subscription": 7, "result": map[string]any{"slot": 380000000}}})
			default:
				_ = conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": true})
			}
		}
	}))
	t.Cleanup(srv.Close)
	hc := NewHealthChecker(HealthCheckerConfig{Logger: quietLogger, URL: "ws" + strings.TrimPrefix(srv.URL, "http"), Name: "p",
		Path: t.Name(), Timeout: time.Second, ChainType: "solana", ConnectionType: "websocket"})
	t.Cleanup(hc.Stop)

	hc.CheckAndSetHealth()
	hc.CheckAndSetHealth()
	assert.False(t, hc.IsTainted())
	assert.Equal(t, uint64(380000000), hc.BlockNumber())
	assert.Equal(t, "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d", hc.Network())
	assert.Equal(t, int32(1), genesisCalls.Load())
}
