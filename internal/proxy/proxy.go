package proxy

import (
	"bytes"
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
	return statusCode >= http.StatusInternalServerError || statusCode == http.StatusTooManyRequests
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
	// Read body once and store it
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("Failed to read request body", "error", err)
		p.errServiceUnavailable(w)
		return
	}
	defer r.Body.Close()

	for _, target := range p.targets {
		if !p.hcm.IsHealthy(target.Name()) {
			p.logger.Debug("Skipping unhealthy provider", "provider", target.Name())
			continue
		}
		start := time.Now()

		p.logger.Debug("Attempting request with provider", 
			"provider", target.Name(),
			"method", r.Method,
			"path", r.URL.Path,
		)

		pw := NewResponseWriter()
		// Reuse the same body bytes for each attempt
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		p.timeoutHandler(target).ServeHTTP(pw, r)

		if p.HasNodeProviderFailed(pw.statusCode) {
			p.metricRequestDuration.WithLabelValues(target.Name(), r.Method, strconv.Itoa(pw.statusCode)).
				Observe(time.Since(start).Seconds())
			p.metricRequestErrors.WithLabelValues(target.Name(), "rerouted").Inc()

			p.logger.Debug("Request failed, trying next provider", 
				"provider", target.Name(),
				"status", pw.statusCode,
			)
			continue
		}

		duration := time.Since(start)
		p.logger.Debug("Request successful with provider", 
			"provider", target.Name(),
			"status", pw.statusCode,
			"duration", Duration(duration),
		)

		p.copyHeaders(w, pw)
		w.WriteHeader(pw.statusCode)
		w.Write(pw.body.Bytes()) // nolint:errcheck

		p.metricRequestDuration.WithLabelValues(target.Name(), r.Method, strconv.Itoa(pw.statusCode)).
			Observe(duration.Seconds())

		return
	}

	p.logger.Debug("No healthy providers available")
	p.errServiceUnavailable(w)
}
