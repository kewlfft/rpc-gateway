package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestWebSocketProxy_PipesBothWaysAndForwardsClose checks that messages pass through
// unchanged in both directions (text and binary) and that a graceful upstream close
// reaches the client as a normal close frame.
func TestWebSocketProxy_PipesBothWaysAndForwardsClose(t *testing.T) {
	upgrader := websocket.Upgrader{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			mt, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if string(msg) == "bye" {
				_ = conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
				return
			}
			// Echo, then push an unsolicited notification
			_ = conn.WriteMessage(mt, msg)
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","method":"eth_subscription","params":{"subscription":"0x1","result":{"number":"0x1"}}}`))
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
	defer client.Close()

	expect := func(wantType int, want string) {
		t.Helper()
		mt, msg, err := client.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if mt != wantType || string(msg) != want {
			t.Fatalf("got type %d %q, want type %d %q", mt, msg, wantType, want)
		}
	}
	notification := `{"jsonrpc":"2.0","method":"eth_subscription","params":{"subscription":"0x1","result":{"number":"0x1"}}}`

	req := `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`
	if err := client.WriteMessage(websocket.TextMessage, []byte(req)); err != nil {
		t.Fatal(err)
	}
	expect(websocket.TextMessage, req)
	expect(websocket.TextMessage, notification)

	bin := []byte{0x00, 0xff, 0x10}
	if err := client.WriteMessage(websocket.BinaryMessage, bin); err != nil {
		t.Fatal(err)
	}
	expect(websocket.BinaryMessage, string(bin))
	expect(websocket.TextMessage, notification)

	if err := client.WriteMessage(websocket.TextMessage, []byte("bye")); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = client.ReadMessage()
	if !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		t.Fatalf("expected normal close from upstream, got %v", err)
	}
}
