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

// HTTPClientSettings defines configuration for HTTP clients
type HTTPClientSettings struct {
	Timeout               time.Duration
	MaxIdleConns          int
	MaxIdleConnsPerHost   int
	IdleConnTimeout       time.Duration
	ResponseHeaderTimeout time.Duration
	DisableCompression    bool
	ForceAttemptHTTP2     bool
}

// DefaultHTTPClientConfig returns optimized default configuration for both user requests and health checks
func DefaultHTTPClientConfig(timeout time.Duration) HTTPClientSettings {
	return HTTPClientSettings{
		Timeout:               timeout,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: timeout,
		DisableCompression:    true, // Prevent auto-decompression so we can forward gzip as-is
		ForceAttemptHTTP2:     true, // Enable HTTP/2 for better multiplexing
	}
}

// HTTPClientManager manages shared HTTP clients with connection pooling
type HTTPClientManager struct {
	mu         sync.RWMutex
	clients    map[string]*http.Client
	transports map[string]*http.Transport
}

// NewHTTPClientManager creates a new HTTP client manager
func NewHTTPClientManager() *HTTPClientManager {
	return &HTTPClientManager{
		clients:    make(map[string]*http.Client),
		transports: make(map[string]*http.Transport),
	}
}

// GetOrCreateClient gets an existing client or creates a new one with the given config
func (m *HTTPClientManager) GetOrCreateClient(key string, config HTTPClientSettings) *http.Client {
	m.mu.Lock()
	defer m.mu.Unlock()

	if client, exists := m.clients[key]; exists {
		return client
	}

	transport := &http.Transport{
		MaxIdleConns:          config.MaxIdleConns,
		MaxIdleConnsPerHost:   config.MaxIdleConnsPerHost,
		IdleConnTimeout:       config.IdleConnTimeout,
		ResponseHeaderTimeout: config.ResponseHeaderTimeout,
		DisableCompression:    config.DisableCompression,
		ForceAttemptHTTP2:     config.ForceAttemptHTTP2,
		TLSClientConfig: &tls.Config{
			ClientSessionCache: tlsSessionCache,
		},
	}

	client := &http.Client{
		Timeout:   config.Timeout,
		Transport: transport,
	}

	m.clients[key] = client
	m.transports[key] = transport
	return client
}

// Close closes all managed clients and their transports
func (m *HTTPClientManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, transport := range m.transports {
		transport.CloseIdleConnections()
	}
	clear(m.clients)
	clear(m.transports)
}

// HTTPClientFactory creates optimized HTTP clients with proper isolation
type HTTPClientFactory struct {
	managers map[string]*HTTPClientManager
	mu       sync.RWMutex
}

// NewHTTPClientFactory creates a new HTTP client factory
func NewHTTPClientFactory() *HTTPClientFactory {
	return &HTTPClientFactory{
		managers: make(map[string]*HTTPClientManager),
	}
}

// GetManagerForProxy gets or creates a manager for a specific proxy
func (f *HTTPClientFactory) GetManagerForProxy(proxyPath string) *HTTPClientManager {
	f.mu.Lock()
	defer f.mu.Unlock()

	if manager, exists := f.managers[proxyPath]; exists {
		return manager
	}

	manager := NewHTTPClientManager()
	f.managers[proxyPath] = manager
	return manager
}

// Close closes all managers
func (f *HTTPClientFactory) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, manager := range f.managers {
		manager.Close()
	}
	clear(f.managers)
}

// CreateOptimizedHTTPClient creates an optimized HTTP client for a specific proxy
func (f *HTTPClientFactory) CreateOptimizedHTTPClient(proxyPath string, timeout time.Duration) *http.Client {
	manager := f.GetManagerForProxy(proxyPath)
	config := DefaultHTTPClientConfig(timeout)
	key := "unified-" + proxyPath
	return manager.GetOrCreateClient(key, config)
}

var globalFactory = NewHTTPClientFactory()

// CreateOptimizedHTTPClient creates an optimized HTTP client using the global factory
func CreateOptimizedHTTPClient(key string, timeout time.Duration) *http.Client {
	proxyPath := key
	if after, ok := strings.CutPrefix(key, "proxy-"); ok {
		proxyPath = after
	}
	return globalFactory.CreateOptimizedHTTPClient(proxyPath, timeout)
}
