package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

type WebSocketProxy struct {
	targetURL string
	logger    *slog.Logger
}

func NewWebSocketProxy(targetURL string, logger *slog.Logger) *WebSocketProxy {
	return &WebSocketProxy{targetURL: targetURL, logger: logger}
}

func (p *WebSocketProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.logger.Debug("handling websocket upgrade",
		"target", p.targetURL,
		"path", r.URL.Path)

	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		p.logger.Error("websocket upgrade failed", "error", err)
		return
	}
	defer clientConn.Close()

	dialer := websocket.Dialer{
		HandshakeTimeout:  45 * time.Second,
		ReadBufferSize:    1024,
		WriteBufferSize:   1024,
		EnableCompression: true,
	}

	targetConn, resp, err := dialer.Dial(p.targetURL, nil)
	if err != nil {
		p.logger.Error("dial to target failed", "error", err, "resp", resp)
		return
	}
	defer targetConn.Close()

	p.logger.Debug("websocket tunnel established")

	errCh := make(chan error, 2)
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			clientConn.Close()
			targetConn.Close()
		})
	}

	pipe := func(src, dst *websocket.Conn, direction string) {
		for {
			mt, reader, err := src.NextReader()
			if err != nil {
				p.logger.Debug(direction+" read error", "error", err)
				errCh <- err
				return
			}
			writer, err := dst.NextWriter(mt)
			if err != nil {
				p.logger.Debug(direction+" write error", "error", err)
				errCh <- err
				return
			}
			if _, err = io.Copy(writer, reader); err != nil {
				p.logger.Debug(direction+" copy error", "error", err)
				errCh <- err
				return
			}
			_ = writer.Close()
		}
	}

	go pipe(clientConn, targetConn, "client->target")
	go pipe(targetConn, clientConn, "target->client")

	// Periodic ping to keep the connection alive
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	go func() {
		for {
			select {
			case <-ticker.C:
				deadline := time.Now().Add(2 * time.Second)
				if err := clientConn.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
					errCh <- err
					return
				}
				if err := targetConn.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
					errCh <- err
					return
				}
			}
		}
	}()

	err = <-errCh
	closeAll()
	p.logger.Debug("websocket proxy terminated", "reason", err)
} 