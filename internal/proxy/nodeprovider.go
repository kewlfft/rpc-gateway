package proxy

import (
	"net/http"
	"time"
	"log/slog"
	"github.com/gorilla/websocket"
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

// IsHealthy returns true if the provider is healthy for the given connection type
func (n *NodeProvider) IsHealthy(connType string) bool {
	if connType == "websocket" {
		return n.wsChecker != nil && n.wsChecker.IsHealthy()
	}
	return n.httpChecker != nil && n.httpChecker.IsHealthy()
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
