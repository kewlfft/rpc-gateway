package proxy

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

const bufferSize = 16384

// pingInterval is how often both sides of a proxied WebSocket are pinged (var for tests).
var pingInterval = 15 * time.Second

// upstreamCloseInfo contains error and whether it's a graceful close from upstream
type upstreamCloseInfo struct {
	err             error
	isGracefulClose bool
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  bufferSize,
	WriteBufferSize: bufferSize,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// WebSocketProxy pipes a client WebSocket to a dedicated upstream connection.
// Subscriptions live on that upstream connection and end with it.
type WebSocketProxy struct {
	targetURL string
	logger    *slog.Logger
	timeout   time.Duration // Timeout for connection attempts and pings
}

func NewWebSocketProxy(targetURL string, timeout time.Duration, logger *slog.Logger) *WebSocketProxy {
	return &WebSocketProxy{
		targetURL: targetURL,
		logger:    logger,
		timeout:   timeout,
	}
}

// ServeHTTP dials upstream, then upgrades the client and pipes the two.
func (p *WebSocketProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	targetConn := p.createNewConnection()
	if targetConn == nil {
		http.Error(w, "upstream WebSocket unavailable", http.StatusBadGateway)
		return
	}
	p.serve(w, r, targetConn)
}

// serve upgrades the client and pipes it to the already-dialed upstream
// connection, which it takes ownership of.
func (p *WebSocketProxy) serve(w http.ResponseWriter, r *http.Request, targetConn *websocket.Conn) {
	defer targetConn.Close()
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		p.logger.Error("websocket upgrade failed", "error", err)
		return
	}
	defer clientConn.Close()

	// Detect dead peers: pings go out every pingInterval, and each pong or message
	// pushes the read deadline out. A peer silent for longer than readTimeout is
	// dropped instead of leaving the other side waiting forever.
	readTimeout := 2*pingInterval + p.timeout
	for _, c := range []*websocket.Conn{clientConn, targetConn} {
		_ = c.SetReadDeadline(time.Now().Add(readTimeout))
		c.SetPongHandler(func(string) error {
			return c.SetReadDeadline(time.Now().Add(readTimeout))
		})
	}

	// Buffer of 3 to handle errors from 2 pipes + ping goroutine without blocking
	errCh := make(chan upstreamCloseInfo, 3)

	// pipe streams each message from src to dst without buffering or parsing it.
	pipe := func(src, dst *websocket.Conn, direction string) {
		fail := func(what string, err error, graceful bool) {
			p.logger.Debug(direction+" "+what+" error",
				"error", err,
				"target_url", p.targetURL,
				"is_close", graceful)
			errCh <- upstreamCloseInfo{err: err, isGracefulClose: graceful}
		}
		for {
			mt, reader, err := src.NextReader()
			if err == nil {
				err = src.SetReadDeadline(time.Now().Add(readTimeout))
			}
			if err != nil {
				// A close frame from upstream is a graceful close to forward to the client
				// (IsCloseError with no codes always returns false, so check the type)
				var closeErr *websocket.CloseError
				fail("read", err, direction == "target->client" && errors.As(err, &closeErr))
				return
			}
			writer, err := dst.NextWriter(mt)
			if err != nil {
				fail("write", err, false)
				return
			}
			if _, err := io.Copy(writer, reader); err != nil {
				writer.Close()
				fail("copy", err, false)
				return
			}
			if err := writer.Close(); err != nil {
				fail("close", err, false)
				return
			}
		}
	}

	go pipe(clientConn, targetConn, "client->target")
	go pipe(targetConn, clientConn, "target->client")

	// Periodic ping to keep both connections alive; stops when serve returns
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
			}
			deadline := time.Now().Add(p.timeout)
			if err := clientConn.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
				errCh <- upstreamCloseInfo{err: err, isGracefulClose: false}
				return
			}
			if err := targetConn.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
				errCh <- upstreamCloseInfo{err: err, isGracefulClose: false}
				return
			}
		}
	}()

	closeInfo := <-errCh
	// If upstream closed gracefully, forward a close frame to the client
	if closeInfo.isGracefulClose {
		_ = clientConn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(2*time.Second))
	}
	clientConn.Close()
	targetConn.Close()
	p.logger.Debug("websocket proxy terminated",
		"reason", closeInfo.err,
		"target_url", p.targetURL,
		"graceful_close", closeInfo.isGracefulClose)
}

// createNewConnection dials a new upstream WebSocket connection, or returns nil on failure.
func (p *WebSocketProxy) createNewConnection() *websocket.Conn {
	dialer := websocket.Dialer{
		HandshakeTimeout:  p.timeout,
		ReadBufferSize:    bufferSize,
		WriteBufferSize:   bufferSize,
		EnableCompression: true,
	}

	conn, resp, err := dialer.Dial(p.targetURL, nil)
	if err != nil {
		// The status (e.g. 403 IP blocked, 429 rate limited) says why a handshake failed
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		p.logger.Error("failed to create new connection", "error", err, "status", status)
		return nil
	}
	return conn
}
