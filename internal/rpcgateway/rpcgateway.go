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

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/kewlfft/rpc-gateway/internal/metrics"
	"github.com/kewlfft/rpc-gateway/internal/proxy"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"
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

	// Start all health check managers (Start is non-blocking)
	for path, hcm := range r.hcms {
		if err := hcm.Start(c); err != nil {
			slog.Error("failed to start health check manager", "path", path, "error", err)
		}
	}

	// Start metrics server
	go func() {
		if err := r.metrics.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metrics server error", "error", err)
		}
	}()

	// Start main server
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
	defer ln.Close()
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
	// Set log level based on LOG_LEVEL environment variable
	logLevel := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}[strings.ToLower(os.Getenv("LOG_LEVEL"))]
	if logLevel == 0 {
		logLevel = slog.LevelWarn // Default to warn
	}

	// Initialize maps for proxies and health check managers
	proxies := make(map[string]proxy.ChainTypeHandler)
	hcms := make(map[string]*proxy.HealthCheckManager)

	logger := slog.New(
		slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
			Level: logLevel,
		}))

	// Create health check managers and proxies for each proxy config
	for i, proxyConfig := range config.Proxies {
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
			Logger:          logger,
			DisableHealthChecks: true, // Always disable health checks in NewProxy
			PathIndex:       i, // Pass the path index for incremental staggering
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

	// Handle each proxy path
	for path, p := range proxies {
		// Get the chain type for this proxy
		chainType := p.GetChainType()

		// Define a reusable handler constructor with optimized path handling
		handler := func(p http.Handler) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				// Fast path for WebSocket requests
				if websocket.IsWebSocketUpgrade(r) {
					p.ServeHTTP(w, r)
					return
				}

				// Optimize path handling based on chain type
				if chainType == "tron" {
					// Handle Tron chain paths
					if path := strings.TrimPrefix(r.URL.Path, "/"+path); path != "" {
						r.URL.Path = path
					} else {
						r.URL.Path = "/"
					}
				} else {
					// For non-Tron chains, preserve the original path
					r.URL.Path = strings.TrimPrefix(r.URL.Path, "/"+path)
				}

				// Forward the request with original method and query params
				p.ServeHTTP(w, r)
			}
		}

		// Register base path and trailing slash path
		basePath := fmt.Sprintf("/%s", path)
		r.Handle(basePath, handler(p))
		r.Handle(basePath+"/", handler(p))
		
		// Add catch-all route for Tron chain type
		if chainType == "tron" {
			r.Handle(basePath+"/*", handler(p))
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

// NewRPCGatewayFromConfigFile creates an instance of RPCGateway from provided configuration file.
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
