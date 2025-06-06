package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"time"

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

// Duration is a custom type that implements slog.LogValuer for better duration formatting
type Duration time.Duration

func (d Duration) LogValue() slog.Value {
	return slog.StringValue(time.Duration(d).String())
}

// Proxy represents an RPC proxy with health checking and failover
type Proxy struct {
	hcm     *HealthCheckManager
	timeout time.Duration
	logger  *slog.Logger
	targets []*NodeProvider
}

// NewProxy creates a new proxy
func NewProxy(ctx context.Context, config Config) (*Proxy, error) {
	// Create health check manager
	hcm, err := NewHealthCheckManager(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create health check manager: %w", err)
	}

	proxy := &Proxy{
		hcm:     hcm,
		timeout: config.UpstreamTimeout,
		logger:  config.Logger,
	}

	// Create providers for each target
	for _, target := range config.Targets {
		p, err := NewNodeProvider(target)
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
	return statusCode >= http.StatusInternalServerError || 
		statusCode == http.StatusTooManyRequests ||
		statusCode == http.StatusRequestEntityTooLarge ||
		statusCode == http.StatusForbidden ||
		statusCode == http.StatusUnauthorized ||
		statusCode == http.StatusServiceUnavailable
}

// ServeHTTP handles incoming HTTP requests
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Read and buffer request body once
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("failed to read request body", "error", err)
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	for _, target := range p.targets {
		name := target.Name()
		if !p.hcm.IsHealthy(name) {
			continue
		}

		// Postpone health check since we're making a request
		if hc := p.hcm.GetHealthChecker(name); hc != nil {
			hc.PostponeCheck()
		}

		// Clone request with buffered body
		req := r.Clone(r.Context())
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		// Use httptest.NewRecorder to capture headers and body
		rec := httptest.NewRecorder()

		target.ServeHTTP(rec, req)
		duration := time.Since(start).Seconds()

		if !p.HasNodeProviderFailed(rec.Code) {
			// Copy headers
			for k, values := range rec.Header() {
				if len(values) > 0 {
					w.Header().Set(k, values[0])
				}
			}
			w.WriteHeader(rec.Code)
			_, err := w.Write(rec.Body.Bytes())
			if err != nil {
				p.logger.Error("failed to write response", "error", err)
			}
			durationMs := time.Since(start).Milliseconds()
			metricRequestDuration.WithLabelValues(r.Method, name, "success").Observe(float64(durationMs) / 1000)
			metricRequestErrors.WithLabelValues(r.Method, name, "success").Inc()
			p.logger.Debug("request handled by provider",
				"provider", name,
				"status", rec.Code,
				"method", r.Method,
				"path", r.URL.Path,
				"duration_ms", durationMs,
			)
			return
		}

		// Provider failed, record and taint
		durationMs := time.Since(start).Milliseconds()
		metricRequestDuration.WithLabelValues(r.Method, name, "error").Observe(float64(durationMs) / 1000)
		metricRequestErrors.WithLabelValues(r.Method, name, "error").Inc()
		metricRequestErrors.WithLabelValues(r.Method, name, "rerouted").Inc()

		if hc := p.hcm.GetHealthChecker(name); hc != nil {
			hc.TaintHTTP()
		}

		p.logger.Debug("provider failed, trying next",
			"provider", name,
			"status", rec.Code,
			"method", r.Method,
			"path", r.URL.Path,
			"duration_ms", durationMs,
		)
	}

	p.logger.Error("all providers failed")
	http.Error(w, "all providers failed", http.StatusServiceUnavailable)
}

// GetHealthCheckManager returns the health check manager for this proxy
func (p *Proxy) GetHealthCheckManager() *HealthCheckManager {
	return p.hcm
}
