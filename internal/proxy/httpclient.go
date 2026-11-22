package proxy

import (
	"crypto/tls"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Shared TLS session cache for faster reconnects to HTTPS upstream RPC hosts.
var tlsSessionCache = tls.NewLRUClientSessionCache(128)

var (
	httpClientsMu sync.Mutex
	httpClients   = make(map[string]*http.Client)
)

// CreateOptimizedHTTPClient returns the shared HTTP client for a proxy path, creating it
// on first use. User requests and health checks for the same path share one connection
// pool. A "proxy-" key prefix is ignored; the timeout of the first call for a key wins.
func CreateOptimizedHTTPClient(key string, timeout time.Duration) *http.Client {
	key = strings.TrimPrefix(key, "proxy-")

	httpClientsMu.Lock()
	defer httpClientsMu.Unlock()

	if client, ok := httpClients[key]; ok {
		return client
	}

	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			MaxIdleConns:          512,
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: timeout,
			DisableCompression:    true, // forward gzip as-is instead of auto-decompressing
			ForceAttemptHTTP2:     true,
			TLSClientConfig: &tls.Config{
				ClientSessionCache: tlsSessionCache,
			},
		},
	}
	httpClients[key] = client
	return client
}
