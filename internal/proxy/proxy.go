package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	rpcerrors "github.com/kewlfft/rpc-gateway/internal/errors"
)

// Proxy represents an RPC proxy with health checking and failover
type Proxy struct {
	hcm         *HealthCheckManager
	timeout     time.Duration
	logger      *slog.Logger
	targets     []*NodeProvider
	chainType   string
	transport   http.RoundTripper // shared pooled transport; see forwardRequest
	healthyPool sync.Pool
}

// RandomizeProviders randomizes the order of providers using a simple time-based shuffle to avoid math/rand
func (p *Proxy) RandomizeProviders() {
	n := len(p.targets)
	seed := uint64(time.Now().UnixNano())
	for i := n - 1; i > 0; i-- {
		seed = seed*6364136223846793005 + 1
		j := int(seed % uint64(i+1))
		p.targets[i], p.targets[j] = p.targets[j], p.targets[i]
	}
}

// NewProxy creates a new proxy
func NewProxy(config Config) (*Proxy, error) {
	// Create health check manager
	hcm, err := NewHealthCheckManager(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create health check manager: %w", err)
	}

	// Shared connection pool with the health checkers of this path
	client := CreateOptimizedHTTPClient(config.Path, config.Timeout)

	proxy := &Proxy{
		hcm:       hcm,
		timeout:   config.Timeout,
		logger:    config.Logger,
		chainType: config.ChainType,
		transport: client.Transport,
	}
	proxy.healthyPool.New = func() any {
		return make([]*NodeProvider, 0, len(config.Targets))
	}

	// Create providers for each target with pre-allocated capacity
	proxy.targets = make([]*NodeProvider, 0, len(config.Targets))
	for _, target := range config.Targets {
		p := NewNodeProvider(target, config.Timeout, config.Logger)
		// Link health checkers directly to the node provider for high performance health checks
		p.SetHealthCheckers(
			hcm.GetHealthChecker(target.Name, "http"),
			hcm.GetHealthChecker(target.Name, "websocket"),
		)
		proxy.targets = append(proxy.targets, p)
	}

	// The health check manager is started by RPCGateway.Start()
	return proxy, nil
}

// HasNodeProviderFailed checks if a provider has failed based on status code
func (p *Proxy) HasNodeProviderFailed(statusCode int) bool {
	// Consider any non-2xx status code as a failure
	return statusCode < 200 || statusCode >= 300
}

