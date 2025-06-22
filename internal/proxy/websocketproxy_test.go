package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"log/slog"
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
	proxy := NewWebSocketProxy(wsURL, logger)

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

	for i, msg := range received {
		if !contains(msg, subIDs[i]) {
			t.Errorf("unsubscribe message %d does not contain subID %q: %s", i, subIDs[i], msg)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || (len(s) > len(substr) && (contains(s[1:], substr) || contains(s[:len(s)-1], substr))))
} 