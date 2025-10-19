package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"time"

	"github.com/kewlfft/rpc-gateway/internal/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/gorilla/websocket"
)

var (
	metricRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "rpc_request_duration_seconds",
			Help:    "Duration of RPC requests in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "provider", "status"},
	)
	metricRequestErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rpc_requests_total",
			Help: "Total number of RPC requests",
		},
		[]string{"method", "provider", "status"},
	)
)

// ChainTypeHandler is an interface that extends http.Handler with chain type information
type ChainTypeHandler interface {
	http.Handler
	GetChainType() string
	RandomizeProviders()
	GetHealthCheckManager() *HealthCheckManager
}

// Proxy represents an RPC proxy with health checking and failover
type Proxy struct {
	hcm       *HealthCheckManager
	timeout   time.Duration
	logger    *slog.Logger
	targets   []*NodeProvider
	chainType string
	client    *http.Client
}

// Ensure Proxy implements ChainTypeHandler
var _ ChainTypeHandler = (*Proxy)(nil)

// RandomizeProviders randomizes the order of providers in the targets slice
func (p *Proxy) RandomizeProviders() {
	rand.Shuffle(len(p.targets), func(i, j int) {
		p.targets[i], p.targets[j] = p.targets[j], p.targets[i]
	})
}

// NewProxy creates a new proxy
func NewProxy(ctx context.Context, config Config) (*Proxy, error) {
	// Create health check manager
	hcm, err := NewHealthCheckManager(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create health check manager: %w", err)
	}

	// Create optimized HTTP client using shared connection pool with default settings
	clientKey := "proxy-" + config.Path
	client := CreateOptimizedHTTPClient(clientKey, config.Timeout)

	proxy := &Proxy{
		hcm:       hcm,
		timeout:   config.Timeout,
		logger:    config.Logger,
		chainType: config.ChainType,
		client:    client,
	}

	// Create providers for each target
	for _, target := range config.Targets {
		p := NewNodeProvider(target, config.Timeout, config.Logger)
		proxy.targets = append(proxy.targets, p)
	}

	// Wire up WebSocket proxy references to health checkers
	wsProxies := make(map[string]*WebSocketProxy)
	for _, target := range proxy.targets {
		if wsProxy := target.GetWebSocketProxy(); wsProxy != nil {
			wsProxies[target.Name()] = wsProxy
		}
	}
	
	if len(wsProxies) > 0 {
		hcm.SetWebSocketProxyReferences(wsProxies)
	}

	// Start health check manager if not disabled
	if !config.DisableHealthChecks {
		if err := hcm.Start(ctx); err != nil {
			return nil, fmt.Errorf("failed to start health check manager: %w", err)
		}
		config.Logger.Info("health check manager started")
	}

	return proxy, nil
}

// HasNodeProviderFailed checks if a provider has failed based on status code
func (p *Proxy) HasNodeProviderFailed(statusCode int) bool {
	// Consider any non-2xx status code as a failure
	return statusCode < 200 || statusCode >= 300
}

// writeErrorResponse writes an error response in the appropriate format based on the request
func (p *Proxy) writeErrorResponse(w http.ResponseWriter, r *http.Request, message string, status int) {
	errors.WriteJSONRPCError(w, r, message, status)
}

// copyResponse copies headers, status code, and body from the source response to the target response writer
func (p *Proxy) copyResponse(w http.ResponseWriter, resp *http.Response) error {
	// Check if headers have already been written to avoid "superfluous response.WriteHeader call"
	if w.Header().Get("Content-Type") == "" {
		// Copy headers only if they haven't been written yet
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
	}

	// Stream the response body directly
	if _, err := io.Copy(w, resp.Body); err != nil {
		// Check if this is a broken pipe error (client disconnected)
		if isBrokenPipeError(err) {
			// Don't treat broken pipe as a provider failure - it's a client disconnection
			return fmt.Errorf("client disconnected: %w", err)
		}
		return fmt.Errorf("failed to stream response: %w", err)
	}

	return nil
}

// isBrokenPipeError checks if the error is a broken pipe error (client disconnected)
// isBrokenPipeError is defined in healthchecker_utils.go

// getConnectionType determines the connection type based on the request
func (p *Proxy) getConnectionType(r *http.Request) string {
	if websocket.IsWebSocketUpgrade(r) {
		return "websocket"
	}
	return "http"
}

// recordMetrics records metrics for a request
func (p *Proxy) recordMetrics(method, name, status string, start time.Time) int64 {
	duration := time.Since(start).Milliseconds()
	metricRequestDuration.WithLabelValues(method, name, status).Observe(float64(duration) / 1000)
	metricRequestErrors.WithLabelValues(method, name, status).Inc()
	return duration
}

// handleProviderFailure handles provider failure by recording metrics, tainting the provider, and logging
func (p *Proxy) handleProviderFailure(name string, r *http.Request, start time.Time, statusCode int, err error) {
	duration := p.recordMetrics(r.Method, name, "error", start)
	metricRequestErrors.WithLabelValues(r.Method, name, "rerouted").Inc()

	connectionType := p.getConnectionType(r)

	if hc := p.hcm.GetHealthChecker(name, connectionType); hc != nil {
		hc.TaintHTTP()
	}

	p.logger.Debug("provider failed, trying next",
		"provider", name,
		"status", statusCode,
		"error", err,
		"method", r.Method,
		"upstream_path", r.URL.Path,
		"path", p.hcm.path,
		"duration_ms", duration,
		"connectionType", connectionType,
	)
}

func (p *Proxy) logSuccessfulRequest(r *http.Request, name string, status int, start time.Time) {
	duration := p.recordMetrics(r.Method, name, "success", start)

	p.logger.Debug("request handled",
		"provider", name,
		"status", status,
		"method", r.Method,
		"path", p.hcm.path,
		"duration_ms", duration,
	)
}

