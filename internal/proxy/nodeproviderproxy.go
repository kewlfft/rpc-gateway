package proxy

import (
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/pkg/errors"
)

func NewNodeProviderProxy(config NodeProviderConfig) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(config.Connection.HTTP.URL)
	if err != nil {
		return nil, errors.Wrap(err, "cannot parse url")
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
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
	}

	// Prevent automatic decompression of gzipped responses
	proxy.Transport = &http.Transport{
		DisableCompression: true,
	}

	return proxy, nil
}
