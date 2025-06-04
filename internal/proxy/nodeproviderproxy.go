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
	proxy.Director = func(r *http.Request) {
		r.Host = target.Host
		r.URL.Scheme = target.Scheme
		r.URL.Host = target.Host
		// Keep the original path from the request
		// r.URL.Path = target.Path
	}

	// Add custom error handler to properly handle response body errors
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// Don't try to read from the response body if there's an error
		w.WriteHeader(http.StatusBadGateway)
	}

	return proxy, nil
}
