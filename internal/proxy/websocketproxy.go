package proxy

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	jsonv2 "encoding/json/v2"

	"github.com/gorilla/websocket"
)

const (
	bufferSize   = 16384
	pingInterval = 15 * time.Second
)

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

type WebSocketProxy struct {
	targetURL string
	logger    *slog.Logger
	timeout   time.Duration // Timeout for connection attempts
	// Subscription tracking
	subscriptions map[string]bool
	mu            sync.RWMutex
	// Connection pooling
	connPool chan *websocket.Conn
	// Prevent concurrent UnsubscribeAll calls
	unsubscribing sync.Mutex
}

func NewWebSocketProxy(targetURL string, timeout time.Duration, logger *slog.Logger) *WebSocketProxy {
	return &WebSocketProxy{
		targetURL:     targetURL,
		logger:        logger,
		timeout:       timeout,
		subscriptions: make(map[string]bool),
		connPool:      make(chan *websocket.Conn, 5), // Pool of 5 connections
	}
}

func (p *WebSocketProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		p.logger.Error("websocket upgrade failed", "error", err)
		return
	}
	defer clientConn.Close()

	// Create dedicated connection for this client (no pooling for subscriptions)
	targetConn := p.createNewConnection()
	if targetConn == nil {
		p.logger.Error("failed to create target connection",
			"target_url", p.targetURL)
		return
	}
	defer targetConn.Close()

	// Subscriptions of this client connection; shared by both pipe goroutines
	subs := newConnSubscriptions()
	debugEnabled := p.logger.Enabled(context.Background(), slog.LevelDebug)

	// Buffer of 3 to handle errors from 2 pipes + ping goroutine without blocking
	errCh := make(chan upstreamCloseInfo, 3)
	var once sync.Once
	closeAll := func(isGracefulClose bool) {
		once.Do(func() {
			// Clean up subscriptions for this connection
			p.cleanupConnectionSubscriptions(subs)
			// If upstream closed gracefully, forward close frame to client
			if isGracefulClose {
				deadline := time.Now().Add(2 * time.Second)
				// Ignore error - connection may already be closed
				_ = clientConn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), deadline)
			}
			clientConn.Close()
			targetConn.Close()
		})
	}

	fail := func(direction, what string, err error) {
		p.logger.Debug(direction+" "+what+" error",
			"error", err,
			"target_url", p.targetURL)
		errCh <- upstreamCloseInfo{err: err, isGracefulClose: false}
	}

	// client->target: client requests are small, so read each one whole and record
	// subscription requests BEFORE forwarding, so the upstream reply can never be
	// processed before its request is known.
	clientToTarget := func() {
		const direction = "client->target"
		for {
			mt, msg, err := clientConn.ReadMessage()
			if err != nil {
				p.logger.Debug(direction+" read error",
					"error", err,
					"target_url", p.targetURL,
					"is_close", false)
				errCh <- upstreamCloseInfo{err: err, isGracefulClose: false}
				return
			}
			p.trackSubscriptionFromMessage(msg, subs)
			if err := targetConn.WriteMessage(mt, msg); err != nil {
				fail(direction, "write", err)
				return
			}
		}
	}

	// target->client: stream every message; only parse it when it can matter
	// (a subscription reply is pending, or debug logging of upstream errors is on).
	targetToClient := func() {
		const direction = "target->client"
		var buf bytes.Buffer
		for {
			mt, reader, err := targetConn.NextReader()
			if err != nil {
				// A close frame from upstream is a graceful close
				isGracefulClose := websocket.IsCloseError(err)
				p.logger.Debug(direction+" read error",
					"error", err,
					"target_url", p.targetURL,
					"is_close", isGracefulClose)
				errCh <- upstreamCloseInfo{err: err, isGracefulClose: isGracefulClose}
				return
			}

			writer, err := clientConn.NextWriter(mt)
			if err != nil {
				fail(direction, "write", err)
				return
			}

			parse := debugEnabled || subs.hasPending()
			src := reader
			if parse {
				buf.Reset()
				src = io.TeeReader(reader, &buf)
			}
			if _, err = io.Copy(writer, src); err != nil {
				writer.Close()
				fail(direction, "copy", err)
				return
			}
			if err = writer.Close(); err != nil {
				fail(direction, "close", err)
				return
			}

			if parse {
				var msg wsMessage
				if jsonv2.Unmarshal(buf.Bytes(), &msg) == nil {
					p.trackSubscriptionResponse(&msg, subs)
					if debugEnabled {
						p.logJSONRPCError(msg.Error)
					}
				}
			}
		}
	}

	go clientToTarget()
	go targetToClient()

	// Periodic ping to keep the connection alive
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	go func() {
		for range ticker.C {
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
	closeAll(closeInfo.isGracefulClose)
	p.logger.Debug("websocket proxy terminated",
		"reason", closeInfo.err,
		"target_url", p.targetURL,
		"graceful_close", closeInfo.isGracefulClose)
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
	// Prevent concurrent calls to UnsubscribeAll
	p.unsubscribing.Lock()
	defer p.unsubscribing.Unlock()

	// Copy subscription IDs while holding the lock
	p.mu.Lock()
	subIDs := slices.Collect(maps.Keys(p.subscriptions))
	p.mu.Unlock()

	if len(subIDs) == 0 {
		return
	}

	p.logger.Info("unsubscribing from all subscriptions", "count", len(subIDs))

	// Get connection from pool or create new one
	conn := p.getConnection()
	if conn == nil {
		p.logger.Error("failed to get connection for unsubscribe")
		// Don't clear subscriptions if we can't get a connection
		// They will be cleaned up when connections close naturally
		return
	}
	defer p.returnConnection(conn)

	// Clear subscriptions map only after successfully getting a connection
	// This ensures we don't lose track of subscriptions if connection fails
	p.mu.Lock()
	clear(p.subscriptions)
	p.mu.Unlock()

	// Send unsubscribe messages for each subscription
	for _, subID := range subIDs {
		unsubMsg := map[string]any{
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

// wsMessage holds the JSON-RPC fields needed for subscription tracking.
// Other members (e.g. large notification payloads) are skipped, not decoded.
type wsMessage struct {
	ID     any            `json:"id"`
	Method string         `json:"method"`
	Params jsontext.Value `json:"params"`
	Result jsontext.Value `json:"result"`
	Error  jsontext.Value `json:"error"`
}

// connSubscriptions tracks the subscriptions of one client connection.
type connSubscriptions struct {
	mu      sync.Mutex
	active  map[string]bool // subscription IDs owned by this connection
	pending map[any]bool    // IDs of subscribe requests awaiting a reply
}

func newConnSubscriptions() *connSubscriptions {
	return &connSubscriptions{active: make(map[string]bool), pending: make(map[any]bool)}
}

func (c *connSubscriptions) hasPending() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending) > 0
}

func (p *WebSocketProxy) trackSubscriptionFromMessage(msgBytes []byte, subs *connSubscriptions) {
	var msg wsMessage
	if err := jsonv2.Unmarshal(msgBytes, &msg); err != nil || msg.Method == "" {
		return // Not a valid JSON-RPC request
	}

	switch msg.Method {
	case "eth_subscribe", "shh_subscribe", "net_subscribe":
		// Track the request ID so we can identify the subscription response
		if msg.ID != nil {
			subs.mu.Lock()
			subs.pending[msg.ID] = true
			subs.mu.Unlock()
			p.logger.Debug("subscription request detected", "method", msg.Method, "id", msg.ID)
		}
	case "eth_unsubscribe", "shh_unsubscribe", "net_unsubscribe":
		var params []any
		if jsonv2.Unmarshal(msg.Params, &params) == nil && len(params) > 0 {
			if subID, ok := params[0].(string); ok {
				// Remove from both connection-specific and global maps
				subs.mu.Lock()
				delete(subs.active, subID)
				subs.mu.Unlock()
				p.RemoveSubscription(subID)
			}
		}
	}
}

func (p *WebSocketProxy) trackSubscriptionResponse(msg *wsMessage, subs *connSubscriptions) {
	// Only track subscription IDs if this response corresponds to a pending subscription request
	subs.mu.Lock()
	if msg.ID == nil || !subs.pending[msg.ID] {
		subs.mu.Unlock()
		return // Not a subscription response
	}
	delete(subs.pending, msg.ID)

	var subID string
	if msg.Result.Kind() != '"' || jsonv2.Unmarshal(msg.Result, &subID) != nil {
		subs.mu.Unlock()
		return // No subscription ID (e.g. error response)
	}
	subs.active[subID] = true
	subs.mu.Unlock()

	p.TrackSubscription(subID)
	p.logger.Debug("subscription ID tracked from response", "subID", subID)
}

// logJSONRPCError logs JSON-RPC error responses from upstream providers (debug only)
func (p *WebSocketProxy) logJSONRPCError(rawErr jsontext.Value) {
	if rawErr.Kind() != '{' {
		return
	}
	var errObj map[string]any
	if jsonv2.Unmarshal(rawErr, &errObj) != nil || errObj == nil {
		return
	}
	code, _ := errObj["code"].(float64)
	message, _ := errObj["message"].(string)
	p.logger.Debug("JSON-RPC error from upstream provider",
		"target_url", p.targetURL,
		"error_code", int(code),
		"error_message", message)
}

// cleanupConnectionSubscriptions removes subscriptions for a specific connection
func (p *WebSocketProxy) cleanupConnectionSubscriptions(subs *connSubscriptions) {
	subs.mu.Lock()
	defer subs.mu.Unlock()
	if len(subs.active) == 0 {
		return
	}

	p.logger.Debug("cleaning up connection subscriptions", "count", len(subs.active))

	// Remove from global map
	p.mu.Lock()
	for subID := range subs.active {
		delete(p.subscriptions, subID)
	}
	p.mu.Unlock()

	clear(subs.active)
}

// getConnection gets a connection from the pool or creates a new one
func (p *WebSocketProxy) getConnection() *websocket.Conn {
	// Try to get from pool first
	select {
	case conn := <-p.connPool:
		// Test if connection is still alive
		if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(p.timeout)); err != nil {
			conn.Close()
			// Create new connection if pool connection is dead
			return p.createNewConnection()
		}
		return conn
	default:
		// Pool is empty, create new connection
		return p.createNewConnection()
	}
}

// returnConnection returns a connection to the pool
func (p *WebSocketProxy) returnConnection(conn *websocket.Conn) {
	if conn == nil {
		return
	}

	select {
	case p.connPool <- conn:
		// Connection returned to pool
	default:
		// Pool is full, close connection
		conn.Close()
	}
}

// createNewConnection creates a new WebSocket connection
func (p *WebSocketProxy) createNewConnection() *websocket.Conn {
	dialer := websocket.Dialer{
		HandshakeTimeout:  p.timeout,
		ReadBufferSize:    bufferSize,
		WriteBufferSize:   bufferSize,
		EnableCompression: true,
	}

	conn, _, err := dialer.Dial(p.targetURL, nil)
	if err != nil {
		p.logger.Error("failed to create new connection", "error", err)
		return nil
	}

	return conn
}
