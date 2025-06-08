package proxy

import (
	"net/http"
	"time"
	"log/slog"
	"github.com/gorilla/websocket"
)

type NodeProvider struct {
	config  NodeProviderConfig
	proxy   http.Handler
	wsProxy http.Handler
}

func NewNodeProvider(config NodeProviderConfig, timeout time.Duration, logger *slog.Logger) (*NodeProvider, error) {
	proxy, err := newNodeProviderProxy(config.Connection.HTTP.URL, timeout)
	if err != nil {
		return nil, err
	}

	var wsProxy http.Handler
	if config.Connection.WebSocket.URL != "" {
		wsProxy = NewWebSocketProxy(config.Connection.WebSocket.URL, logger)
	}

	return &NodeProvider{
		config:  config,
		proxy:   proxy,
		wsProxy: wsProxy,
	}, nil
}

func (n *NodeProvider) Name() string {
	return n.config.Name
}

func (n *NodeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if websocket.IsWebSocketUpgrade(r) {
		if n.wsProxy == nil {
			http.Error(w, "WebSocket not supported", http.StatusBadRequest)
			return
		}
		n.wsProxy.ServeHTTP(w, r)
		return
	}
	n.proxy.ServeHTTP(w, r)
}


