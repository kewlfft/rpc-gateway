package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newLagManager builds a manager of HTTP-only providers that are never started:
// tests set block samples and run the lag gate themselves, with explicit times.
func newLagManager(t *testing.T, threshold uint64, names ...string) *HealthCheckManager {
	t.Helper()
	config := createConfig()
	config.Path = t.Name()
	config.Logger = quietLogger
	config.Timeout = time.Second
	config.HealthChecks.BlockDiffThreshold = threshold
	config.Targets = nil
	for _, n := range names {
		config.Targets = append(config.Targets, httpTarget(n, "http://"+n))
	}
	hcm, err := NewHealthCheckManager(config)
	require.NoError(t, err)
	t.Cleanup(hcm.Stop) // cancel taint-removal timers
	return hcm
}

// report records a sample for a provider at t and runs the gate, as a
// completed health check does.
func report(hcm *HealthCheckManager, name string, block uint64, t time.Time) {
	hcm.GetHealthChecker(name, "http").setBlockNumber(block, t)
	hcm.checkBlockLag(t)
}

func tainted(hcm *HealthCheckManager, name string) bool {
	return hcm.GetHealthChecker(name, "http").IsTainted()
}

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// primeRate runs one earlier cycle so the gate knows the chain's block rate:
// every provider at `block` one minute before t0.
func primeRate(hcm *HealthCheckManager, block uint64, names ...string) {
	for _, n := range names {
		report(hcm, n, block, t0.Add(-time.Minute))
	}
}

func TestBlockLagThreshold(t *testing.T) {
	for _, tc := range []struct {
		lag   uint64
		taint bool
	}{{0, false}, {2, false}, {3, true}, {50, true}} {
		t.Run(fmt.Sprint("lag=", tc.lag), func(t *testing.T) {
			hcm := newLagManager(t, 2, "lead", "lag")
			report(hcm, "lead", 1000, t0)
			report(hcm, "lag", 1000-tc.lag, t0.Add(ms(10))) // answers last: no allowance
			assert.Equal(t, tc.taint, tainted(hcm, "lag"))
			assert.False(t, tainted(hcm, "lead"))
		})
	}
}

// A lagging provider that answers before the leader used to be compared only
// with the previous cycle's (lower) blocks and always passed.
func TestBlockLagCatchesLaggerThatAnswersFirst(t *testing.T) {
	hcm := newLagManager(t, 2, "lead", "lag")
	primeRate(hcm, 995, "lead", "lag") // Ethereum pace: 5 blocks/min

	report(hcm, "lag", 995, t0.Add(ms(50))) // fast but 5 behind
	assert.False(t, tainted(hcm, "lag"), "nothing fresher to compare with yet")

	report(hcm, "lead", 1000, t0.Add(ms(400)))
	assert.True(t, tainted(hcm, "lag"), "leader's block must re-judge the earlier answer")
	assert.False(t, tainted(hcm, "lead"))
}

// On fast chains the blocks produced between two answers of one cycle are
// allowed for, so answering first is not mistaken for lag.
func TestBlockLagAllowsForAnswerTimeOnFastChains(t *testing.T) {
	hcm := newLagManager(t, 2, "fast", "slow", "behind")
	primeRate(hcm, 1000-240, "fast", "slow", "behind") // 4 blocks/s (Arbitrum)

	report(hcm, "fast", 1000, t0.Add(ms(100)))
	report(hcm, "behind", 996, t0.Add(ms(100)))
	report(hcm, "slow", 1003, t0.Add(ms(800))) // 0.7 s later → allowance ceil(2.8) = 3

	assert.False(t, tainted(hcm, "fast"), "3 behind, of which 3 explained by answer time")
	assert.True(t, tainted(hcm, "behind"), "7 behind > threshold 2 + allowance 3")
	assert.False(t, tainted(hcm, "slow"))
}

// Before a block rate is known, an earlier answer can't be told from lag, so
// only answers at least as recent as the top block are judged.
func TestBlockLagFirstCycleJudgesOnlyLaterAnswers(t *testing.T) {
	hcm := newLagManager(t, 2, "early", "lead", "late")
	report(hcm, "early", 990, t0)
	report(hcm, "lead", 1000, t0.Add(ms(100)))
	report(hcm, "late", 990, t0.Add(ms(200)))
	assert.False(t, tainted(hcm, "early"))
	assert.True(t, tainted(hcm, "late"))
}