// forwardRequest handles both standard and Tron requests with direct streaming
func (p *Proxy) forwardRequest(w http.ResponseWriter, r *http.Request, body []byte, start time.Time, target *NodeProvider, urlPath string) bool {
	name := target.Name()

	// Create a new context with timeout for this specific request to avoid context cancellation cascade
	// This prevents all providers from failing when the original request context is cancelled
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	// Create request with proper URL using the new context
	req, err := http.NewRequestWithContext(ctx, r.Method, urlPath, bytes.NewReader(body))
	if err != nil {
		p.logger.Error("Failed to create request",
			"error", err,
			"method", r.Method,
			"path", r.URL.Path,
			"provider_url", urlPath)
		p.handleProviderFailure(name, r, start, http.StatusServiceUnavailable, err)
		return false
	}

	// Copy headers from original request
	for k, v := range r.Header {
		req.Header[k] = v
	}

	// Add API key header if configured
	if apiKey := target.config.Connection.HTTP.APIKey; apiKey != "" {
		req.Header.Set("TRON-PRO-API-KEY", apiKey)
	}

	// Add query parameters to the request
	if r.URL.RawQuery != "" {
		req.URL.RawQuery = r.URL.RawQuery
	}

	// Use direct HTTP client call for minimal latency
	resp, err := p.client.Do(req)
	if err != nil {
		// Check if this is a context cancellation error
		if ctx.Err() == context.Canceled || ctx.Err() == context.DeadlineExceeded {
			p.logger.Warn("Request cancelled or timed out",
				"error", err,
				"url", urlPath,
				"method", r.Method,
				"context_error", ctx.Err())
		} else {
			p.logger.Error("Request failed",
				"error", err,
				"url", urlPath,
				"method", r.Method)
		}
		p.handleProviderFailure(name, r, start, http.StatusServiceUnavailable, err)
		return false
	}
	defer resp.Body.Close()

	// Check for non-2xx status codes
	if p.HasNodeProviderFailed(resp.StatusCode) {
		p.logger.Error("Provider returned error status",
			"provider", name,
			"status", resp.StatusCode,
			"method", r.Method,
			"url", urlPath)
		p.handleProviderFailure(name, r, start, resp.StatusCode, nil)
		return false
	}

	if err := p.copyResponse(w, resp); err != nil {
		// Check if this is a broken pipe error (client disconnected)
		if isBrokenPipeError(err) {
			p.logger.Debug("Client disconnected during response streaming",
				"error", err,
				"url", urlPath,
				"method", r.Method)
			// Don't taint provider for client disconnection
			return false
		}
		
		p.logger.Error("Failed to copy response",
			"error", err,
			"url", urlPath,
			"method", r.Method)
		p.handleProviderFailure(name, r, start, resp.StatusCode, err)
		return false
	}

	p.logSuccessfulRequest(r, name, resp.StatusCode, start)
	return true
}

// Update ServeHTTP to use the unified forwardRequest
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	isWebSocket := websocket.IsWebSocketUpgrade(r)

	var bodyBytes []byte
	if !isWebSocket {
		var err error
		bodyBytes, err = io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			p.logger.Error("failed to read request body",
				"error", err,
				"method", r.Method,
				"path", r.URL.Path)
			p.writeErrorResponse(w, r, "Failed to read request body", http.StatusBadRequest)
			return
		}
	}

	// Pre-compute connection type and get healthy providers
	connType := p.getConnectionType(r)
	healthyProviders := p.getHealthyProviders(connType)
	
	if len(healthyProviders) == 0 {
		p.writeErrorResponse(w, r, "No healthy providers available", http.StatusServiceUnavailable)
		return
	}

	// Track failed providers to avoid retrying the same provider
	failedProviders := make(map[string]bool, len(healthyProviders))
	maxRetries := 2 // Allow up to 2 retries for context cancellation
	
	for attempt := 0; attempt <= maxRetries; attempt++ {
		for _, target := range healthyProviders {
			name := target.Name()
			
			// Skip if provider already failed or became unhealthy
			if failedProviders[name] || !p.hcm.IsHealthy(name, connType) {
				continue
			}

			if isWebSocket {
				target.ServeHTTP(w, r)
				return
			}

			url := target.config.Connection.HTTP.URL
			if p.chainType == "tron" {
				url += r.URL.Path
			}

			if p.forwardRequest(w, r, bodyBytes, start, target, url) {
				return
			}
			
			// Mark this provider as failed for this request
			failedProviders[name] = true
		}
		
		// If we've tried all providers and this is not the last attempt, wait briefly before retry
		if attempt < maxRetries && len(failedProviders) < len(healthyProviders) {
			time.Sleep(time.Millisecond * 50) // Reduced delay
		}
	}

	p.writeErrorResponse(w, r, "All providers failed", http.StatusServiceUnavailable)
}

// GetHealthCheckManager returns the health check manager for this proxy
func (p *Proxy) GetHealthCheckManager() *HealthCheckManager {
	return p.hcm
}

// GetChainType returns the chain type of the proxy
func (p *Proxy) GetChainType() string {
	return p.chainType
}

// GetTargets returns a copy of the targets slice
func (p *Proxy) GetTargets() []*NodeProvider {
	return p.targets
}

// getHealthyProviders returns only healthy providers for the given connection type
func (p *Proxy) getHealthyProviders(connType string) []*NodeProvider {
	healthy := make([]*NodeProvider, 0, len(p.targets))
	for _, target := range p.targets {
		if p.hcm.IsHealthy(target.Name(), connType) {
			healthy = append(healthy, target)
		}
	}
	return healthy
}

