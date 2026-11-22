package proxy

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var quietLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func newTestProxy(t *testing.T, chainType string, targets ...NodeProviderConfig) *Proxy {
	t.Helper()
	config := createConfig()
	config.Path = t.Name()
	config.ChainType = chainType
	config.Logger = quietLogger
	config.Targets = targets
	p, err := NewProxy(config)
	require.NoError(t, err)
	return p
}

func httpTarget(name, url string) NodeProviderConfig {
	return NodeProviderConfig{Name: name, Connection: ConnectionConfig{HTTP: HTTPConnection{URL: url}}}
}

func postRPC(p *Proxy, path string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"eth_chainId"}`))
	req.Header.Set("Content-Type", "application/json")
	if mutate != nil {
		mutate(req)
	}
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	return rr
}

func TestProviderQueryIsKeptAndMerged(t *testing.T) {
	var gotQuery atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery.Store(r.URL.RawQuery)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":9,"result":"0x1"}`))
	}))
	defer upstream.Close()
	p := newTestProxy(t, "", httpTarget("p", upstream.URL+"/?apikey=secret"))

	require.Equal(t, http.StatusOK, postRPC(p, "/", nil).Code)
	assert.Equal(t, "apikey=secret", gotQuery.Load(), "provider query must survive a client request without query")

	require.Equal(t, http.StatusOK, postRPC(p, "/?x=1", nil).Code)
	assert.Equal(t, "apikey=secret&x=1", gotQuery.Load())
}

func TestHopByHopHeadersAreNotForwarded(t *testing.T) {
	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Keep-Alive", "timeout=5")
		w.Header().Set("Proxy-Authenticate", "Basic")
		w.Header().Set("X-Upstream", "kept")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":9,"result":"0x1"}`))
	}))
	defer upstream.Close()
	p := newTestProxy(t, "", httpTarget("p", upstream.URL))

	rr := postRPC(p, "/", func(r *http.Request) {
		r.Header.Set("Connection", "X-Secret")
		r.Header.Set("X-Secret", "s")
		r.Header.Set("Te", "trailers")
		r.Header.Set("X-Client", "kept")
	})
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Empty(t, got.Get("X-Secret"), "headers named in Connection are hop-by-hop")
	assert.Empty(t, got.Get("Te"))
	assert.Equal(t, "kept", got.Get("X-Client"))
	assert.Empty(t, rr.Header().Get("Keep-Alive"))
	assert.Empty(t, rr.Header().Get("Proxy-Authenticate"))
	assert.Equal(t, "kept", rr.Header().Get("X-Upstream"))
}

func TestClientErrorsDoNotTaintProviders(t *testing.T) {
	var hitsA, hitsB atomic.Int32
	statusA, statusB := http.StatusMethodNotAllowed, http.StatusMethodNotAllowed
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsA.Add(1)
		w.WriteHeader(statusA)
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsB.Add(1)
		if statusB != http.StatusOK {
			w.WriteHeader(statusB)
			_, _ = w.Write([]byte("method not allowed"))
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":9,"result":"0x1"}`))
	}))
	defer b.Close()
	p := newTestProxy(t, "", httpTarget("a", a.URL), httpTarget("b", b.URL))
	tainted := func() (bool, bool) {
		return p.hcm.GetHealthChecker("a", "http").IsTainted(), p.hcm.GetHealthChecker("b", "http").IsTainted()
	}

	// Every provider rejects the request: the client gets the provider's answer, nobody is tainted
	rr := postRPC(p, "/", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rr.Code)
	assert.Equal(t, "method not allowed", rr.Body.String())
	ta, tb := tainted()
	assert.False(t, ta)
	assert.False(t, tb)

	// One provider rejects, the next accepts: failover still works, still no taint
	statusB = http.StatusOK
	rr = postRPC(p, "/", nil)
	assert.Equal(t, http.StatusOK, rr.Code)
	ta, tb = tainted()
	assert.False(t, ta)
	assert.False(t, tb)

	// A real provider error still taints
	statusA = http.StatusBadGateway
	assert.Equal(t, http.StatusOK, postRPC(p, "/", nil).Code)
	ta, _ = tainted()
	assert.True(t, ta)
}

func TestTronPathKeepsEscapedCharacters(t *testing.T) {
	var gotURI string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.RequestURI
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	p := newTestProxy(t, "tron", httpTarget("p", upstream.URL))

	require.Equal(t, http.StatusOK, postRPC(p, "/wallet/a%3Fb", nil).Code)
	assert.Equal(t, "/wallet/a%3Fb", gotURI)
}

