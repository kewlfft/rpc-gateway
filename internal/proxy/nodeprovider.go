package proxy

import (
	"net/http"
	"time"
)

type NodeProvider struct {
	config  NodeProviderConfig
	proxy   http.Handler
}

func NewNodeProvider(config NodeProviderConfig, timeout time.Duration) (*NodeProvider, error) {
	proxy, err := newNodeProviderProxy(config.Connection.HTTP.URL, timeout)
	if err != nil {
		return nil, err
	}

	return &NodeProvider{
		config:  config,
		proxy:   proxy,
	}, nil
}

func (n *NodeProvider) Name() string {
	return n.config.Name
}

func (n *NodeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n.proxy.ServeHTTP(w, r)
}


