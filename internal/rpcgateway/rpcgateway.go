package rpcgateway

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kewlfft/rpc-gateway/internal/metrics"
	"github.com/kewlfft/rpc-gateway/internal/proxy"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"
	"github.com/gorilla/websocket"
)

type RPCGateway struct {
	config  RPCGatewayConfig
	proxies map[string]proxy.ChainTypeHandler
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
		time.Sleep(300 * time.Millisecond)
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
	proxies := make(map[string]proxy.ChainTypeHandler)
	hcms := make(map[string]*proxy.HealthCheckManager)

	// Create health check managers and proxies for each proxy config
	for _, proxyConfig := range config.Proxies {
		timeout, err := time.ParseDuration(proxyConfig.Timeout)
		if err != nil {
			return nil, errors.Wrap(err, "invalid timeout")
		}

		// Create proxy configuration
		proxyCfg := proxy.Config{
			Path:            proxyConfig.Path,
			ChainType:       proxyConfig.ChainType,
			Timeout:         timeout,
			HealthChecks:    proxyConfig.HealthChecks,
			Targets:         proxyConfig.Targets,
			Logger: slog.New(
				slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
					Level: logLevel,
				})),
			DisableHealthChecks: true, // Always disable health checks in NewProxy
		}

		// Create proxy
		p, err := proxy.NewProxy(context.Background(), proxyCfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create proxy: %w", err)
		}

		proxies[proxyConfig.Path] = p
		hcms[proxyConfig.Path] = p.GetHealthCheckManager()
	}

	// Randomize providers if enabled
	if config.RandomizeProviders {
		for _, p := range proxies {
			p.RandomizeProviders()
		}
		slog.Info("providers randomized at startup")
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
		// Get the chain type for this proxy
		chainType := p.GetChainType()

		// Define a reusable handler constructor
		handler := func(p http.Handler, stripPath bool) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				logger := slog.Default()
				logger.Debug("handling request",
					"path", r.URL.Path,
					"method", r.Method,
					"upgrade", r.Header.Get("Upgrade"),
					"connection", r.Header.Get("Connection"))

				if websocket.IsWebSocketUpgrade(r) {
					logger.Debug("websocket upgrade request detected")
					p.ServeHTTP(w, r)
					return
				}

				// For Tron chain type, preserve the full path to handle wallet/ endpoints
				if chainType == "tron" {
					// Remove the proxy path prefix from the URL path
					r.URL.Path = strings.TrimPrefix(r.URL.Path, "/"+path)
					if r.URL.Path == "" {
						r.URL.Path = "/"
					}
				} else if stripPath {
					r.URL.Path = "/"
				}

				logger.Debug("modified request path",
					"original_path", r.URL.Path,
					"chain_type", chainType,
					"strip_path", stripPath)

				p.ServeHTTP(w, r)
			}
		}

		r.Handle(fmt.Sprintf("/%s", path), handler(p, true))
		r.Handle(fmt.Sprintf("/%s/", path), handler(p, true))
		// Add a catch-all route for Tron chain type to handle wallet/ endpoints
		if chainType == "tron" {
			r.Handle(fmt.Sprintf("/%s/*", path), handler(p, true))
		}
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

// SetRandomizeProviders sets the randomizeProviders flag and applies it to all proxies
func (r *RPCGateway) SetRandomizeProviders(randomize bool) {
	r.config.RandomizeProviders = randomize
	if randomize {
		for _, p := range r.proxies {
			p.RandomizeProviders()
		}
		slog.Info("providers randomized from CLI flag")
	}
}