func TestErrorResponseKeepsIDWithCharsetContentType(t *testing.T) {
	p := newTestProxy(t, "") // no providers at all
	rr := postRPC(p, "/", func(r *http.Request) { r.Header.Set("Content-Type", "application/json; charset=utf-8") })
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.Contains(t, rr.Body.String(), `"id":9`)
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
	aHTTP, aWS, bHTTP := hcm.GetHealthChecker("a", "http"), hcm.GetHealthChecker("a", "websocket"), hcm.GetHealthChecker("b", "http")

	bHTTP.blockNumber.Store(100)
	aHTTP.blockNumber.Store(100)
	aWS.blockNumber.Store(90)
	hcm.checkBlockLagAndTaint(aWS, 90)
	assert.True(t, aWS.IsTainted(), "lagging WebSocket endpoint must be tainted")
	assert.False(t, aHTTP.IsTainted(), "in-sync HTTP endpoint of the same provider must not be")

	aWS.RemoveTaint()
	aWS.blockNumber.Store(100)
	aHTTP.blockNumber.Store(90)
	hcm.checkBlockLagAndTaint(aHTTP, 90)
	assert.True(t, aHTTP.IsTainted())
	assert.False(t, aWS.IsTainted())
}

// wsUpstream starts an echo WebSocket server counting accepted connections.
// If silent, it accepts but never reads, so it never answers pings.
func wsUpstream(t *testing.T, silent bool) (*httptest.Server, *atomic.Int32) {
	var conns atomic.Int32
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		conns.Add(1)
		defer conn.Close()
		if silent {
			// Raw read: never processes (or answers) pings, returns once the conn closes
			_, _ = conn.UnderlyingConn().Read(make([]byte, 1))
			return
		}
		for {
			mt, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			_ = conn.WriteMessage(mt, msg)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &conns
}

// wsGateway serves a Proxy with one WebSocket target; an optional ping interval
// overrides the default for that proxy only.
func wsGateway(t testing.TB, upstreamURL string, ping ...time.Duration) *httptest.Server {
	config := createConfig()
	config.Path = t.Name()
	config.Timeout = 200 * time.Millisecond
	config.Logger = quietLogger
	config.Targets = []NodeProviderConfig{{Name: "p", Connection: ConnectionConfig{
		WebSocket: WebSocketConnection{URL: "ws" + strings.TrimPrefix(upstreamURL, "http")},
	}}}
	p, err := NewProxy(config)
	require.NoError(t, err)
	if len(ping) > 0 {
		p.targets[0].GetWebSocketProxy().pingInterval = ping[0]
	}
	gw := httptest.NewServer(p)
	t.Cleanup(gw.Close)
	return gw
}

func TestWebSocketDialsUpstreamOncePerClient(t *testing.T) {
	upstream, conns := wsUpstream(t, false)
	gw := wsGateway(t, upstream.URL)

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gw.URL, "http"), nil)
	require.NoError(t, err)
	defer client.Close()
	require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte("hi")))
	_, msg, err := client.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, "hi", string(msg))
	assert.Equal(t, int32(1), conns.Load(), "one upstream connection per client")
}

func TestWebSocketDropsSilentPeerAndLeaksNoGoroutines(t *testing.T) {
	upstream, _ := wsUpstream(t, true)
	gw := wsGateway(t, upstream.URL, 50*time.Millisecond)

	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gw.URL, "http"), nil)
			if !assert.NoError(t, err) {
				return
			}
			defer client.Close()
			// Upstream never answers pings: the gateway must give up and close us
			_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
			_, _, err = client.ReadMessage()
			var netErr net.Error
			if assert.Error(t, err) {
				assert.False(t, errorsAsTimeout(err, &netErr), "client should be closed by the gateway, not time out itself")
			}
		})
	}
	wg.Wait()

	// Ping goroutines and pipes must be gone once connections end. Count only
	// goroutines inside the WebSocket proxy: other tests run background checkers.
	proxyGoroutines := func() (int, string) {
		buf := make([]byte, 1<<20)
		all := string(buf[:runtime.Stack(buf, true)])
		n := 0
		for g := range strings.SplitSeq(all, "\n\n") {
			if strings.Contains(g, "proxy.(*WebSocketProxy).serve") {
				n++
			}
		}
		return n, all
	}
	deadline := time.Now().Add(2 * time.Second)
	n, dump := proxyGoroutines()
	for n > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		n, dump = proxyGoroutines()
	}
	if !assert.Zero(t, n, "WebSocket proxy goroutines leaked") {
		t.Log(dump)
	}
}

func errorsAsTimeout(err error, target *net.Error) bool {
	ne, ok := err.(net.Error)
	if ok {
		*target = ne
		return ne.Timeout()
	}
	return false
}

