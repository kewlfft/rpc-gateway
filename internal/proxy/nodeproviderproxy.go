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

	proxy := httputil.NewSingleHostReverseProxy(target)

	proxy.Transport = &http.Transport{
		ResponseHeaderTimeout: timeout,
	}

	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = target.Host
	}

	return proxy, nil
}
