package proxy

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

// Production default level is warn; debug calls must stay cheap at that level.
var benchLogger = slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn}))

const benchReqBody = `{"jsonrpc":"2.0","id":42,"method":"eth_call","params":[{"to":"0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48","data":"0x70a08231000000000000000000000000000000000000000000000000000000000000dead"},"latest"]}`

var benchRespBody = []byte(`{"jsonrpc":"2.0","id":42,"result":"0x` + strings.Repeat("0", 64) + `"}`)

// memTransport answers every request in-memory, so the benchmark measures gateway CPU only.
type memTransport struct{}

func (memTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	_, _ = io.Copy(io.Discard, r.Body)
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(benchRespBody)),
		ContentLength: int64(len(benchRespBody)),
		Request:       r,
	}, nil
}

// discardWriter is a minimal ResponseWriter that drops the body.
type discardWriter struct{ h http.Header }

func (w *discardWriter) Header() http.Header         { return w.h }
func (w *discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *discardWriter) WriteHeader(int)             {}

func newBenchProxy(b *testing.B) *Proxy {
	b.Helper()
	config := createConfig()
	config.Path = "bench-" + b.Name()
	config.Logger = benchLogger
	for i := range 3 {
		config.Targets = append(config.Targets, NodeProviderConfig{
			Name:       fmt.Sprintf("p%d", i),
			Connection: ConnectionConfig{HTTP: HTTPConnection{URL: "http://upstream.invalid/"}},
		})
	}
	p, err := NewProxy(config)
	if err != nil {
		b.Fatal(err)
	}
	p.transport = memTransport{}
	return p
}

func BenchmarkServeHTTP(b *testing.B) {
	p := newBenchProxy(b)
	body := []byte(benchReqBody)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		w := &discardWriter{h: make(http.Header)}
		for pb.Next() {
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			clear(w.h)
			p.ServeHTTP(w, req)
		}
	})
}

// BenchmarkWebSocketNotifications measures proxying upstream subscription
// notifications (newHeads-sized) to a client through WebSocketProxy.
func BenchmarkWebSocketNotifications(b *testing.B) {
	notification := []byte(`{"jsonrpc":"2.0","method":"eth_subscription","params":{"subscription":"0x9cef478923ff08bf67fde6c64013158d","result":{"baseFeePerGas":"0x3b9aca00","difficulty":"0x0","extraData":"0x6265617665726275696c642e6f7267","gasLimit":"0x1c9c380","gasUsed":"0xd2a1d2","hash":"0x` + strings.Repeat("ab", 32) + `","logsBloom":"0x` + strings.Repeat("00", 256) + `","miner":"0x95222290dd7278aa3ddd389cc1e1d165cc4bafe5","mixHash":"0x` + strings.Repeat("cd", 32) + `","nonce":"0x0000000000000000","number":"0x13a5e1f","parentHash":"0x` + strings.Repeat("ef", 32) + `","receiptsRoot":"0x` + strings.Repeat("12", 32) + `","sha3Uncles":"0x` + strings.Repeat("34", 32) + `","size":"0x1f2a","stateRoot":"0x` + strings.Repeat("56", 32) + `","timestamp":"0x6553f100","transactionsRoot":"0x` + strings.Repeat("78", 32) + `"}}}`)

	start := make(chan int, 1)
	upgrader := websocket.Upgrader{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Pre-flight connections from the failover check get closed without a start signal.
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		n, ok := <-start
		if !ok {
			return
		}
		for range n {
			if err := conn.WriteMessage(websocket.TextMessage, notification); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()

	gateway := wsGateway(b, upstream.URL)

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http"), nil)
	if err != nil {
		b.Fatal(err)
	}
	defer client.Close()

	b.SetBytes(int64(len(notification)))
	b.ReportAllocs()
	b.ResetTimer()
	start <- b.N
	for range b.N {
		if _, _, err := client.ReadMessage(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	close(start)
}

// BenchmarkServeHTTPLoopback runs the full path: client -> gateway http.Server ->
// Proxy -> pooled transport -> upstream http.Server, all over loopback.
func BenchmarkServeHTTPLoopback(b *testing.B) {
	var upstreamConns, gatewayConns atomic.Int64
	countNew := func(n *atomic.Int64) func(net.Conn, http.ConnState) {
		return func(_ net.Conn, s http.ConnState) {
			if s == http.StateNew {
				n.Add(1)
			}
		}
	}
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(benchRespBody)
	}))
	upstream.Config.ConnState = countNew(&upstreamConns)
	upstream.Start()
	defer upstream.Close()

	config := createConfig()
	config.Path = "bench-loopback"
	config.Logger = benchLogger
	config.Targets = []NodeProviderConfig{{
		Name:       "p0",
		Connection: ConnectionConfig{HTTP: HTTPConnection{URL: upstream.URL}},
	}}
	p, err := NewProxy(config)
	if err != nil {
		b.Fatal(err)
	}
	gateway := httptest.NewUnstartedServer(p)
	gateway.Config.ConnState = countNew(&gatewayConns)
	gateway.Start()
	defer gateway.Close()

	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 256}}
	body := []byte(benchReqBody)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := client.Post(gateway.URL, "application/json", bytes.NewReader(body))
			if err != nil {
				b.Error(err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				b.Errorf("status %d", resp.StatusCode)
				return
			}
		}
	})
	b.StopTimer()
	// Connections opened per request; should be ~0 when keep-alive pooling works
	b.ReportMetric(float64(upstreamConns.Load())/float64(b.N), "upconns/op")
	b.ReportMetric(float64(gatewayConns.Load())/float64(b.N), "gwconns/op")
}
