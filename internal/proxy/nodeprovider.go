package proxy

import (
	"github.com/gorilla/websocket"
	"log/slog"
	"net/http"
	"time"
)

type NodeProvider struct {
	config      NodeProviderConfig
	wsProxy     http.Handler
	httpChecker *HealthChecker
	wsChecker   *HealthChecker
}

func NewNodeProvider(config NodeProviderConfig, timeout time.Duration, logger *slog.Logger) *NodeProvider {
	var wsProxy http.Handler
	if config.Connection.WebSocket.URL != "" {
		wsProxy = NewWebSocketProxy(config.Connection.WebSocket.URL, timeout, logger)
	}

	return &NodeProvider{
		config:  config,
		wsProxy: wsProxy,
	}
}

// SetHealthCheckers sets the health checkers for the provider
func (n *NodeProvider) SetHealthCheckers(httpChecker, wsChecker *HealthChecker) {
	n.httpChecker = httpChecker
	n.wsChecker = wsChecker
}

// HealthChecker returns the health checker for the given connection type.
func (n *NodeProvider) HealthChecker(connType string) *HealthChecker {
	if connType == "websocket" {
		return n.wsChecker
	}
	return n.httpChecker
}

// IsHealthy returns true if the provider is healthy for the given connection type
func (n *NodeProvider) IsHealthy(connType string) bool {
	hc := n.HealthChecker(connType)
	return hc != nil && hc.IsHealthy()
}

func (n *NodeProvider) Name() string {
	return n.config.Name
}

func (n *NodeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if websocket.IsWebSocketUpgrade(r) {
		if n.wsProxy != nil {
			n.wsProxy.ServeHTTP(w, r)
		} else {
			http.Error(w, "WebSocket not supported", http.StatusBadRequest)
		}
		return
	}
	http.Error(w, "HTTP requests handled by main proxy", http.StatusInternalServerError)
}

func (n *NodeProvider) GetWebSocketProxy() *WebSocketProxy {
	if n.wsProxy == nil {
		return nil
	}
	return n.wsProxy.(*WebSocketProxy)
}
