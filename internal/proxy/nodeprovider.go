package proxy

import (
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/kewlfft/rpc-gateway/internal/middleware"
)

// NodeProvider represents a single RPC provider
type NodeProvider struct {
	config  NodeProviderConfig
	proxy   *httputil.ReverseProxy
	timeout time.Duration
}

// NewNodeProvider creates a new node provider
func NewNodeProvider(config NodeProviderConfig, timeout time.Duration) (*NodeProvider, error) {
	proxy, err := NewNodeProviderProxy(config, timeout)
	if err != nil {
		return nil, err
	}

	return &NodeProvider{
		config:  config,
		proxy:   proxy,
		timeout: timeout,
	}, nil
}

func (n *NodeProvider) Name() string {
	return n.config.Name
}

func (n *NodeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	gzip := strings.Contains(r.Header.Get("Content-Encoding"), "gzip")

	if !n.config.Connection.HTTP.Compression && gzip {
		middleware.Gunzip(n.proxy).ServeHTTP(w, r)
		return
	}

	n.proxy.ServeHTTP(w, r)
}
