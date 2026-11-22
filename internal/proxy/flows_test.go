package proxy

import (
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

func wsGateway(t *testing.T, upstreamURL string) *httptest.Server {
	config := createConfig()
	config.Path = t.Name()
	config.Timeout = 200 * time.Millisecond
	config.Logger = quietLogger
	config.Targets = []NodeProviderConfig{{Name: "p", Connection: ConnectionConfig{
		WebSocket: WebSocketConnection{URL: "ws" + strings.TrimPrefix(upstreamURL, "http")},
	}}}
	p, err := NewProxy(config)
	require.NoError(t, err)
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
	old := pingInterval
	pingInterval = 50 * time.Millisecond
	t.Cleanup(func() { pingInterval = old })

	upstream, _ := wsUpstream(t, true)
	gw := wsGateway(t, upstream.URL)

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
