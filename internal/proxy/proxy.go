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

// Proxy represents an RPC proxy with health checking and failover
type Proxy struct {
	hcm       *HealthCheckManager
	timeout   time.Duration
	logger    *slog.Logger
	targets   []*NodeProvider
	chainType string
}

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

	// Pre-read body for HTTP requests
	var bodyBytes []byte
	if !isWebSocket {
		var err error
		bodyBytes, err = io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			p.writeErrorResponse(w, r, "Failed to read request body", http.StatusBadRequest)
			return
		}
	}

	// Special handling for Tron requests (all methods)
	if p.chainType == "tron" && !isWebSocket {
		// Parse the JSON-RPC request
		var reqBody map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &reqBody); err != nil {
			p.writeErrorResponse(w, r, "Invalid JSON request", http.StatusBadRequest)
			return
		}
		method, ok := reqBody["method"].(string)
		if !ok {
			p.writeErrorResponse(w, r, "Missing method in request", http.StatusBadRequest)
			return
		}
		params, _ := reqBody["params"].([]interface{})
		id := reqBody["id"]

		// Prepare the body for Tron: use params[0] if present and is an object, else {}
		var tronBody []byte
		if len(params) > 0 {
			if m, ok := params[0].(map[string]interface{}); ok {
				tronBody, _ = json.Marshal(m)
			} else {
				tronBody = []byte("{}")
			}
		} else {
			tronBody = []byte("{}")
		}

		// Try each provider
		for _, target := range p.targets {
			name := target.Name()
			connectionType := "http"
			if !p.hcm.IsHealthy(name, connectionType) {
				continue
			}
			if hc := p.hcm.GetHealthChecker(name, connectionType); hc != nil {
				hc.PostponeCheck()
			}

			// Build the upstream URL
			upstreamURL := target.config.Connection.HTTP.URL + "/wallet/" + method
			upstreamReq, err := http.NewRequestWithContext(r.Context(), "POST", upstreamURL, bytes.NewReader(tronBody))
			if err != nil {
				continue
			}
			upstreamReq.Header.Set("Content-Type", "application/json")
			// Set API key if present
			apiKey := target.config.Connection.HTTP.APIKey
			if apiKey != "" {
				upstreamReq.Header.Set("TRON-PRO-API-KEY", apiKey)
			}

			client := &http.Client{Timeout: p.timeout}
			resp, err := client.Do(upstreamReq)
			if err != nil {
				p.handleProviderFailure(name, r, start, http.StatusServiceUnavailable, err)
				continue
			}
			defer resp.Body.Close()

			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				p.handleProviderFailure(name, r, start, resp.StatusCode, err)
				continue
			}

			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				p.handleProviderFailure(name, r, start, resp.StatusCode, nil)
				continue
			}

			// Wrap the Tron response in a JSON-RPC envelope
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"jsonrpc":"2.0","id":`))
			if id == nil {
				w.Write([]byte("null"))
			} else {
				idBytes, _ := json.Marshal(id)
				w.Write(idBytes)
			}
			w.Write([]byte(`,"result":`))
			w.Write(respBody)
			w.Write([]byte("}"))
			return
		}
		p.writeErrorResponse(w, r, "All providers failed", http.StatusServiceUnavailable)
		return
	}

	for _, target := range p.targets {
		name := target.Name()
		connectionType := "http"
		if isWebSocket {
			connectionType = "websocket"
		}

		if !p.hcm.IsHealthy(name, connectionType) {
			continue
		}

		if hc := p.hcm.GetHealthChecker(name, connectionType); hc != nil {
			hc.PostponeCheck()
		}

		// WebSocket: just forward
		if isWebSocket {
			target.ServeHTTP(w, r)
			return
		}

		// Clone request with original body for HTTP
		req := r.Clone(r.Context())
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		rec := httptest.NewRecorder()
		target.ServeHTTP(rec, req)

		// Handle simulated errors via context
		if err, ok := req.Context().Value("error").(error); ok {
			statusCode, _ := req.Context().Value("statusCode").(int)
			p.handleProviderFailure(name, r, start, statusCode, err)
			continue
		}

		if p.HasNodeProviderFailed(rec.Code) {
			p.handleProviderFailure(name, r, start, rec.Code, nil)
			continue
		}

		// Write headers and body
		for k, vv := range rec.Header() {
			for _, v := range vv {
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

		durationMs := time.Since(start).Milliseconds()
		metricRequestDuration.WithLabelValues(r.Method, name, "success").Observe(float64(durationMs) / 1000)
		metricRequestErrors.WithLabelValues(r.Method, name, "success").Inc()

		p.logger.Debug("request handled",
			"provider", name,
			"status", rec.Code,
			"method", r.Method,
			"path", p.hcm.path,
			"duration_ms", durationMs,
		)
		return
	}

	p.writeErrorResponse(w, r, "All providers failed", http.StatusServiceUnavailable)
}

// GetHealthCheckManager returns the health check manager for this proxy
func (p *Proxy) GetHealthCheckManager() *HealthCheckManager {
	return p.hcm
}

// GetTargets returns a copy of the targets slice
func (p *Proxy) GetTargets() []*NodeProvider {
	return p.targets
}
