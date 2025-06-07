package proxy

import (
	"net/http"
	"strings"
	"time"

	"github.com/kewlfft/rpc-gateway/internal/middleware"
)

type NodeProvider struct {
	config  NodeProviderConfig
	proxy   http.Handler
	timeout time.Duration
}

func NewNodeProvider(config NodeProviderConfig, timeout time.Duration) (*NodeProvider, error) {
	proxy, err := newNodeProviderProxy(config.Connection.HTTP.URL, timeout)
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
