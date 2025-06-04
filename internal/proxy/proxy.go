package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

type BufferedResponseWriter struct {
	header     http.Header
	body       *bytes.Buffer
	statusCode int
}

func (w *BufferedResponseWriter) Header() http.Header {
	return w.header
}

func (w *BufferedResponseWriter) Write(b []byte) (int, error) {
	return w.body.Write(b)
}

func (w *BufferedResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
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

	// Start health check manager
	if err := hcm.Start(ctx); err != nil {
		return nil, fmt.Errorf("failed to start health check manager: %w", err)
	}
	config.Logger.Info("health check manager started")

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
	var lastErr error

	// Try each provider until one succeeds
	for _, target := range p.targets {
		name := target.Name()
		if !p.hcm.IsHealthy(name) {
			continue
		}

		// Create a buffered response writer to capture the response
		bw := &BufferedResponseWriter{
			header: make(http.Header),
			body:   &bytes.Buffer{},
		}

		// Forward the request to the provider
		target.ServeHTTP(bw, r)

		// Check if the request was successful
		if !p.HasNodeProviderFailed(bw.statusCode) {
			// Copy headers and body to the original response writer
			p.copyHeaders(w, bw.header)
			w.WriteHeader(bw.statusCode)
			if _, err := io.Copy(w, bw.body); err != nil {
				p.logger.Error("failed to write response", "error", err)
			}

			// Record metrics
			duration := time.Since(start).Seconds()
			metricRequestDuration.WithLabelValues(r.Method, name, "success").Observe(duration)
			metricRequestErrors.WithLabelValues(r.Method, name, "success").Inc()
			return
		}

		// Record failure metrics
		duration := time.Since(start).Seconds()
		metricRequestDuration.WithLabelValues(r.Method, name, "error").Observe(duration)
		metricRequestErrors.WithLabelValues(r.Method, name, "error").Inc()

		// Mark provider as failed
		if p.HasNodeProviderFailed(bw.statusCode) {
			metricRequestErrors.WithLabelValues(r.Method, name, "rerouted").Inc()
			if hc := p.hcm.GetHealthChecker(name); hc != nil {
				p.logger.Debug("tainting provider due to failed request", 
					"provider", name,
					"status", bw.statusCode,
					"method", r.Method,
					"path", r.URL.Path)
				hc.TaintHTTP()
			}
			p.logger.Debug("provider failed, trying next", 
				"provider", name, 
				"status", bw.statusCode,
				"method", r.Method,
				"path", r.URL.Path,
				"body", bw.body.String(),
				"headers", r.Header,
				"duration", Duration(duration))
		}

		lastErr = fmt.Errorf("provider %s failed with status %d", name, bw.statusCode)
	}

	// If we get here, all providers failed
	if lastErr != nil {
		p.logger.Error("all providers failed", "error", lastErr)
		http.Error(w, "all providers failed", http.StatusServiceUnavailable)
	}
}

// copyHeaders copies headers from src to dst
func (p *Proxy) copyHeaders(dst http.ResponseWriter, src http.Header) {
	for k, v := range src {
		for _, val := range v {
			dst.Header().Add(k, val)
		}
	}
}
