package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Duration is a custom type that implements slog.LogValuer for better duration formatting
type Duration time.Duration

func (d Duration) LogValue() slog.Value {
	return slog.StringValue(time.Duration(d).String())
}

type Proxy struct {
	targets []*NodeProvider
	hcm     *HealthCheckManager
	timeout time.Duration
	logger  *slog.Logger

	metricRequestDuration *prometheus.HistogramVec
	metricRequestErrors   *prometheus.CounterVec
}

func NewProxy(config Config) (*Proxy, error) {
	proxy := &Proxy{
		hcm:     config.HealthcheckManager,
		timeout: config.Proxy.UpstreamTimeout,
		logger:  config.HealthcheckManager.logger,
		metricRequestDuration: promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "zeroex_rpc_gateway_request_duration_seconds",
				Help: "Histogram of response time for Gateway in seconds",
				Buckets: []float64{
					.025,
					.05,
					.1,
					.25,
					.5,
					1,
					2.5,
					5,
					10,
					15,
					20,
					25,
					30,
				},
			}, []string{
				"provider",
				"method",
				"status_code",
			}),
		metricRequestErrors: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "zeroex_rpc_gateway_request_errors_handled_total",
				Help: "The total number of request errors handled by gateway",
			}, []string{
				"provider",
				"type",
			}),
	}

	for _, target := range config.Targets {
		p, err := NewNodeProvider(target)
		if err != nil {
			return nil, err
		}

		proxy.targets = append(proxy.targets, p)
	}

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

func (p *Proxy) copyHeaders(dst http.ResponseWriter, src http.ResponseWriter) {
	for k, v := range src.Header() {
		if len(v) == 0 {
			continue
		}

		dst.Header().Set(k, v[0])
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
	// Read the body once and store it for reuse
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("Failed to read request body", "error", err)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// Create a new context for the request that won't be canceled by parent context
	ctx := context.Background()
	r = r.WithContext(ctx)

	// Try each provider sequentially
	for _, target := range p.targets {
		name := target.Name()
		
		// Skip if provider is unhealthy
		if !p.hcm.IsHealthy(name) {
			p.logger.Debug("Skipping unhealthy provider", 
				"provider", name,
			)
			continue
		}

		start := time.Now()
		p.logger.Debug("Attempting request with provider", 
			"provider", name,
			"method", r.Method,
			"path", r.URL.Path,
		)

		// Create a new request for each attempt to ensure clean state
		req, err := http.NewRequestWithContext(ctx, r.Method, r.URL.String(), bytes.NewReader(bodyBytes))
		if err != nil {
			p.logger.Error("Failed to create request", "error", err)
			continue
		}

		// Copy headers from original request
		for k, v := range r.Header {
			req.Header[k] = v
		}

		pw := NewResponseWriter()
		p.timeoutHandler(target).ServeHTTP(pw, req)

		if p.HasNodeProviderFailed(pw.statusCode) {
			p.metricRequestDuration.WithLabelValues(name, r.Method, strconv.Itoa(pw.statusCode)).
				Observe(time.Since(start).Seconds())
			p.metricRequestErrors.WithLabelValues(name, "rerouted").Inc()

			// Taint the provider if it returned an error
			if hc := p.hcm.GetHealthChecker(name); hc != nil {
				hc.TaintHTTP()
				p.logger.Debug("Provider tainted due to error", 
					"provider", name,
					"status", pw.statusCode,
				)
			}

			p.logger.Debug("Request failed, trying next provider", 
				"provider", name,
				"status", pw.statusCode,
			)
			continue
		}

		duration := time.Since(start)
		p.logger.Debug("Request successful with provider", 
			"provider", name,
			"status", pw.statusCode,
			"duration", Duration(duration),
		)

		p.copyHeaders(w, pw)
		w.WriteHeader(pw.statusCode)
		w.Write(pw.body.Bytes()) // nolint:errcheck

		p.metricRequestDuration.WithLabelValues(name, r.Method, strconv.Itoa(pw.statusCode)).
			Observe(duration.Seconds())

		return
	}

	p.logger.Debug("No healthy providers available")
	p.errServiceUnavailable(w)
}
