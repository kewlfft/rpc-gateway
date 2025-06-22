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

// Duration is a custom type that implements slog.LogValuer for better duration formatting
type Duration time.Duration

func (d Duration) LogValue() slog.Value {
	return slog.StringValue(time.Duration(d).String())
}

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

	proxy := &Proxy{
		hcm:       hcm,
		timeout:   config.Timeout,
		logger:    config.Logger,
		chainType: config.ChainType,
		client: &http.Client{
			Timeout: config.Timeout,
			Transport: &http.Transport{
				DisableCompression: true, // Prevent auto-decompression so we can forward gzip as-is
			},
		},
	}

	// Create providers for each target
	for _, target := range config.Targets {
		p, err := NewNodeProvider(target, config.Timeout, config.Logger)
		if err != nil {
			return nil, fmt.Errorf("failed to create provider for target %s: %w", target.Name, err)
		}
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
	// Copy headers
	for k, v := range resp.Header {
		w.Header()[k] = v
	}

	w.WriteHeader(resp.StatusCode)

	// Stream the response body directly
	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("failed to stream response: %w", err)
	}

	return nil
}

// handleProviderFailure handles provider failure by recording metrics, tainting the provider, and logging
func (p *Proxy) handleProviderFailure(name string, r *http.Request, start time.Time, statusCode int, err error) {
	durationMs := time.Since(start).Milliseconds()
	metricRequestDuration.WithLabelValues(r.Method, name, "error").Observe(float64(durationMs) / 1000)
	metricRequestErrors.WithLabelValues(r.Method, name, "error").Inc()
	metricRequestErrors.WithLabelValues(r.Method, name, "rerouted").Inc()

	connectionType := "http"
	if websocket.IsWebSocketUpgrade(r) {
		connectionType = "websocket"
	}

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
		"duration_ms", durationMs,
		"connectionType", connectionType,
	)
}

func (p *Proxy) logSuccessfulRequest(r *http.Request, name string, status int, start time.Time) {
	duration := time.Since(start).Milliseconds()
	metricRequestDuration.WithLabelValues(r.Method, name, "success").Observe(float64(duration) / 1000)
	metricRequestErrors.WithLabelValues(r.Method, name, "success").Inc()

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

	// Create request with proper URL
	req, err := http.NewRequestWithContext(r.Context(), r.Method, urlPath, bytes.NewReader(body))
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
		p.logger.Error("Request failed",
			"error", err,
			"url", urlPath,
			"method", r.Method)
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

	for _, target := range p.targets {
		name := target.Name()
		connType := "http"
		if isWebSocket {
			connType = "websocket"
		}

		if !p.hcm.IsHealthy(name, connType) {
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
