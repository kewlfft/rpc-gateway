package rpcgateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kewlfft/rpc-gateway/internal/metrics"
	"github.com/kewlfft/rpc-gateway/internal/proxy"
	"gopkg.in/yaml.v3"
)

type RPCGateway struct {
	config  RPCGatewayConfig
	proxies map[string]proxy.ChainTypeHandler
	hcms    map[string]*proxy.HealthCheckManager
	server  *http.Server
	metrics *metrics.Server
}


func (r *RPCGateway) Start(c context.Context) error {
	// Check if ports are available
	if err := checkPortAvailability(r.config.Port); err != nil {
		return fmt.Errorf("rpc-gateway port not available: %w", err)
	}
	if r.config.Metrics.IsEnabled() {
		portStr := fmt.Sprintf("%d", r.config.Metrics.Port)
		if err := checkPortAvailability(portStr); err != nil {
			return fmt.Errorf("metrics port not available: %w", err)
		}
	}

	// Start all health check managers (Start is non-blocking)
	for path, hcm := range r.hcms {
		if err := hcm.Start(c); err != nil {
			slog.Error("failed to start health check manager", "path", path, "error", err)
		}
	}

	// Start metrics server if enabled
	if r.config.Metrics.IsEnabled() {
		go func() {
			if err := r.metrics.Start(); isServerClosedError(err) {
				slog.Error("metrics server error", "error", err)
			}
		}()
	}

	// Start main server
	go func() {
		if err := r.server.ListenAndServe(); isServerClosedError(err) {
			slog.Error("rpc-gateway server error", "error", err)
		}
	}()

	return nil
}

// checkPortAvailability checks if a port is available for use
func checkPortAvailability(port string) error {
	addr := ":" + port
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("port %s is not available: %w", port, err)
	}
	defer ln.Close()
	return nil
}

// isServerClosedError checks if an error is http.ErrServerClosed (expected during shutdown)
func isServerClosedError(err error) bool {
	return err != nil && !errors.Is(err, http.ErrServerClosed)
}

func (r *RPCGateway) Stop(c context.Context) error {
	// Stop servers in reverse order of dependency
	slog.Info("shutting down rpc-gateway")

	// Stop main server
	if err := r.server.Close(); isServerClosedError(err) {
		slog.Error("error stopping rpc-gateway server", "error", err)
	}

	// Stop metrics server if enabled
	if r.config.Metrics.IsEnabled() {
		if err := r.metrics.Stop(); isServerClosedError(err) {
			slog.Error("error stopping metrics server", "error", err)
		}
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
	logLevelStr := strings.ToLower(os.Getenv("LOG_LEVEL"))
	logLevelMap := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	logLevel, exists := logLevelMap[logLevelStr]
	if !exists {
		logLevel = slog.LevelWarn // Default to warn
	}

	// Initialize maps for proxies and health check managers with known capacity
	proxies := make(map[string]proxy.ChainTypeHandler, len(config.Proxies))
	hcms := make(map[string]*proxy.HealthCheckManager, len(config.Proxies))

	logger := slog.New(
		slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
			Level: logLevel,
			ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				// Convert time to UTC
				if a.Key == slog.TimeKey {
					return slog.Attr{
						Key:   a.Key,
						Value: slog.TimeValue(a.Value.Time().UTC()),
					}
				}
				return a
			},
		}))

	// Log the configured log level using default logger so it always appears
	slog.Info("configured log level", "level", logLevel.String(), "LOG_LEVEL", os.Getenv("LOG_LEVEL"))

	// Create health check managers and proxies for each proxy config
	for i, proxyConfig := range config.Proxies {
		timeout, err := time.ParseDuration(proxyConfig.Timeout)
		if err != nil {
			return nil, fmt.Errorf("invalid timeout: %w", err)
		}

		// Create proxy configuration
		proxyCfg := proxy.Config{
			Path:            proxyConfig.Path,
			ChainType:       proxyConfig.ChainType,
			Timeout:         timeout,
			HealthChecks:    proxyConfig.HealthChecks,
			Targets:         proxyConfig.Targets,
			Logger:          logger,
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

	r := http.NewServeMux()

	// Handle each proxy path
	for path, p := range proxies {
		chainType := p.GetChainType()
		pathPrefix := "/" + path
		isTron := chainType == "tron"

		handler := func(w http.ResponseWriter, r *http.Request) {
			// WebSocket requests don't need path modification
			if websocket.IsWebSocketUpgrade(r) {
				p.ServeHTTP(w, r)
				return
			}
			// Trim path prefix and handle tron special case
			trimmed := strings.TrimPrefix(r.URL.Path, pathPrefix)
			if isTron && trimmed == "" {
				trimmed = "/"
			}
			r.URL.Path = trimmed
			p.ServeHTTP(w, r)
		}

		// Register routes: base path, trailing slash, and catch-all for tron
		r.Handle(pathPrefix, http.HandlerFunc(handler))
		r.Handle(pathPrefix+"/", http.HandlerFunc(handler))
		if isTron {
			r.Handle(pathPrefix+"/*", http.HandlerFunc(handler))
		}
	}

	return &RPCGateway{
		config:  config,
		proxies: proxies,
		hcms:    hcms,
		metrics: metrics.NewServer(config.Metrics),
		server: &http.Server{
			Addr:              ":" + config.Port,
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

	paths := make([]string, len(config.Proxies))
	for i, p := range config.Proxies {
		paths[i] = p.Path
	}
	slog.Info("Loaded config", "proxies", len(config.Proxies), "paths", paths)

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