// Samples from earlier cycles (or of endpoints that skip checks while
// tainted) are not judged against the current top block, nor raise it.
func TestBlockLagIgnoresStaleSamples(t *testing.T) {
	hcm := newLagManager(t, 2, "a", "b", "high", "low")
	primeRate(hcm, 900, "a", "b")
	report(hcm, "high", 2000, t0.Add(-30*time.Second)) // bogus high, earlier cycle
	report(hcm, "low", 950, t0.Add(-20*time.Second))   // behind, earlier cycle

	report(hcm, "a", 1000, t0)
	report(hcm, "b", 1000, t0.Add(ms(100)))
	assert.False(t, tainted(hcm, "a"), "a stale high block must not raise the top")
	assert.False(t, tainted(hcm, "b"))
	assert.False(t, tainted(hcm, "low"), "a stale low block must not be judged")
	assert.False(t, tainted(hcm, "high"))
}

func TestBlockLagTaintsTheLaggingEndpointOnly(t *testing.T) {
	config := createConfig()
	config.Path = t.Name()
	config.Logger = quietLogger
	config.Targets = []NodeProviderConfig{
		{Name: "a", Connection: ConnectionConfig{HTTP: HTTPConnection{URL: "http://a"}, WebSocket: WebSocketConnection{URL: "ws://a"}}},
		{Name: "b", Connection: ConnectionConfig{HTTP: HTTPConnection{URL: "http://b"}}},
	}
	hcm, err := NewHealthCheckManager(config)
	require.NoError(t, err)
	t.Cleanup(hcm.Stop)
	aHTTP, aWS, bHTTP := hcm.GetHealthChecker("a", "http"), hcm.GetHealthChecker("a", "websocket"), hcm.GetHealthChecker("b", "http")

	bHTTP.setBlockNumber(100, t0)
	aHTTP.setBlockNumber(100, t0)
	aWS.setBlockNumber(90, t0.Add(ms(500)))
	hcm.checkBlockLag(t0.Add(ms(500)))
	assert.True(t, aWS.IsTainted(), "lagging WebSocket endpoint must be tainted")
	assert.False(t, aHTTP.IsTainted(), "in-sync HTTP endpoint of the same provider must not be")
	assert.False(t, bHTTP.IsTainted())
}

// evmUpstream answers health checks for an EVM node at a fixed block and
// chain ID, counting the requests it gets.
type evmUpstream struct {
	block, chainID atomic.Uint64
	requests, live atomic.Int32
	*httptest.Server
}

