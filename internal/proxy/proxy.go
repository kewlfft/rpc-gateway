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

// copyHeaders copies all headers from src to dst without duplicating Content-* logic
func (p *Proxy) copyHeaders(dst http.ResponseWriter, src http.Header) {
	dstHeader := dst.Header()
	for k, values := range src {
		for _, v := range values {
			dstHeader.Add(k, v)
		}
	}
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

		// Clone request with buffered body
		req := r.Clone(r.Context())
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		// Use buffered response writer with pre-allocated buffer
		bw := &BufferedResponseWriter{
			header: make(http.Header, 4), // Pre-allocate for common headers
			body:   bytes.NewBuffer(make([]byte, 0, 32*1024)), // 32KB initial capacity
		}

		target.ServeHTTP(bw, req)
		duration := time.Since(start).Seconds()

		if !p.HasNodeProviderFailed(bw.statusCode) {
			p.copyHeaders(w, bw.header)
			w.WriteHeader(bw.statusCode)
			_, err := io.Copy(w, bw.body)
			if err != nil {
				p.logger.Error("failed to write response", "error", err)
			}
			metricRequestDuration.WithLabelValues(r.Method, name, "success").Observe(duration)
			metricRequestErrors.WithLabelValues(r.Method, name, "success").Inc()
			return
		}

		// Provider failed, record and taint
		metricRequestDuration.WithLabelValues(r.Method, name, "error").Observe(duration)
		metricRequestErrors.WithLabelValues(r.Method, name, "error").Inc()
		metricRequestErrors.WithLabelValues(r.Method, name, "rerouted").Inc()

		if hc := p.hcm.GetHealthChecker(name); hc != nil {
			hc.TaintHTTP()
		}

		p.logger.Debug("provider failed, trying next",
			"provider", name,
			"status", bw.statusCode,
			"method", r.Method,
			"path", r.URL.Path,
			"headers", r.Header,
			"duration", Duration(duration),
		)
	}

	p.logger.Error("all providers failed")
	http.Error(w, "all providers failed", http.StatusServiceUnavailable)
}

// GetHealthCheckManager returns the health check manager for this proxy
func (p *Proxy) GetHealthCheckManager() *HealthCheckManager {
	return p.hcm
}