func TestRequestBodySizeLimit(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
	}))
	defer upstream.Close()
	p := newTestProxy(t, "", httpTarget("p", upstream.URL))
	gw := httptest.NewServer(p)
	defer gw.Close()

	post := func(body io.Reader, contentLength int64) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, gw.URL, body)
		require.NoError(t, err)
		req.ContentLength = contentLength // -1 sends chunked, without a declared length
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}

	// At the limit: forwarded
	atLimit := strings.Repeat(" ", MaxRequestBodySize-2) + "{}"
	assert.Equal(t, http.StatusOK, post(strings.NewReader(atLimit), int64(len(atLimit))).StatusCode)

	// Over the limit, declared length: rejected before reading
	over := atLimit + " "
	assert.Equal(t, http.StatusRequestEntityTooLarge, post(strings.NewReader(over), int64(len(over))).StatusCode)

	// Over the limit, chunked (no length): rejected while reading
	assert.Equal(t, http.StatusRequestEntityTooLarge, post(io.NopCloser(strings.NewReader(over)), -1).StatusCode)

	assert.Equal(t, int32(1), hits.Load(), "oversized requests must not reach the provider")
	assert.False(t, p.hcm.GetHealthChecker("p", "http").IsTainted(), "oversized requests must not taint")
}

// streamingUpstream sends response headers after headerDelay, then `chunks`
// chunks separated by gap, flushing each one.
func streamingUpstream(t *testing.T, headerDelay, gap time.Duration, chunks int) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(headerDelay)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for i := range chunks {
			if i > 0 {
				time.Sleep(gap)
			}
			_, _ = w.Write([]byte(strings.Repeat("x", 1000)))
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTimeoutProxy(t *testing.T, timeout time.Duration, urls ...string) *Proxy {
	t.Helper()
	config := createConfig()
	config.Path = t.Name()
	config.Timeout = timeout
	config.Logger = quietLogger
	for i, u := range urls {
		config.Targets = append(config.Targets, httpTarget(fmt.Sprintf("p%d", i), u))
	}
	p, err := NewProxy(config)
	require.NoError(t, err)
	return p
}

func TestLongStreamingResponseIsNotCutByTimeout(t *testing.T) {
	const timeout = 200 * time.Millisecond
	// 12 chunks, 100 ms apart: ~1.1 s total, over 5x the timeout, never stalling
	p := newTimeoutProxy(t, timeout, streamingUpstream(t, 0, 100*time.Millisecond, 12).URL)
	start := time.Now()
	rr := postRPC(p, "/", nil)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, 12*1000, rr.Body.Len(), "full body must arrive")
	assert.Greater(t, time.Since(start), 5*timeout)
	assert.False(t, p.hcm.GetHealthChecker("p0", "http").IsTainted())
}

func TestSlowFirstByteFailsOver(t *testing.T) {
	const timeout = 200 * time.Millisecond
	slow := streamingUpstream(t, time.Second, 0, 1)
	fast := streamingUpstream(t, 0, 0, 1)
	p := newTimeoutProxy(t, timeout, slow.URL, fast.URL)
	start := time.Now()
	rr := postRPC(p, "/", nil)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, 1000, rr.Body.Len())
	assert.Less(t, time.Since(start), 800*time.Millisecond, "must give up on the slow provider after ~timeout")
	assert.True(t, p.hcm.GetHealthChecker("p0", "http").IsTainted())
	assert.False(t, p.hcm.GetHealthChecker("p1", "http").IsTainted())
}

func TestStallMidResponseIsCutAndTainted(t *testing.T) {
	const timeout = 200 * time.Millisecond
	// Second chunk comes 2 s after the first: a stall well over the timeout
	p := newTimeoutProxy(t, timeout, streamingUpstream(t, 0, 2*time.Second, 2).URL)
	start := time.Now()
	rr := postRPC(p, "/", nil)
	assert.Less(t, time.Since(start), time.Second, "stall must be cut after ~timeout")
	assert.Equal(t, 1000, rr.Body.Len(), "only the data sent before the stall")
	assert.True(t, p.hcm.GetHealthChecker("p0", "http").IsTainted())
}

// slowWriter accepts data slowly, like a client on a poor link.
type slowWriter struct {
	discardWriter
	delay time.Duration
}

func (w *slowWriter) Write(b []byte) (int, error) {
	time.Sleep(w.delay)
	return len(b), nil
}

