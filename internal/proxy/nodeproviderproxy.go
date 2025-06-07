package proxy

import (
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/kewlfft/rpc-gateway/internal/errors"
	pkgerrors "github.com/pkg/errors"
)

// sharedTransport is a shared HTTP transport with connection pooling settings
var sharedTransport = &http.Transport{
	// Connection pooling settings
	MaxIdleConns:        100,              // Maximum number of idle connections
	MaxIdleConnsPerHost: 10,               // Maximum number of idle connections per host
	IdleConnTimeout:     90 * time.Second, // How long to keep idle connections
	// TCP settings
	DialContext: (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	// TLS settings
	TLSHandshakeTimeout: 10 * time.Second,
	// Other optimizations
	ForceAttemptHTTP2:     true,
	MaxConnsPerHost:       100,
	ResponseHeaderTimeout: 10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	// Disable compression as we handle it ourselves
	DisableCompression: true,
}

func NewNodeProviderProxy(config NodeProviderConfig) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(config.Connection.HTTP.URL)
	if err != nil {
		return nil, pkgerrors.Wrap(err, "cannot parse url")
	}

	proxy := httputil.NewSingleHostReverseProxy(target)

	// Set up the request for the upstream server
	proxy.Director = func(r *http.Request) {
		r.Host = target.Host
		r.URL.Scheme = target.Scheme
		r.URL.Host = target.Host
	}

	// Add custom error handler to properly handle response body errors
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		errors.WriteJSONRPCError(w, r, "Bad Gateway", http.StatusBadGateway)
	}

	// Use the shared transport with connection pooling
	proxy.Transport = sharedTransport

	return proxy, nil
}
