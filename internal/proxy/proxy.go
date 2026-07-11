package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kewlfft/rpc-gateway/internal/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
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

// RandomizeProviders randomizes the order of providers using a simple time-based shuffle to avoid math/rand
func (p *Proxy) RandomizeProviders() {
	n := len(p.targets)
	seed := uint64(time.Now().UnixNano())
	for i := n - 1; i > 0; i-- {
		seed = seed*6364136223846793005 + 1
		j := int(seed % uint64(i+1))
		p.targets[i], p.targets[j] = p.targets[j], p.targets[i]
	}
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

	// Create providers for each target with pre-allocated capacity
	proxy.targets = make([]*NodeProvider, 0, len(config.Targets))
	for _, target := range config.Targets {
		p := NewNodeProvider(target, config.Timeout, config.Logger)
		// Link health checkers directly to the node provider for high performance health checks
		p.SetHealthCheckers(
			hcm.GetHealthChecker(target.Name, "http"),
			hcm.GetHealthChecker(target.Name, "websocket"),
		)
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

	// Health check manager will be started by RPCGateway.Start()
	// Don't start it here to avoid duplicate starts

	return proxy, nil
}

// HasNodeProviderFailed checks if a provider has failed based on status code
func (p *Proxy) HasNodeProviderFailed(statusCode int) bool {
	// Consider any non-2xx status code as a failure
	return statusCode < 200 || statusCode >= 300
}

// writeErrorResponse writes an error response in the appropriate format based on the request
// bodyBytes is optional - if provided, it will be used to extract the request ID
// providerDetails is optional - if provided, it will be included in the error log for diagnostics
func (p *Proxy) writeErrorResponse(w http.ResponseWriter, r *http.Request, message string, status int, bodyBytes []byte, providerDetails ...string) {
	errors.WriteJSONRPCError(w, r, message, status, bodyBytes, providerDetails...)
}

// copyResponse copies headers, status code, and body from the source response to the target response writer
func (p *Proxy) copyResponse(w http.ResponseWriter, resp *http.Response) error {
	// Check if headers have already been written to avoid "superfluous response.WriteHeader call"
	if w.Header().Get("X-Status-Set") == "" {
		// Copy headers only if they haven't been written yet
		maps.Copy(w.Header(), resp.Header)
		w.Header().Set("X-Status-Set", "true")
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
func (p *Proxy) handleProviderFailure(target *NodeProvider, r *http.Request, start time.Time, statusCode int, err error) {
	name := target.Name()
	duration := p.recordMetrics(r.Method, name, "error", start)
	metricRequestErrors.WithLabelValues(r.Method, name, "rerouted").Inc()

	connectionType := p.getConnectionType(r)

	var hc *HealthChecker
	if connectionType == "websocket" {
		hc = target.wsChecker
	} else {
		hc = target.httpChecker
	}

	if hc != nil {
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
// Returns: true if successful, false if failed and retryable, -1 if failed and not retryable (client disconnect)
func (p *Proxy) forwardRequest(w http.ResponseWriter, r *http.Request, body []byte, start time.Time, target *NodeProvider, urlPath string) int {
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
		p.handleProviderFailure(target, r, start, http.StatusServiceUnavailable, err)
		return 0
	}

	// Copy headers from original request
	maps.Copy(req.Header, r.Header)

	// Add custom headers if configured
	for key, value := range target.config.Connection.HTTP.Headers {
		req.Header.Set(key, value)
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
		p.handleProviderFailure(target, r, start, http.StatusServiceUnavailable, err)
		return 0
	}
	defer resp.Body.Close()

	// Check for non-2xx status codes
	if p.HasNodeProviderFailed(resp.StatusCode) {
		p.logger.Error("Provider returned error status",
			"provider", name,
			"status", resp.StatusCode,
			"method", r.Method,
			"url", urlPath)
		p.handleProviderFailure(target, r, start, resp.StatusCode, nil)
		return 0
	}

	if err := p.copyResponse(w, resp); err != nil {
		// Check if this is a broken pipe error (client disconnected)
		if isBrokenPipeError(err) {
			p.logger.Debug("Client disconnected during response streaming",
				"error", err,
				"url", urlPath,
				"method", r.Method)
			// Don't taint provider for client disconnection and don't retry
			return -1
		}

		p.logger.Error("Failed to copy response",
			"error", err,
			"url", urlPath,
			"method", r.Method)
		p.handleProviderFailure(target, r, start, resp.StatusCode, err)
		return 0
	}

	p.logSuccessfulRequest(r, name, resp.StatusCode, start)
	return 1
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
			p.writeErrorResponse(w, r, "Failed to read request body", http.StatusBadRequest, nil)
			return
		}
		// Restore body for potential error handling
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}

	// Pre-compute connection type and get healthy providers (using cache)
	connType := p.getConnectionType(r)
	healthyProviders := p.getHealthyProviders(connType)

	if len(healthyProviders) == 0 {
		providerDetails := p.collectProviderDetails(connType)
		p.writeErrorResponse(w, r, fmt.Sprintf("No healthy providers available for chain %s", p.hcm.path), http.StatusServiceUnavailable, bodyBytes, providerDetails...)
		return
	}

	for _, target := range healthyProviders {
		name := target.Name()

		if isWebSocket {
			wsProxy := target.GetWebSocketProxy()
			if wsProxy == nil {
				continue
			}

			// Create upstream connection BEFORE upgrading (upgrade commits HTTP response)
			// If this fails, we can try next provider without committing to the client
			if targetConn := wsProxy.createNewConnection(); targetConn == nil {
				p.logger.Debug("WebSocket connection failed",
					"provider", name)
				if hc := p.hcm.GetHealthChecker(name, connType); hc != nil {
					hc.TaintHTTP()
				}
				p.logger.Debug("trying next WebSocket provider",
					"failed_provider", name,
					"total_providers", len(healthyProviders))
				continue // Try next provider
			} else {
				// Close test connection - real one will be created in ServeHTTP
				targetConn.Close()
			}

			// Connection successful - safe to upgrade
			target.ServeHTTP(w, r)
			return
		}

		url := target.config.Connection.HTTP.URL
		if p.chainType == "tron" {
			url += r.URL.Path
		}

		result := p.forwardRequest(w, r, bodyBytes, start, target, url)
		if result == 1 {
			// Success
			return
		} else if result == -1 {
			// Client disconnected - don't retry
			return
		}

		// If the request failed but we already sent headers to the client,
		// we MUST NOT retry because the stream is already "committed" and corrupted.
		if w.Header().Get("X-Status-Set") != "" {
			p.logger.Warn("Provider failed during streaming, cannot retry to avoid response corruption",
				"provider", name,
				"path", p.hcm.path)
			return
		}
	}

	providerDetails := p.collectProviderDetails(connType)
	p.writeErrorResponse(w, r, fmt.Sprintf("All providers failed for chain %s", p.hcm.path), http.StatusServiceUnavailable, bodyBytes, providerDetails...)
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
		if target.IsHealthy(connType) {
			healthy = append(healthy, target)
		}
	}
	return healthy
}

// collectProviderDetails collects diagnostic information about all providers
func (p *Proxy) collectProviderDetails(connType string) []string {
	providerDetails := make([]string, len(p.targets))
	for i, target := range p.targets {
		name := target.Name()
		var checker *HealthChecker
		if connType == "websocket" {
			checker = target.wsChecker
		} else {
			checker = target.httpChecker
		}

		var status string
		if checker == nil {
			status = name + ":no_checker"
		} else if checker.IsTainted() {
			status = name + ":tainted"
		} else {
			status = name + ":unknown"
		}
		providerDetails[i] = status
	}
	return providerDetails
}