func TestSlowClientIsNotBlamedOnProvider(t *testing.T) {
	const timeout = 200 * time.Millisecond
	// Provider is fast; the client takes 2x the timeout to accept each write
	p := newTimeoutProxy(t, timeout, streamingUpstream(t, 0, 0, 3).URL)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	w := &slowWriter{discardWriter: discardWriter{h: make(http.Header)}, delay: 2 * timeout}
	p.ServeHTTP(w, req)
	assert.False(t, p.hcm.GetHealthChecker("p0", "http").IsTainted())
}

func TestRetryAfterParsing(t *testing.T) {
	for _, tc := range []struct {
		headers map[string]string
		want    time.Duration
	}{
		{map[string]string{"Retry-After": "60"}, 60 * time.Second},
		{map[string]string{"Retry-After": "1"}, time.Second},
		{map[string]string{"X-Ratelimit-Reset": "1"}, time.Second},
		{map[string]string{"X-Ratelimit-Reset": "0"}, 0}, // base.org: "now", use default
		{map[string]string{"Retry-After": "garbage"}, 0},
		{map[string]string{}, 0},
	} {
		h := http.Header{}
		for k, v := range tc.headers {
			h.Set(k, v)
		}
		assert.Equal(t, tc.want, retryAfter(h), "%v", tc.headers)
	}
	// HTTP date and Unix timestamp forms
	h := http.Header{"Retry-After": {time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)}}
	assert.InDelta(t, 30, retryAfter(h).Seconds(), 1.5)
	h = http.Header{"X-Ratelimit-Reset": {fmt.Sprint(time.Now().Add(20 * time.Second).Unix())}}
	assert.InDelta(t, 20, retryAfter(h).Seconds(), 1.5)
}

// rateLimitedUpstream answers 429 (with optional Retry-After) while limited is set,
// counting live requests and eth_blockNumber probes separately.
type rateLimitedUpstream struct {
	limited      atomic.Bool
	retryAfter   string
	live, probes atomic.Int32
	*httptest.Server
}

func newRateLimitedUpstream(t *testing.T, retryAfter string) *rateLimitedUpstream {
	u := &rateLimitedUpstream{retryAfter: retryAfter}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "eth_blockNumber") {
			u.probes.Add(1)
		} else {
			u.live.Add(1)
		}
		if u.limited.Load() {
			if u.retryAfter != "" {
				w.Header().Set("Retry-After", u.retryAfter)
			}
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x10"}`))
	}))
	t.Cleanup(u.Close)
	return u
}

func TestRateLimitedProviderRecoversViaProbe(t *testing.T) {
	a := newRateLimitedUpstream(t, "") // no hint: 1 s default, doubling
	b := newRateLimitedUpstream(t, "")
	p := newTestProxy(t, "", httpTarget("a", a.URL), httpTarget("b", b.URL))
	hcA := p.hcm.GetHealthChecker("a", "http")

	a.limited.Store(true)
	require.Equal(t, http.StatusOK, postRPC(p, "/", nil).Code, "fails over to b")
	require.True(t, hcA.IsTainted())

	// First probe after ~1 s: still limited, so a stays tainted and the wait doubles
	time.Sleep(1300 * time.Millisecond)
	assert.Equal(t, int32(1), a.probes.Load())
	assert.True(t, hcA.IsTainted())

	// Limit lifts; the next probe (~2 s later) restores a
	a.limited.Store(false)
	for range 4 { // live traffic meanwhile goes to b only
		require.Equal(t, http.StatusOK, postRPC(p, "/", nil).Code)
	}
	time.Sleep(2300 * time.Millisecond)
	assert.Equal(t, int32(2), a.probes.Load())
	assert.False(t, hcA.IsTainted())
	assert.Equal(t, int32(1), a.live.Load(), "no live request reached a while it was limited")

	// Back to priority order: a serves again
	require.Equal(t, http.StatusOK, postRPC(p, "/", nil).Code)
	assert.Equal(t, int32(2), a.live.Load())
}

func TestRateLimitHonoursRetryAfter(t *testing.T) {
	a := newRateLimitedUpstream(t, "3")
	b := newRateLimitedUpstream(t, "")
	p := newTestProxy(t, "", httpTarget("a", a.URL), httpTarget("b", b.URL))
	hcA := p.hcm.GetHealthChecker("a", "http")

	a.limited.Store(true)
	require.Equal(t, http.StatusOK, postRPC(p, "/", nil).Code)
	a.limited.Store(false)

	time.Sleep(1500 * time.Millisecond) // past the 1 s default, before Retry-After
	assert.True(t, hcA.IsTainted())
	assert.Zero(t, a.probes.Load(), "no probe before Retry-After elapses")

	time.Sleep(2000 * time.Millisecond)
	assert.Equal(t, int32(1), a.probes.Load())
	assert.False(t, hcA.IsTainted())
}
