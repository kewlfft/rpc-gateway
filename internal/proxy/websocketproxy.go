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
	bufferSize      = 16384
	pingInterval     = 15 * time.Second
	pingTimeout      = 2 * time.Second
)

// upstreamCloseInfo contains error and whether it's a graceful close from upstream
type upstreamCloseInfo struct {
	err           error
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
	poolMu   sync.Mutex
}

func NewWebSocketProxy(targetURL string, timeout time.Duration, logger *slog.Logger) *WebSocketProxy {
	return &WebSocketProxy{
		targetURL: targetURL,
		logger:    logger,
		timeout:   timeout,
		subscriptions: make(map[string]bool),
		connPool: make(chan *websocket.Conn, 5), // Pool of 5 connections
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

	// Track subscriptions for this specific connection
	connectionSubscriptions := make(map[string]bool)
	// Track pending subscription request IDs to identify subscription responses
	pendingSubscriptionRequests := make(map[interface{}]bool)
	
	// Buffer of 3 to handle errors from 2 pipes + ping goroutine without blocking
	errCh := make(chan upstreamCloseInfo, 3)
	var once sync.Once
	closeAll := func(isGracefulClose bool) {
		once.Do(func() {
			// Clean up subscriptions for this connection
			p.cleanupConnectionSubscriptions(connectionSubscriptions)
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

	pipe := func(src, dst *websocket.Conn, direction string) {
		for {
			mt, reader, err := src.NextReader()
			if err != nil {
				// Check if this is a graceful close from upstream
				isGracefulClose := direction == "target->client" && websocket.IsCloseError(err)
				p.logger.Debug(direction+" read error",
					"error", err,
					"target_url", p.targetURL,
					"is_close", isGracefulClose)
				// Send error info (non-blocking - channel has sufficient buffer)
				errCh <- upstreamCloseInfo{err: err, isGracefulClose: isGracefulClose}
				return
			}
			
			// Get writer first to start forwarding immediately
			writer, err := dst.NextWriter(mt)
			if err != nil {
				p.logger.Debug(direction+" write error",
					"error", err,
					"target_url", p.targetURL)
				errCh <- upstreamCloseInfo{err: err, isGracefulClose: false}
				return
			}
			
			// Buffer for parsing while forwarding
			var buf bytes.Buffer
			teeReader := io.TeeReader(reader, &buf)
			
			// Forward immediately while also buffering
			if _, err = io.Copy(writer, teeReader); err != nil {
				writer.Close()
				p.logger.Debug(direction+" copy error",
					"error", err,
					"target_url", p.targetURL)
				errCh <- upstreamCloseInfo{err: err, isGracefulClose: false}
				return
			}
			if err = writer.Close(); err != nil {
				p.logger.Debug(direction+" close error",
					"error", err,
					"target_url", p.targetURL)
				errCh <- upstreamCloseInfo{err: err, isGracefulClose: false}
				return
			}
			
			// Parse from buffer (non-blocking for next message)
			msgBytes := buf.Bytes()
			if direction == "client->target" {
				p.trackSubscriptionFromMessage(msgBytes, connectionSubscriptions, pendingSubscriptionRequests)
			} else if direction == "target->client" {
				p.trackSubscriptionResponse(msgBytes, connectionSubscriptions, pendingSubscriptionRequests)
				// Log JSON-RPC errors from upstream provider
				p.logJSONRPCError(msgBytes)
			}
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
					errCh <- upstreamCloseInfo{err: err, isGracefulClose: false}
					return
				}
				if err := targetConn.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
					errCh <- upstreamCloseInfo{err: err, isGracefulClose: false}
					return
				}
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
	
	// Get connection from pool or create new one
	conn := p.getConnection()
	if conn == nil {
		p.logger.Error("failed to get connection for unsubscribe")
		return
	}
	defer p.returnConnection(conn)

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

func (p *WebSocketProxy) trackSubscriptionFromMessage(msgBytes []byte, connectionSubscriptions map[string]bool, pendingSubscriptionRequests map[interface{}]bool) {
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
		// Track the request ID so we can identify the subscription response
		if requestID, ok := msg["id"]; ok && requestID != nil {
			pendingSubscriptionRequests[requestID] = true
			p.logger.Debug("subscription request detected", "method", method, "id", requestID)
		}
	}

	// Check if this is an unsubscribe request
	if method == "eth_unsubscribe" || method == "shh_unsubscribe" || method == "net_unsubscribe" {
		if params, ok := msg["params"].([]interface{}); ok && len(params) > 0 {
			if subID, ok := params[0].(string); ok {
				// Remove from both connection-specific and global maps
				delete(connectionSubscriptions, subID)
				p.RemoveSubscription(subID)
			}
		}
	}
}

func (p *WebSocketProxy) trackSubscriptionResponse(msgBytes []byte, connectionSubscriptions map[string]bool, pendingSubscriptionRequests map[interface{}]bool) {
	// Parse JSON-RPC message
	var msg map[string]interface{}
	if err := json.Unmarshal(msgBytes, &msg); err != nil {
		return // Not a valid JSON message
	}

	// Only track subscription IDs if this response corresponds to a pending subscription request
	requestID, hasID := msg["id"]
	if !hasID || !pendingSubscriptionRequests[requestID] {
		return // Not a subscription response
	}

	// Remove from pending requests
	delete(pendingSubscriptionRequests, requestID)

	// Check if this is a response with a result (subscription ID)
	if result, ok := msg["result"]; ok {
		if subID, ok := result.(string); ok {
			// Track in both connection-specific and global maps
			connectionSubscriptions[subID] = true
			p.TrackSubscription(subID)
			p.logger.Debug("subscription ID tracked from response", "subID", subID)
		}
	}
}

// logJSONRPCError logs JSON-RPC error responses from upstream providers
func (p *WebSocketProxy) logJSONRPCError(msgBytes []byte) {
	var msg map[string]interface{}
	if err := json.Unmarshal(msgBytes, &msg); err != nil {
		return // Not a valid JSON message
	}

	// Check if this is a JSON-RPC error response
	if errObj, ok := msg["error"].(map[string]interface{}); ok {
		code, _ := errObj["code"].(float64)
		message, _ := errObj["message"].(string)
		
		p.logger.Debug("JSON-RPC error from upstream provider",
			"target_url", p.targetURL,
			"error_code", int(code),
			"error_message", message)
	}
}

// cleanupConnectionSubscriptions removes subscriptions for a specific connection
func (p *WebSocketProxy) cleanupConnectionSubscriptions(connectionSubscriptions map[string]bool) {
	if len(connectionSubscriptions) == 0 {
		return
	}
	
	p.logger.Debug("cleaning up connection subscriptions", "count", len(connectionSubscriptions))
	
	// Remove from global map
	p.mu.Lock()
	for subID := range connectionSubscriptions {
		delete(p.subscriptions, subID)
	}
	p.mu.Unlock()
	
	// Clear connection-specific map
	for subID := range connectionSubscriptions {
		delete(connectionSubscriptions, subID)
	}
}

// getConnection gets a connection from the pool or creates a new one
func (p *WebSocketProxy) getConnection() *websocket.Conn {
	// Try to get from pool first
	select {
	case conn := <-p.connPool:
		// Test if connection is still alive
		if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(pingTimeout)); err != nil {
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