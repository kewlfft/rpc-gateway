package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	bufferSize      = 1024
	handshakeTimeout = 45 * time.Second
	pingInterval     = 15 * time.Second
	pingTimeout      = 2 * time.Second
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  bufferSize,
	WriteBufferSize: bufferSize,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

type WebSocketProxy struct {
	targetURL string
	logger    *slog.Logger
	// Subscription tracking
	subscriptions map[string]bool
	mu            sync.RWMutex
}

func NewWebSocketProxy(targetURL string, logger *slog.Logger) *WebSocketProxy {
	return &WebSocketProxy{
		targetURL: targetURL, 
		logger: logger,
		subscriptions: make(map[string]bool),
	}
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
		HandshakeTimeout:  handshakeTimeout,
		ReadBufferSize:    bufferSize,
		WriteBufferSize:   bufferSize,
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
			
			// Read the message to parse for subscriptions
			msgBytes, err := io.ReadAll(reader)
			if err != nil {
				p.logger.Debug(direction+" read error", "error", err)
				errCh <- err
				return
			}
			
			// Track subscriptions if this is a client->target message
			if direction == "client->target" {
				p.trackSubscriptionFromMessage(msgBytes)
			} else if direction == "target->client" {
				p.trackSubscriptionResponse(msgBytes)
			}
			
			writer, err := dst.NextWriter(mt)
			if err != nil {
				p.logger.Debug(direction+" write error", "error", err)
				errCh <- err
				return
			}
			if _, err = io.Copy(writer, bytes.NewReader(msgBytes)); err != nil {
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
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	go func() {
		for {
			select {
			case <-ticker.C:
				deadline := time.Now().Add(pingTimeout)
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

// TrackSubscription adds a subscription ID to the tracking map
func (p *WebSocketProxy) TrackSubscription(subID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subscriptions[subID] = true
	p.logger.Debug("subscription tracked", "subID", subID)
}

// RemoveSubscription removes a subscription ID from the tracking map
func (p *WebSocketProxy) RemoveSubscription(subID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.subscriptions, subID)
	p.logger.Debug("subscription removed", "subID", subID)
}

// UnsubscribeAll sends unsubscribe messages for all tracked subscriptions
func (p *WebSocketProxy) UnsubscribeAll() {
	p.mu.Lock()
	subIDs := make([]string, 0, len(p.subscriptions))
	for subID := range p.subscriptions {
		subIDs = append(subIDs, subID)
	}
	p.subscriptions = make(map[string]bool)
	p.mu.Unlock()

	if len(subIDs) == 0 {
		return
	}

	p.logger.Info("unsubscribing from all subscriptions", "count", len(subIDs))
	
	// Create a new connection to send unsubscribe messages
	dialer := websocket.Dialer{
		HandshakeTimeout:  handshakeTimeout,
		ReadBufferSize:    bufferSize,
		WriteBufferSize:   bufferSize,
		EnableCompression: true,
	}

	conn, _, err := dialer.Dial(p.targetURL, nil)
	if err != nil {
		p.logger.Error("failed to create connection for unsubscribe", "error", err)
		return
	}
	defer conn.Close()

	// Send unsubscribe messages for each subscription
	for _, subID := range subIDs {
		unsubMsg := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "eth_unsubscribe",
			"params":  []string{subID},
		}
		
		if err := conn.WriteJSON(unsubMsg); err != nil {
			p.logger.Error("failed to send unsubscribe message", "subID", subID, "error", err)
		} else {
			p.logger.Debug("unsubscribe message sent", "subID", subID)
		}
	}
}

func (p *WebSocketProxy) trackSubscriptionFromMessage(msgBytes []byte) {
	// Parse JSON-RPC message
	var msg map[string]interface{}
	if err := json.Unmarshal(msgBytes, &msg); err != nil {
		return // Not a valid JSON message
	}

	method, ok := msg["method"].(string)
	if !ok {
		return // No method field
	}

	// Check if this is a subscription request
	if method == "eth_subscribe" || method == "shh_subscribe" || method == "net_subscribe" {
		// Extract subscription ID from response (we'll track it when we see the response)
		p.logger.Debug("subscription request detected", "method", method)
	}

	// Check if this is an unsubscribe request
	if method == "eth_unsubscribe" || method == "shh_unsubscribe" || method == "net_unsubscribe" {
		if params, ok := msg["params"].([]interface{}); ok && len(params) > 0 {
			if subID, ok := params[0].(string); ok {
				p.RemoveSubscription(subID)
			}
		}
	}
}

func (p *WebSocketProxy) trackSubscriptionResponse(msgBytes []byte) {
	// Parse JSON-RPC message
	var msg map[string]interface{}
	if err := json.Unmarshal(msgBytes, &msg); err != nil {
		return // Not a valid JSON message
	}

	// Check if this is a response with a result (subscription ID)
	if result, ok := msg["result"]; ok {
		if subID, ok := result.(string); ok {
			// This is likely a subscription ID from a successful subscription
			p.TrackSubscription(subID)
			p.logger.Debug("subscription ID tracked from response", "subID", subID)
		}
	}
} 