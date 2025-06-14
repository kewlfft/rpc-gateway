package proxy

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

func newNodeProviderProxy(rawurl string, timeout time.Duration) (http.Handler, error) {
	target, err := url.Parse(rawurl)
	if err != nil {
		return nil, err
	}

	// High-performance transport config
	transport := &http.Transport{
		MaxIdleConns:          1024,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: timeout,
		DisableCompression:    true, // Disable if your RPC traffic is already fast or uncompressed
		ForceAttemptHTTP2:     true,
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport

	return proxy, nil
}