// copyResponse copies headers, status code, and body from the source response to the target response writer
func (p *Proxy) copyResponse(w http.ResponseWriter, resp *http.Response) error {
	maps.Copy(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	// Stream the response body directly
	if _, err := io.Copy(w, resp.Body); err != nil {
		// Check if this is a broken pipe error (client disconnected)
		if isBrokenPipeError(err) {
			// Don't treat broken pipe as a provider failure - it's a client disconnection
			return fmt.Errorf("client disconnected: %w", err)
		}
		return fmt.Errorf("failed to stream response: %w", err)
	}

	return nil
}

// getConnectionType determines the connection type based on the request
func (p *Proxy) getConnectionType(r *http.Request) string {
	if websocket.IsWebSocketUpgrade(r) {
		return "websocket"
	}
	return "http"
}

// handleProviderFailure taints the failed provider and logs the failure
func (p *Proxy) handleProviderFailure(target *NodeProvider, r *http.Request, start time.Time, statusCode int, err error) {
	name := target.Name()
	duration := time.Since(start).Milliseconds()

	connectionType := p.getConnectionType(r)
	if hc := target.HealthChecker(connectionType); hc != nil {
		hc.TaintHTTP()
	}

	p.logger.Debug("provider failed, trying next",
		"provider", name,
		"status", statusCode,
		"error", err,
		"method", r.Method,
		"upstream_path", r.URL.Path,
		"path", p.hcm.path,
		"duration_ms", duration,
		"connectionType", connectionType,
	)
}

func (p *Proxy) logSuccessfulRequest(r *http.Request, name string, status int, start time.Time) {
	// Hot path: skip building log attributes unless debug is enabled
	if !p.logger.Enabled(r.Context(), slog.LevelDebug) {
		return
	}
	duration := time.Since(start).Milliseconds()
	p.logger.Debug("request handled",
		"provider", name,
		"status", status,
		"method", r.Method,
		"path", p.hcm.path,
		"duration_ms", duration,
	)
}

// forwardResult tells ServeHTTP whether to try the next provider.
type forwardResult int

const (
	forwardRetry forwardResult = iota // failed before anything was written to the client
	forwardDone                       // response sent, or client gone / response already committed
)

// forwardRequest handles both standard and Tron requests with direct streaming
func (p *Proxy) forwardRequest(w http.ResponseWriter, r *http.Request, body []byte, start time.Time, target *NodeProvider, urlPath string) forwardResult {
	name := target.Name()

	// Create a new context with timeout for this specific request to avoid context cancellation cascade
	// This prevents all providers from failing when the original request context is cancelled
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	// Create request with proper URL using the new context
	req, err := http.NewRequestWithContext(ctx, r.Method, urlPath, bytes.NewReader(body))
	if err != nil {
		p.logger.Error("Failed to create request",
			"error", err,
			"method", r.Method,
			"path", r.URL.Path,
			"provider_url", urlPath)
		p.handleProviderFailure(target, r, start, http.StatusServiceUnavailable, err)
		return forwardRetry
	}

	req.URL.RawQuery = r.URL.RawQuery

	customHeaders := target.config.Connection.HTTP.Headers
	if len(customHeaders) == 0 && req.URL.User == nil {
		// Nothing to add: forward the client's headers as-is. The transport never
		// modifies request headers, so sharing r.Header is safe and saves a copy.
		req.Header = r.Header
	} else {
		// Copy so provider-specific headers never leak into r.Header, which is
		// reused when failing over to the next provider
		maps.Copy(req.Header, r.Header)
		for key, value := range customHeaders {
			req.Header.Set(key, value)
		}
		// Basic auth from a "user:pass@" provider URL, as http.Client would add it
		if u := req.URL.User; u != nil && req.Header.Get("Authorization") == "" {
			password, _ := u.Password()
			req.SetBasicAuth(u.Username(), password)
		}
	}

	// Call the transport directly: http.Client would clone all headers (for redirects)
	// and arm a second deadline timer per request. Our ctx already enforces p.timeout,
	// and redirects are not followed (a 3xx counts as a provider failure).
	resp, err := p.transport.RoundTrip(req)
	if err != nil {
		// Check if this is a context cancellation error
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			p.logger.Warn("Request cancelled or timed out",
				"error", err,
				"url", urlPath,
				"method", r.Method,
				"context_error", ctx.Err())
		} else {
			p.logger.Error("Request failed",
				"error", err,
				"url", urlPath,
				"method", r.Method)
		}
		p.handleProviderFailure(target, r, start, http.StatusServiceUnavailable, err)
		return forwardRetry
	}
	defer resp.Body.Close()

	// Check for non-2xx status codes
	if p.HasNodeProviderFailed(resp.StatusCode) {
		p.logger.Error("Provider returned error status",
			"provider", name,
			"status", resp.StatusCode,
			"method", r.Method,
			"url", urlPath)
		p.handleProviderFailure(target, r, start, resp.StatusCode, nil)
		return forwardRetry
	}

	if err := p.copyResponse(w, resp); err != nil {
		// Check if this is a broken pipe error (client disconnected)
		if isBrokenPipeError(err) {
			p.logger.Debug("Client disconnected during response streaming",
				"error", err,
				"url", urlPath,
				"method", r.Method)
			// Don't taint provider for client disconnection and don't retry
			return forwardDone
		}

		p.logger.Error("Failed to copy response",
			"error", err,
			"url", urlPath,
			"method", r.Method)
		p.handleProviderFailure(target, r, start, resp.StatusCode, err)
		// Headers are already sent: retrying would corrupt the response
		p.logger.Warn("Provider failed during streaming, cannot retry to avoid response corruption",
			"provider", name,
			"path", p.hcm.path)
		return forwardDone
	}

	p.logSuccessfulRequest(r, name, resp.StatusCode, start)
	return forwardDone
}