func newEVMUpstream(t *testing.T, block, chainID uint64) *evmUpstream {
	u := &evmUpstream{}
	u.block.Store(block)
	u.chainID.Store(chainID)
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.requests.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.UserAgent() != userAgent {
			u.live.Add(1)
		}
		switch {
		case strings.Contains(string(body), "eth_blockNumber"):
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":"0x%x"}`, u.block.Load())
		case strings.Contains(string(body), "eth_chainId"):
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":"0x%x"}`, u.chainID.Load())
		default:
			r.Body = io.NopCloser(bytes.NewReader(body))
			mockEVMHealthRPC(w, r)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

// newUpstreamManager builds a manager over HTTP upstreams, never started:
// tests run each endpoint's scheduled check themselves, in a chosen order.
func newUpstreamManager(t *testing.T, ups map[string]*evmUpstream) *HealthCheckManager {
	t.Helper()
	config := createConfig()
	config.Path = t.Name()
	config.Logger = quietLogger
	config.HealthChecks.BlockDiffThreshold = 2
	config.Targets = nil
	for name, u := range ups {
		config.Targets = append(config.Targets, httpTarget(name, u.URL))
	}
	hcm, err := NewHealthCheckManager(config)
	require.NoError(t, err)
	t.Cleanup(hcm.Stop)
	return hcm
}

// Wiring: a completed health check records its block and runs the gate.
func TestHealthCheckRunsBlockLagGate(t *testing.T) {
	hcm := newUpstreamManager(t, map[string]*evmUpstream{
		"lead": newEVMUpstream(t, 1000, 1),
		"lag":  newEVMUpstream(t, 990, 1),
	})
	lead, lag := hcm.GetHealthChecker("lead", "http"), hcm.GetHealthChecker("lag", "http")

	lead.CheckAndSetHealth()
	lag.CheckAndSetHealth()
	assert.Equal(t, uint64(1000), lead.BlockNumber())
	assert.Equal(t, uint64(990), lag.BlockNumber())
	assert.True(t, lag.IsTainted())
	assert.False(t, lead.IsTainted())
}

// Endpoints that disagree on the chain ID (say a BSC URL in the eth path)
// stop the path: no guessing which is wrong, and no lag judgement across
// chains, until the wrong URL is removed from the config.
func TestChainIDConflictStopsThePath(t *testing.T) {
	ups := map[string]*evmUpstream{
		"a":     newEVMUpstream(t, 1000, 1),
		"wrong": newEVMUpstream(t, 50_000_000, 56),
	}
	config := createConfig()
	config.Path = t.Name()
	config.Logger = quietLogger
	config.Targets = []NodeProviderConfig{httpTarget("a", ups["a"].URL), httpTarget("wrong", ups["wrong"].URL)}
	p, err := NewProxy(config)
	require.NoError(t, err)
	t.Cleanup(p.hcm.Stop)
	a, wrong := p.hcm.GetHealthChecker("a", "http"), p.hcm.GetHealthChecker("wrong", "http")

	a.CheckAndSetHealth()
	assert.False(t, p.hcm.ChainConflict(), "one chain ID known: no conflict")
	wrong.CheckAndSetHealth()
	assert.True(t, p.hcm.ChainConflict())
	assert.False(t, a.IsTainted(), "the higher wrong-chain block must not taint a")

	rr := postRPC(p, "/", nil)
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.Contains(t, rr.Body.String(), "different networks")
	assert.Zero(t, ups["a"].live.Load()+ups["wrong"].live.Load(), "no request forwarded")

	// Endpoints agreeing again (here: the wrong one re-reads its chain ID on
	// recovery) serve the path again
	ups["wrong"].chainID.Store(1)
	wrong.TaintHealthCheck("test")
	makeDue(wrong)
	wrong.CheckAndSetHealth()
	assert.False(t, p.hcm.ChainConflict())
	assert.Equal(t, http.StatusOK, postRPC(p, "/", nil).Code)
}

// With a majority, an endpoint on another chain is excluded (not tainted):
// it serves nothing, and its higher block doesn't taint the others.
func TestChainIDOutlierExcludedByMajority(t *testing.T) {
	ups := map[string]*evmUpstream{
		"a":     newEVMUpstream(t, 1000, 1),
		"b":     newEVMUpstream(t, 1000, 1),
		"wrong": newEVMUpstream(t, 50_000_000, 56),
	}
	config := createConfig()
	config.Path = t.Name()
	config.Logger = quietLogger
	config.Targets = []NodeProviderConfig{
		httpTarget("wrong", ups["wrong"].URL), httpTarget("a", ups["a"].URL), httpTarget("b", ups["b"].URL),
	}
	p, err := NewProxy(config)
	require.NoError(t, err)
	t.Cleanup(p.hcm.Stop)

	for _, n := range []string{"wrong", "a", "b"} {
		p.hcm.GetHealthChecker(n, "http").CheckAndSetHealth()
	}
	assert.False(t, p.hcm.ChainConflict())
	wrong := p.hcm.GetHealthChecker("wrong", "http")
	assert.False(t, wrong.IsTainted(), "excluded, not tainted")
	assert.False(t, wrong.IsHealthy())
	assert.True(t, p.hcm.GetHealthChecker("a", "http").IsHealthy())
	assert.True(t, p.hcm.GetHealthChecker("b", "http").IsHealthy())

	// wrong is first in priority order, yet gets no traffic
	require.Equal(t, http.StatusOK, postRPC(p, "/", nil).Code)
	assert.Zero(t, ups["wrong"].live.Load())
	assert.Equal(t, int32(1), ups["a"].live.Load())
}

// The vote is recomputed on every update: two wrong endpoints answering first
// at startup briefly outvote a, which recovers once the rest answer.
func TestChainIDMajorityCorrectsEarlyStartupVote(t *testing.T) {
	ups := map[string]*evmUpstream{
		"w1": newEVMUpstream(t, 50_000_000, 56), "w2": newEVMUpstream(t, 50_000_000, 56),
		"a": newEVMUpstream(t, 1000, 1), "b": newEVMUpstream(t, 1000, 1), "c": newEVMUpstream(t, 1000, 1),
	}
	hcm := newUpstreamManager(t, ups)
	check := func(n string) { hcm.GetHealthChecker(n, "http").CheckAndSetHealth() }
	healthy := func(n string) bool { return hcm.GetHealthChecker(n, "http").IsHealthy() }

	check("w1")
	check("w2")
	check("a")
	assert.False(t, healthy("a"), "outvoted 2 to 1 for now")
	check("b")
	assert.True(t, hcm.ChainConflict(), "2 to 2: no majority")
	check("c")
	assert.False(t, hcm.ChainConflict())
	for _, n := range []string{"a", "b", "c"} {
		assert.True(t, healthy(n), n)
		assert.False(t, hcm.GetHealthChecker(n, "http").IsTainted(), "%s: the wrong chain's blocks never judged it", n)
	}
	assert.False(t, healthy("w1"))
	assert.False(t, healthy("w2"))
}

// Votes count per provider: one wrong provider with both HTTP and WebSocket
// must not tie two correct HTTP-only providers (which would stop the path).
func TestNetworkVotesCountPerProvider(t *testing.T) {
	config := createConfig()
	config.Path = t.Name()
	config.Logger = quietLogger
	config.Targets = []NodeProviderConfig{
		{Name: "wrong", Connection: ConnectionConfig{HTTP: HTTPConnection{URL: "http://w"}, WebSocket: WebSocketConnection{URL: "ws://w"}}},
		httpTarget("a", "http://a"), httpTarget("b", "http://b"),
	}
	hcm, err := NewHealthCheckManager(config)
	require.NoError(t, err)
	t.Cleanup(hcm.Stop)
	hcm.GetHealthChecker("wrong", "http").setNetwork("56")
	hcm.GetHealthChecker("wrong", "websocket").setNetwork("56")
	hcm.GetHealthChecker("a", "http").setNetwork("1")
	hcm.GetHealthChecker("b", "http").setNetwork("1")

	hcm.lagMu.Lock()
	conflict := hcm.checkNetworks()
	hcm.lagMu.Unlock()
	assert.False(t, conflict, "2 providers to 1, not 2 endpoints to 2")
	assert.False(t, hcm.GetHealthChecker("wrong", "http").IsHealthy())
	assert.False(t, hcm.GetHealthChecker("wrong", "websocket").IsHealthy())
	assert.True(t, hcm.GetHealthChecker("a", "http").IsHealthy())
}

// Solana endpoints are fingerprinted by genesis hash, so a devnet URL in a
// mainnet path is excluded, and its slot never judges the others.
func TestSolanaDevnetEndpointIsExcluded(t *testing.T) {
	const mainnet, devnet = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d", "EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG"
	solana := func(slot uint64, genesis string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "getGenesisHash") {
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":%q}`, genesis)
				return
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":%d}`, slot)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	config := createConfig()
	config.Path = t.Name()
	config.Logger = quietLogger
	config.ChainType = "solana"
	config.HealthChecks.BlockDiffThreshold = 10
	config.Targets = []NodeProviderConfig{
		httpTarget("devnet", solana(500_000_000, devnet)), // higher slot than mainnet
		httpTarget("a", solana(380_000_000, mainnet)),
		httpTarget("b", solana(380_000_000, mainnet)),
	}
	hcm, err := NewHealthCheckManager(config)
	require.NoError(t, err)
	t.Cleanup(hcm.Stop)
	for _, n := range []string{"devnet", "a", "b"} {
		hcm.GetHealthChecker(n, "http").CheckAndSetHealth()
	}
	assert.Equal(t, mainnet, hcm.GetHealthChecker("a", "http").Network())
	assert.False(t, hcm.GetHealthChecker("devnet", "http").IsHealthy())
	assert.True(t, hcm.GetHealthChecker("a", "http").IsHealthy(), "the devnet slot must not taint a")
	assert.True(t, hcm.GetHealthChecker("b", "http").IsHealthy())
}

// The chain ID is a constant: it is requested on the first check only, and
// again when a tainted endpoint is rechecked.
func TestChainIDFetchedOnceAndOnRecovery(t *testing.T) {
	var chainCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "eth_chainId") {
			chainCalls.Add(1)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		mockEVMHealthRPC(w, r)
	}))
	t.Cleanup(srv.Close)
	hc := NewHealthChecker(HealthCheckerConfig{Logger: quietLogger, URL: srv.URL, Name: "p", Path: t.Name(), Timeout: time.Second})
	t.Cleanup(hc.Stop)

	for range 4 {
		hc.CheckAndSetHealth()
	}
	assert.Equal(t, int32(1), chainCalls.Load())
	assert.Equal(t, "1", hc.Network())

	hc.TaintHealthCheck("test")
	makeDue(hc)
	hc.CheckAndSetHealth()
	assert.False(t, hc.IsTainted())
	assert.Equal(t, int32(2), chainCalls.Load(), "re-read on recovery")
}

