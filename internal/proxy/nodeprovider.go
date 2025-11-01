package proxy

import (
	"net/http"
	"time"
	"log/slog"
	"github.com/gorilla/websocket"
)

type NodeProvider struct {
	config  NodeProviderConfig
	wsProxy http.Handler
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
