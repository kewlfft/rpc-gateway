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
	"net/http/httptest"
	"strconv"
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
		"path", p.hcm.path,
		"duration_ms", durationMs,
		"connectionType", connectionType,
	)
}

// ServeHTTP handles incoming HTTP requests
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	isWebSocket := websocket.IsWebSocketUpgrade(r)

	// Read body once if HTTP
	var bodyBytes []byte
	if !isWebSocket {
		var err error
		bodyBytes, err = io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			p.writeErrorResponse(w, r, "Failed to read request body", http.StatusBadRequest)
			return
		}
	}

	// Generic HTTP/WebSocket handling
	for _, target := range p.targets {
		name := target.Name()
		connType := "http"
		if isWebSocket {
			connType = "websocket"
		}

		if !p.hcm.IsHealthy(name, connType) {
			continue
		}
		if hc := p.hcm.GetHealthChecker(name, connType); hc != nil {
			hc.PostponeCheck()
		}

		if isWebSocket {
			target.ServeHTTP(w, r)
			return
		}

		// Handle Tron requests
		if p.chainType == "tron" {
			if p.handleTronRequest(w, r, bodyBytes, start) {
				return
			}
			continue
		}

		req := r.Clone(r.Context())
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		rec := httptest.NewRecorder()
		target.ServeHTTP(rec, req)

		if err, ok := req.Context().Value("error").(error); ok {
			status, _ := req.Context().Value("statusCode").(int)
			p.handleProviderFailure(name, r, start, status, err)
			continue
		}

		if p.HasNodeProviderFailed(rec.Code) {
			p.handleProviderFailure(name, r, start, rec.Code, nil)
			continue
		}

		for k, vals := range rec.Header() {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}

		respBody := rec.Body.Bytes()
		w.Header().Set("Content-Length", strconv.Itoa(len(respBody)))
		w.WriteHeader(rec.Code)
		if len(respBody) > 0 {
			if _, err := w.Write(respBody); err != nil {
				p.writeErrorResponse(w, r, "Failed to write response", http.StatusInternalServerError)
				return
			}
		}

		duration := time.Since(start).Milliseconds()
		metricRequestDuration.WithLabelValues(r.Method, name, "success").Observe(float64(duration) / 1000)
		metricRequestErrors.WithLabelValues(r.Method, name, "success").Inc()

		p.logger.Debug("request handled",
			"provider", name,
			"status", rec.Code,
			"method", r.Method,
			"path", p.hcm.path,
			"duration_ms", duration,
		)
		return
	}

	p.writeErrorResponse(w, r, "All providers failed", http.StatusServiceUnavailable)
}

// handleTronRequest handles Tron-specific HTTP forwarding logic.
// Returns:
//   - true if request was handled (success or invalid request)
//   - false if all providers failed
func (p *Proxy) handleTronRequest(w http.ResponseWriter, r *http.Request, body []byte, start time.Time) bool {
	const walletPrefix = "/wallet/"

	// Direct REST-style Tron API (e.g., /wallet/gettransactionbyid)
	if strings.HasPrefix(r.URL.Path, walletPrefix) {
		method := strings.TrimPrefix(r.URL.Path, walletPrefix)
		return p.forwardTronCall(w, r, start, method, body)
	}

	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		p.writeErrorResponse(w, r, "Invalid JSON request", http.StatusBadRequest)
		return true
	}

	method, ok := parsed["method"].(string)
	if !ok {
		p.writeErrorResponse(w, r, "Missing method in request", http.StatusBadRequest)
		return true
	}

	method = strings.TrimPrefix(method, "wallet/")
	return p.forwardTronCall(w, r, start, method, body)
}

// forwardTronCall sends the request to healthy Tron targets.
// Returns true if request was handled, false if all providers failed.
func (p *Proxy) forwardTronCall(w http.ResponseWriter, r *http.Request, start time.Time, method string, body []byte) bool {
	client := &http.Client{Timeout: p.timeout}
	contentType := "application/json"

	for _, target := range p.targets {
		name := target.Name()

		if !p.hcm.IsHealthy(name, "http") {
			continue
		}
		if hc := p.hcm.GetHealthChecker(name, "http"); hc != nil {
			hc.PostponeCheck()
		}

		url := target.config.Connection.HTTP.URL + "/wallet/" + method
		req, err := http.NewRequestWithContext(r.Context(), "POST", url, bytes.NewReader(body))
		if err != nil {
			continue
		}

		req.Header.Set("Content-Type", contentType)
		if apiKey := target.config.Connection.HTTP.APIKey; apiKey != "" {
			req.Header.Set("TRON-PRO-API-KEY", apiKey)
		}

		resp, err := client.Do(req)
		if err != nil {
			p.handleProviderFailure(name, r, start, http.StatusServiceUnavailable, err)
			continue
		}

		bodyData, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			p.handleProviderFailure(name, r, start, resp.StatusCode, err)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			p.handleProviderFailure(name, r, start, resp.StatusCode, nil)
			continue
		}

		// Send successful response
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bodyData)
		return true
	}

	return false
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
