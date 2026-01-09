package proxy

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// HTTPClientSettings defines configuration for HTTP clients
type HTTPClientSettings struct {
	// Timeout for requests
	Timeout time.Duration
	// Maximum number of idle connections in the pool
	MaxIdleConns int
	// Maximum number of idle connections per host
	MaxIdleConnsPerHost int
	// How long to keep idle connections alive
	IdleConnTimeout time.Duration
	// Timeout for response headers
	ResponseHeaderTimeout time.Duration
	// Whether to disable compression
	DisableCompression bool
	// Whether to force HTTP/2
	ForceAttemptHTTP2 bool
}

// DefaultHTTPClientConfig returns optimized default configuration for both user requests and health checks
func DefaultHTTPClientConfig(timeout time.Duration) HTTPClientSettings {
	return HTTPClientSettings{
		Timeout:              timeout,
		MaxIdleConns:         512,
		MaxIdleConnsPerHost:  64,
		IdleConnTimeout:      90 * time.Second,
		ResponseHeaderTimeout: timeout,
		DisableCompression:   true, // Prevent auto-decompression so we can forward gzip as-is
		ForceAttemptHTTP2:    true, // Enable HTTP/2 for better multiplexing
	}
}

// HealthCheckHTTPClientConfig is now a wrapper around DefaultHTTPClientConfig to unify pools
func HealthCheckHTTPClientConfig(timeout time.Duration) HTTPClientSettings {
	return DefaultHTTPClientConfig(timeout)
}

// HTTPClientManager manages shared HTTP clients with connection pooling
type HTTPClientManager struct {
	mu       sync.RWMutex
	clients  map[string]*http.Client
	configs  map[string]HTTPClientSettings
	transports map[string]*http.Transport
}

// NewHTTPClientManager creates a new HTTP client manager
func NewHTTPClientManager() *HTTPClientManager {
	return &HTTPClientManager{
		clients:    make(map[string]*http.Client),
		configs:    make(map[string]HTTPClientSettings),
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

	// Create shared transport for this configuration
	transport := &http.Transport{
		MaxIdleConns:          config.MaxIdleConns,
		MaxIdleConnsPerHost:   config.MaxIdleConnsPerHost,
		IdleConnTimeout:       config.IdleConnTimeout,
		ResponseHeaderTimeout: config.ResponseHeaderTimeout,
		DisableCompression:    config.DisableCompression,
		ForceAttemptHTTP2:     config.ForceAttemptHTTP2,
	}

	client := &http.Client{
		Timeout:   config.Timeout,
		Transport: transport,
	}

	m.clients[key] = client
	m.configs[key] = config
	m.transports[key] = transport

	return client
}

// GetClient gets an existing client by key
func (m *HTTPClientManager) GetClient(key string) (*http.Client, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	client, exists := m.clients[key]
	return client, exists
}

// Close closes all managed clients and their transports
func (m *HTTPClientManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, transport := range m.transports {
		transport.CloseIdleConnections()
	}
	
	// Clear all maps
	m.clients = make(map[string]*http.Client)
	m.configs = make(map[string]HTTPClientSettings)
	m.transports = make(map[string]*http.Transport)
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
	f.managers = make(map[string]*HTTPClientManager)
}

// CreateOptimizedHTTPClient creates an optimized HTTP client for a specific proxy
func (f *HTTPClientFactory) CreateOptimizedHTTPClient(proxyPath string, timeout time.Duration) *http.Client {
	manager := f.GetManagerForProxy(proxyPath)
	config := DefaultHTTPClientConfig(timeout)
	// Use a unified key to share the connection pool between health checks and user requests
	key := "unified-" + proxyPath
	return manager.GetOrCreateClient(key, config)
}

// CreateHealthCheckHTTPClient creates an optimized HTTP client for health checks
func (f *HTTPClientFactory) CreateHealthCheckHTTPClient(proxyPath string, timeout time.Duration) *http.Client {
	// Re-use CreateOptimizedHTTPClient to share the connection pool
	return f.CreateOptimizedHTTPClient(proxyPath, timeout)
}

// Global factory instance (can be replaced for testing)
var globalFactory = NewHTTPClientFactory()

// GetGlobalFactory returns the global HTTP client factory
func GetGlobalFactory() *HTTPClientFactory {
	return globalFactory
}

// CreateOptimizedHTTPClient creates an optimized HTTP client using the global factory
func CreateOptimizedHTTPClient(key string, timeout time.Duration) *http.Client {
	// Extract proxy path from key (e.g., "proxy-eth" -> "eth")
	proxyPath := key
	if strings.HasPrefix(key, "proxy-") {
		proxyPath = strings.TrimPrefix(key, "proxy-")
	}
	return globalFactory.CreateOptimizedHTTPClient(proxyPath, timeout)
}


// CreateHealthCheckHTTPClientForProxy creates a shared health check client for all providers in a proxy path
func CreateHealthCheckHTTPClientForProxy(proxyPath string, providerName string, timeout time.Duration) *http.Client {
	// Use CreateOptimizedHTTPClient to ensure unified connection pool with user requests
	return globalFactory.CreateOptimizedHTTPClient(proxyPath, timeout)
}
