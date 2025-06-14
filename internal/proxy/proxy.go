package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
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

	// Ensure Content-Encoding header is preserved
	if resp.Header.Get("Content-Encoding") != "" {
		w.Header().Set("Content-Encoding", resp.Header.Get("Content-Encoding"))
	}

	// Ensure Content-Type header is preserved
	if resp.Header.Get("Content-Type") != "" {
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
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

	// Ensure content type is set
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
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

		if p.chainType == "tron" {
			// Extract method and prepare request body based on URL path
			method, requestBody := p.extractMethodAndBody(r.URL.Path, bodyBytes)
			if method == "" {
				// For Tron chain type, return a proper JSON-RPC error for invalid requests
				if r.URL.Path == "/" {
					p.writeErrorResponse(w, r, "Invalid JSON request", http.StatusBadRequest)
					return
				}
				continue
			}

			// Determine the correct URL prefix based on the original request path
			urlPrefix := "/wallet/"
			if strings.HasPrefix(r.URL.Path, "/walletsolidity/") {
				urlPrefix = "/walletsolidity/"
			}

			url := target.config.Connection.HTTP.URL + urlPrefix + method
			if p.forwardRequest(w, r, requestBody, start, target, url) {
				return
			}
			continue
		}

		// Standard HTTP request
		if p.forwardRequest(w, r, bodyBytes, start, target, target.config.Connection.HTTP.URL) {
			return
		}
	}

	p.writeErrorResponse(w, r, "All providers failed", http.StatusServiceUnavailable)
}

func (p *Proxy) extractMethodAndBody(path string, body []byte) (string, []byte) {
	// Fast path: REST-style wallet endpoint
	switch {
	case strings.HasPrefix(path, "/wallet/"):
		return strings.TrimPrefix(path, "/wallet/"), body
	case strings.HasPrefix(path, "/walletsolidity/"):
		return strings.TrimPrefix(path, "/walletsolidity/"), body
	}

	// JSON-RPC request path
	var parsed struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		ID     json.RawMessage `json:"id"`
	}

	if err := json.Unmarshal(body, &parsed); err != nil {
		p.logger.Error("Tron handler: Invalid JSON", "error", err, "body", string(body))
		return "", nil
	}

	if parsed.Method == "" {
		p.logger.Error("Tron handler: Missing method", "body", string(body))
		return "", nil
	}

	// Determine the URL method (strip prefix), but keep the original method for the JSON-RPC body
	urlMethod := parsed.Method
	if strings.HasPrefix(urlMethod, "wallet/") {
		urlMethod = strings.TrimPrefix(urlMethod, "wallet/")
	} else if strings.HasPrefix(urlMethod, "walletsolidity/") {
		urlMethod = strings.TrimPrefix(urlMethod, "walletsolidity/")
	}

	// Wrap params if needed (ensure it's a valid JSON array)
	params := parsed.Params
	if len(params) > 0 && params[0] != '[' {
		params = append([]byte("["), append(params, ']')...)
	} else if len(params) == 0 {
		params = []byte("[]")
	}

	// Construct the canonical JSON-RPC request body
	buf := new(bytes.Buffer)
	json.NewEncoder(buf).Encode(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  parsed.Method,
		"params":  params,
		"id":      parsed.ID,
	})
	p.logger.Debug("JSON-RPC body sent by proxy", "body", buf.String())
	return urlMethod, buf.Bytes()
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
