package proxy

import (
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWebSocketProxy_UnsubscribeAll(t *testing.T) {
	// Set up a mock WebSocket server to capture unsubscribe messages
	var (
		received []string
		mu       sync.Mutex
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("failed to upgrade: %v", err)
		}
		defer conn.Close()

		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			mu.Lock()
			received = append(received, string(msg))
			mu.Unlock()
		}
	}))
	defer ts.Close()

	// Convert http://127.0.0.1 to ws://127.0.0.1
	u, _ := url.Parse(ts.URL)
	u.Scheme = "ws"
	wsURL := u.String()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := NewWebSocketProxy(wsURL, 45*time.Second, logger)

	// Simulate tracked subscriptions
	subIDs := []string{"sub1", "sub2", "sub3"}
	for _, id := range subIDs {
		proxy.TrackSubscription(id)
	}

	proxy.UnsubscribeAll()

	// Wait a bit for messages to be received
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != len(subIDs) {
		t.Fatalf("expected %d unsubscribe messages, got %d", len(subIDs), len(received))
	}

	for _, subID := range subIDs {
		if !slices.ContainsFunc(received, func(msg string) bool {
			return strings.Contains(msg, subID)
		}) {
			t.Errorf("subscription ID %q not found in any unsubscribe message", subID)
		}
	}
}

func TestWebSocketProxy_TracksSubscriptionsThroughPipe(t *testing.T) {
	upgrader := websocket.Upgrader{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var req struct {
				ID     any    `json:"id"`
				Method string `json:"method"`
			}
			if err := conn.ReadJSON(&req); err != nil {
				return
			}
			switch req.Method {
			case "eth_subscribe":
				_ = conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": "0xsub1"})
				// A notification must pass through untouched
				_ = conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "method": "eth_subscription",
					"params": map[string]any{"subscription": "0xsub1", "result": map[string]any{"number": "0x1"}}})
			case "eth_unsubscribe":
				_ = conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": true})
			case "eth_chainId":
				_ = conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": nil})
			}
		}
	}))
	defer upstream.Close()

	wsProxy := NewWebSocketProxy("ws"+strings.TrimPrefix(upstream.URL, "http"), 3*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	gateway := httptest.NewServer(wsProxy)
	defer gateway.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	subscriptions := func() []string {
		wsProxy.mu.RLock()
		defer wsProxy.mu.RUnlock()
		return slices.Sorted(maps.Keys(wsProxy.subscriptions))
	}
	waitFor := func(want []string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for !slices.Equal(subscriptions(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("subscriptions = %v, want %v", subscriptions(), want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	read := func() string {
		t.Helper()
		_, msg, err := client.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		return string(msg)
	}

	// A null-result reply to a non-subscription request is not tracked
	_ = client.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))
	read()

	_ = client.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":2,"method":"eth_subscribe","params":["newHeads"]}`))
	if got := read(); !strings.Contains(got, `"0xsub1"`) {
		t.Fatalf("unexpected subscribe reply %s", got)
	}
	if got := read(); !strings.Contains(got, `"eth_subscription"`) || !strings.Contains(got, `"number":"0x1"`) {
		t.Fatalf("notification not forwarded intact: %s", got)
	}
	waitFor([]string{"0xsub1"})

	_ = client.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":3,"method":"eth_unsubscribe","params":["0xsub1"]}`))
	read()
	waitFor(nil)

	// Subscriptions are cleaned up when the client disconnects
	_ = client.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":4,"method":"eth_subscribe","params":["newHeads"]}`))
	read()
	read()
	waitFor([]string{"0xsub1"})
	client.Close()
	waitFor(nil)
}
