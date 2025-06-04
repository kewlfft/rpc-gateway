package proxy

import (
	"net/http"
	"net/http/httputil"
	"strings"

	"github.com/kewlfft/rpc-gateway/internal/middleware"
	"github.com/go-http-utils/headers"
)

type NodeProvider struct {
	Config NodeProviderConfig
	Proxy  *httputil.ReverseProxy
}

func NewNodeProvider(config NodeProviderConfig) (*NodeProvider, error) {
	proxy, err := NewNodeProviderProxy(config)
	if err != nil {
		return nil, err
	}

	nodeProvider := &NodeProvider{
		Config: config,
		Proxy:  proxy,
	}

	return nodeProvider, nil
}

func (n *NodeProvider) Name() string {
	return n.Config.Name
}

func (n *NodeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	gzip := strings.Contains(r.Header.Get(headers.ContentEncoding), "gzip")

	if !n.Config.Connection.HTTP.Compression && gzip {
		middleware.Gunzip(n.Proxy).ServeHTTP(w, r)
		return
	}

	n.Proxy.ServeHTTP(w, r)
}
