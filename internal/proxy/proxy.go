package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
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

type Proxy struct {
	hcm     *HealthCheckManager
	timeout time.Duration
	logger  *slog.Logger
	targets []*NodeProvider
}

// NewProxy creates a new proxy
func NewProxy(ctx context.Context, config Config) (*Proxy, error) {
	proxy := &Proxy{
		hcm:     config.HealthcheckManager,
		timeout: config.Proxy.UpstreamTimeout,
		logger:  config.HealthcheckManager.Logger,
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
	if err := config.HealthcheckManager.Start(ctx); err != nil {
		return nil, fmt.Errorf("failed to start health check manager: %w", err)
	}
	config.HealthcheckManager.Logger.Info("health check manager started")

	return proxy, nil
}

func (p *Proxy) HasNodeProviderFailed(statusCode int) bool {
	// Consider the following as failures:
	// - HTTP 5xx responses (server errors)
	// - HTTP 429 (rate limiting)
	// - HTTP 413 (request entity too large)
	// - HTTP 403 (forbidden)
	// - HTTP 401 (unauthorized)
	// - HTTP 503 (service unavailable)
	//
	// Note: We don't consider 4xx client errors (except specific cases above)
	// as failures because different providers might respond differently
	// to the same query (e.g., non-existent block might be 400 for one
	// provider but 200 with error in JSON-RPC for another)
	return statusCode >= http.StatusInternalServerError || 
		statusCode == http.StatusTooManyRequests ||
		statusCode == http.StatusRequestEntityTooLarge ||
		statusCode == http.StatusForbidden ||
		statusCode == http.StatusUnauthorized ||
		statusCode == http.StatusServiceUnavailable
}

func (p *Proxy) copyHeaders(dst http.ResponseWriter, src http.Header) {
	for k, v := range src {
		for _, val := range v {
			dst.Header().Add(k, val)
		}
	}
}

func (p *Proxy) timeoutHandler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		handler := http.TimeoutHandler(next, p.timeout, http.StatusText(http.StatusGatewayTimeout))
		handler.ServeHTTP(w, r)
	}

	return http.HandlerFunc(fn)
}

func (p *Proxy) errServiceUnavailable(w http.ResponseWriter) {
	http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Read body once and reuse it
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("Failed to read request body", "error", err)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	for _, target := range p.targets {
		name := target.Name()
		
		if !p.hcm.IsHealthy(name) {
			p.logger.Debug("Skipping unhealthy provider", "provider", name)
			continue
		}

		start := time.Now()

		// Clone original request with clean context and body
		req := r.Clone(context.Background())
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		// Buffered response writer
		pw := &BufferedResponseWriter{
			header:     make(http.Header, len(r.Header)),
			body:       new(bytes.Buffer),
			statusCode: http.StatusOK,
		}

		p.timeoutHandler(target).ServeHTTP(pw, req)

		status := pw.statusCode
		duration := time.Since(start)

		p.logger.Debug("Provider response", 
			"provider", name,
			"status", status,
			"duration", duration)

		metricRequestDuration.WithLabelValues(r.Method, name, strconv.Itoa(status)).Observe(duration.Seconds())

		if p.HasNodeProviderFailed(status) {
			metricRequestErrors.WithLabelValues(r.Method, name, "rerouted").Inc()

			if hc := p.hcm.GetHealthChecker(name); hc != nil {
				hc.TaintHealthCheck()
			}

			p.logger.Debug("Request failed, trying next provider", "provider", name, "status", status)
			continue
		}

		p.logger.Debug("Request successful", "provider", name, "status", status, "duration", Duration(duration))

		// Copy headers
		p.copyHeaders(w, pw.header)

		w.WriteHeader(status)
		_, _ = w.Write(pw.body.Bytes())
		return
	}

	p.logger.Debug("No healthy providers available")
	p.errServiceUnavailable(w)
}