// Without a configured chain ID, a tie can't tell which side is wrong:
// nobody is tainted for the chain (the block gate still applies).
func TestChainIDTieJudgesNobody(t *testing.T) {
	hcm := newUpstreamManager(t, map[string]*evmUpstream{
		"a": newEVMUpstream(t, 1000, 1),
		"b": newEVMUpstream(t, 1000, 10),
	})
	hcm.GetHealthChecker("a", "http").CheckAndSetHealth()
	hcm.GetHealthChecker("b", "http").CheckAndSetHealth()
	assert.False(t, hcm.GetHealthChecker("a", "http").IsTainted())
	assert.False(t, hcm.GetHealthChecker("b", "http").IsTainted())
}

// ageSamples moves every recorded sample d into the past, as if a check
// interval had passed.
func ageSamples(hcm *HealthCheckManager, d time.Duration) {
	for _, c := range hcm.checkers {
		if s := c.sample.Load(); s != nil {
			aged := *s
			aged.at = s.at.Add(-d)
			c.sample.Store(&aged)
		}
	}
}

// makeDue ends a recheck taint's backoff, as if its wait had elapsed.
func makeDue(h *HealthChecker) {
	h.mu.Lock()
	h.taint.until = time.Now().Add(-time.Second)
	h.mu.Unlock()
}