// ServeHTTP forwards the request to the first healthy provider, failing over on errors.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	connType := p.getConnectionType(r)
	isWebSocket := connType == "websocket"

	var bodyBytes []byte
	if !isWebSocket {
		var err error
		bodyBytes, err = readBody(r)
		if err != nil {
			p.logger.Error("failed to read request body",
				"error", err,
				"method", r.Method,
				"path", r.URL.Path)
			rpcerrors.WriteJSONRPCError(w, r, "Failed to read request body", http.StatusBadRequest, nil)
			return
		}
	}

	healthyProviders := p.getHealthyProviders(connType)
	defer p.releaseHealthyProviders(healthyProviders)

	if len(healthyProviders) == 0 {
		providerDetails := p.collectProviderDetails(connType)
		rpcerrors.WriteJSONRPCError(w, r, fmt.Sprintf("No healthy providers available for chain %s", p.hcm.path), http.StatusServiceUnavailable, bodyBytes, providerDetails...)
		return
	}

	for _, target := range healthyProviders {
		name := target.Name()

		if isWebSocket {
			wsProxy := target.GetWebSocketProxy()
			if wsProxy == nil {
				continue
			}

			// Create upstream connection BEFORE upgrading (upgrade commits HTTP response)
			// If this fails, we can try next provider without committing to the client
			targetConn := wsProxy.createNewConnection()
			if targetConn == nil {
				if hc := target.HealthChecker(connType); hc != nil {
					hc.TaintHTTP()
				}
				p.logger.Debug("WebSocket connection failed, trying next provider",
					"failed_provider", name,
					"total_providers", len(healthyProviders))
				continue
			}
			// Close test connection - the real one is created in ServeHTTP
			targetConn.Close()

			// Connection successful - safe to upgrade
			wsProxy.ServeHTTP(w, r)
			return
		}

		url := target.config.Connection.HTTP.URL
		if p.chainType == "tron" {
			url += r.URL.Path
		}

		if p.forwardRequest(w, r, bodyBytes, start, target, url) == forwardDone {
			return
		}
	}

	providerDetails := p.collectProviderDetails(connType)
	rpcerrors.WriteJSONRPCError(w, r, fmt.Sprintf("All providers failed for chain %s", p.hcm.path), http.StatusServiceUnavailable, bodyBytes, providerDetails...)
}

// readBody reads and closes the request body, sizing the buffer from
// Content-Length when known to avoid repeated growth.
func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	if n := r.ContentLength; n > 0 && n <= maxPresizedBody {
		// net/http caps the body at Content-Length, so an exact-size read suffices
		buf := make([]byte, n)
		if _, err := io.ReadFull(r.Body, buf); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		return buf, nil
	}
	return io.ReadAll(r.Body)
}

// maxPresizedBody caps the Content-Length we trust for pre-allocation.
const maxPresizedBody = 1 << 20

// GetHealthCheckManager returns the health check manager for this proxy
func (p *Proxy) GetHealthCheckManager() *HealthCheckManager {
	return p.hcm
}

// GetChainType returns the chain type of the proxy
func (p *Proxy) GetChainType() string {
	return p.chainType
}

// GetTargets returns the targets slice (not a copy)
func (p *Proxy) GetTargets() []*NodeProvider {
	return p.targets
}

// getHealthyProviders returns only healthy providers for the given connection type.
// The returned slice is pooled; call releaseHealthyProviders when done.
func (p *Proxy) getHealthyProviders(connType string) []*NodeProvider {
	healthy := p.healthyPool.Get().([]*NodeProvider)
	healthy = healthy[:0]
	for _, target := range p.targets {
		if target.IsHealthy(connType) {
			healthy = append(healthy, target)
		}
	}
	return healthy
}

func (p *Proxy) releaseHealthyProviders(healthy []*NodeProvider) {
	p.healthyPool.Put(healthy)
}

// collectProviderDetails collects diagnostic information about all providers
func (p *Proxy) collectProviderDetails(connType string) []string {
	providerDetails := make([]string, len(p.targets))
	for i, target := range p.targets {
		name := target.Name()
		checker := target.HealthChecker(connType)
		switch {
		case checker == nil:
			providerDetails[i] = name + ":no_checker"
		case checker.IsTainted():
			providerDetails[i] = name + ":tainted"
		default:
			providerDetails[i] = name + ":unknown"
		}
	}
	return providerDetails
}
