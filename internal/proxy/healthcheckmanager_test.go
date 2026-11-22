package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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

// Wiring: a completed health check records its block and runs the gate.
func TestHealthCheckRunsBlockLagGate(t *testing.T) {
	upstream := func(block int) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":"0x%x"}`, block)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	config := createConfig()
	config.Path = t.Name()
	config.Logger = quietLogger
	config.HealthChecks.BlockDiffThreshold = 2
	config.Targets = []NodeProviderConfig{
		httpTarget("lead", upstream(1000).URL),
		httpTarget("lag", upstream(990).URL),
	}
	hcm, err := NewHealthCheckManager(config)
	require.NoError(t, err)
	t.Cleanup(hcm.Stop)
	lead, lag := hcm.GetHealthChecker("lead", "http"), hcm.GetHealthChecker("lag", "http")

	lead.CheckAndSetHealth()
	lag.CheckAndSetHealth()
	assert.Equal(t, uint64(1000), lead.BlockNumber())
	assert.Equal(t, uint64(990), lag.BlockNumber())
	assert.True(t, lag.IsTainted())
	assert.False(t, lead.IsTainted())
}
