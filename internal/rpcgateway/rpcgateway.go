package rpcgateway

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/kewlfft/rpc-gateway/internal/metrics"
	"github.com/kewlfft/rpc-gateway/internal/proxy"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"
)

type RPCGateway struct {
	config  RPCGatewayConfig
	proxies map[string]*proxy.Proxy
	hcms    map[string]*proxy.HealthCheckManager
	server  *http.Server
	metrics *metrics.Server
}

func (r *RPCGateway) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.server.Handler.ServeHTTP(w, req)
}

func (r *RPCGateway) Start(c context.Context) error {
	// Check if ports are available
	if err := checkPortAvailability(r.config.Port); err != nil {
		return errors.Wrap(err, "rpc-gateway port not available")
	}
	if err := checkPortAvailability(fmt.Sprintf("%d", r.config.Metrics.Port)); err != nil {
		return errors.Wrap(err, "metrics port not available")
	}

	// Start health check managers with staggered starts
	// This prevents concurrent health checks to the same provider domain across different paths
	for path, hcm := range r.hcms {
		// Add a small delay between each path's health check start
		// This helps prevent rate limiting from providers that are used across multiple paths
		time.Sleep(350 * time.Millisecond)
		if err := hcm.Start(c); err != nil {
			return errors.Wrapf(err, "failed to start health check manager for path %s", path)
		}
	}

	// Start metrics server in a goroutine
	go func() {
		if err := r.metrics.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metrics server error", "error", err)
		}
	}()

	// Start main server in a goroutine
	go func() {
		if err := r.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("rpc-gateway server error", "error", err)
		}
	}()

	return nil
}

// checkPortAvailability checks if a port is available for use
func checkPortAvailability(port string) error {
	addr := fmt.Sprintf(":%s", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("port %s is not available: %w", port, err)
	}
	ln.Close()
	return nil
}

func (r *RPCGateway) Stop(c context.Context) error {
	// Stop servers in reverse order of dependency
	slog.Info("shutting down rpc-gateway")

	// Stop main server
	if err := r.server.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("error stopping rpc-gateway server", "error", err)
	}

	// Stop metrics server
	if err := r.metrics.Stop(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("error stopping metrics server", "error", err)
	}

	// Stop health check managers last
	for _, hcm := range r.hcms {
		if err := hcm.Stop(c); err != nil {
			slog.Error("error stopping health check manager", "error", err)
		}
	}

	slog.Info("rpc-gateway shutdown complete")
	return nil
}

func NewRPCGateway(config RPCGatewayConfig) (*RPCGateway, error) {
	logLevel := slog.LevelWarn

	// Set log level based on LOG_LEVEL environment variable
	if logLevelStr := os.Getenv("LOG_LEVEL"); logLevelStr != "" {
		switch logLevelStr {
		case "debug":
			logLevel = slog.LevelDebug
		case "info":
			logLevel = slog.LevelInfo
		case "warn":
			logLevel = slog.LevelWarn
		case "error":
			logLevel = slog.LevelError
		default:
			// If invalid level is provided, use warn as default
			logLevel = slog.LevelWarn
		}
	}

	// Initialize maps for proxies and health check managers
	proxies := make(map[string]*proxy.Proxy)
	hcms := make(map[string]*proxy.HealthCheckManager)

	// Create health check managers and proxies for each proxy config
	for _, proxyConfig := range config.Proxies {
		upstreamTimeout, err := time.ParseDuration(proxyConfig.UpstreamTimeout)
		if err != nil {
			return nil, errors.Wrap(err, "invalid upstream timeout")
		}

		// Create proxy configuration
		proxyCfg := proxy.Config{
			Path:            proxyConfig.Path,
			UpstreamTimeout: upstreamTimeout,
			HealthChecks:    proxyConfig.HealthChecks,
			Targets:         proxyConfig.Targets,
			Logger: slog.New(
				slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
					Level: logLevel,
				})),
			DisableHealthChecks: true, // Disable automatic start of health checks
		}

		// Create proxy
		p, err := proxy.NewProxy(context.Background(), proxyCfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create proxy: %w", err)
		}

		proxies[proxyConfig.Path] = p
		hcms[proxyConfig.Path] = p.GetHealthCheckManager()
	}

	r := chi.NewRouter()
	// Only add request logger in DEBUG mode
	if logLevel == slog.LevelDebug {
		// Remove the request logging middleware
	}

	// Recoverer is a middleware that recovers from panics, logs the panic (and
	// a backtrace), and returns a HTTP 500 (Internal Server Error) status if
	// possible. Recoverer prints a request ID if one is provided.
	r.Use(middleware.Recoverer)

	// Handle each proxy path
	for path, p := range proxies {
		r.Handle(fmt.Sprintf("/%s", path), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.URL.Path = "/"
			p.ServeHTTP(w, r)
		}))
	}

	return &RPCGateway{
		config:  config,
		proxies: proxies,
		hcms:    hcms,
		metrics: metrics.NewServer(
			metrics.Config{
				Port: config.Metrics.Port,
			},
		),
		server: &http.Server{
			Addr:              fmt.Sprintf(":%s", config.Port),
			Handler:           r,
			WriteTimeout:      time.Second * 15,
			ReadTimeout:       time.Second * 15,
			ReadHeaderTimeout: time.Second * 5,
		},
	}, nil
}

// NewRPCGatewayFromConfigFile creates an instance of RPCGateway from provided
// configuration file.
func NewRPCGatewayFromConfigFile(s string) (*RPCGateway, error) {
	data, err := os.ReadFile(s)
	if err != nil {
		return nil, err
	}

	var config RPCGatewayConfig

	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	slog.Info("Loaded config", "proxies", len(config.Proxies), "paths", func() []string {
		paths := make([]string, len(config.Proxies))
		for i, p := range config.Proxies {
			paths[i] = p.Path
		}
		return paths
	}())

	return NewRPCGateway(config)
}