// A lag-tainted endpoint recovers only through a scheduled check of the same
// cycle as its peers, whichever answers first.
func TestLaggingEndpointStaysOutOnRecheck(t *testing.T) {
	for _, order := range [][]string{{"lead", "lag"}, {"lag", "lead"}} {
		t.Run(strings.Join(order, "-then-"), func(t *testing.T) {
			lagUp := newEVMUpstream(t, 990, 1)
			hcm := newUpstreamManager(t, map[string]*evmUpstream{"lead": newEVMUpstream(t, 1000, 1), "lag": lagUp})
			hcm.GetHealthChecker("lead", "http").setBlockNumber(995, time.Now().Add(-time.Minute)) // Ethereum pace
			lag := hcm.GetHealthChecker("lag", "http")
			lag.TaintHealthCheck("block lag")
			makeDue(lag)

			for _, n := range order {
				hcm.GetHealthChecker(n, "http").CheckAndSetHealth()
			}
			assert.True(t, lag.IsTainted(), "still 10 behind")
			assert.False(t, hcm.GetHealthChecker("lead", "http").IsTainted())

			// Caught up: the next due recheck, a cycle later, lifts it
			lagUp.block.Store(1000)
			ageSamples(hcm, time.Minute)
			makeDue(lag)
			for _, n := range order {
				hcm.GetHealthChecker(n, "http").CheckAndSetHealth()
			}
			assert.False(t, lag.IsTainted())
		})
	}
}
