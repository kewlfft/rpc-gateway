package rpcgateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kewlfft/rpc-gateway/internal/proxy"
	"go.yaml.in/yaml/v3"
)

const (
	serverWriteTimeout      = 90 * time.Second
	serverReadTimeout       = 90 * time.Second
	serverReadHeaderTimeout = 10 * time.Second
	serverIdleTimeout       = 90 * time.Second
)

type RPCGateway struct {
	config  RPCGatewayConfig
	proxies map[string]*proxy.Proxy
	server  *http.Server
}

func (r *RPCGateway) Start(c context.Context) error {
	// Bind first so a busy port fails Start synchronously
	ln, err := net.Listen("tcp", r.server.Addr)
	if err != nil {
		return fmt.Errorf("rpc-gateway port %s not available: %w", r.config.Port, err)
	}

	// Start all health check managers (non-blocking)
	for _, p := range r.proxies {
		p.GetHealthCheckManager().Start(c)
	}

	// Start main server
	go func() {
		if err := r.server.Serve(ln); isUnexpectedServerError(err) {
			slog.Error("rpc-gateway server error", "error", err)
		}
	}()

	return nil
}

// isUnexpectedServerError reports whether err is a real error, i.e. non-nil and not
// http.ErrServerClosed (which is expected during shutdown)
func isUnexpectedServerError(err error) bool {
	return err != nil && !errors.Is(err, http.ErrServerClosed)
}

func (r *RPCGateway) Stop() {
	slog.Info("shutting down rpc-gateway")

	// Stop main server
	if err := r.server.Close(); isUnexpectedServerError(err) {
		slog.Error("error stopping rpc-gateway server", "error", err)
	}

	// Stop health check managers last
	for _, p := range r.proxies {
		p.GetHealthCheckManager().Stop()
	}

	slog.Info("rpc-gateway shutdown complete")
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

	proxies := make(map[string]*proxy.Proxy, len(config.Proxies))

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
			Path:         proxyConfig.Path,
			ChainType:    proxyConfig.ChainType,
			Timeout:      timeout,
			HealthChecks: proxyConfig.HealthChecks,
			Targets:      proxyConfig.Targets,
			Logger:       logger,
			PathIndex:    i, // Pass the path index for incremental staggering
		}

		// Create proxy
		p, err := proxy.NewProxy(proxyCfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create proxy: %w", err)
		}

		proxies[proxyConfig.Path] = p
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
			// Keep the escaped form consistent with Path (used for tron upstream paths)
			if r.URL.RawPath != "" {
				r.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, pathPrefix)
			}
			p.ServeHTTP(w, r)
		}

		// Register base path and subtree ("/path/" already matches every sub-path, incl. tron's)
		r.HandleFunc(pathPrefix, handler)
		r.HandleFunc(pathPrefix+"/", handler)
	}

	return &RPCGateway{
		config:  config,
		proxies: proxies,
		server: &http.Server{
			Addr:              ":" + config.Port,
			Handler:           r,
			WriteTimeout:      serverWriteTimeout,
			ReadTimeout:       serverReadTimeout,
			ReadHeaderTimeout: serverReadHeaderTimeout,
			IdleTimeout:       serverIdleTimeout,
		},
	}, nil
}

// NewRPCGatewayFromConfigFile creates an instance of RPCGateway from provided configuration file.
func NewRPCGatewayFromConfigFile(s string) (*RPCGateway, error) {
	data, err := os.ReadFile(s)
	if err != nil {
		return nil, err
	}

	// Strict decoding: unknown keys (typos, removed options such as "metrics")
	// are errors instead of being silently ignored
	var config RPCGatewayConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&config); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("invalid config %s: %w", s, err)
	}

	paths := make([]string, 0, len(config.Proxies))
	for _, p := range config.Proxies {
		paths = append(paths, p.Path)
	}
	slog.Info("Loaded config", "proxies", len(config.Proxies), "paths", paths)

	return NewRPCGateway(config)
}

// SetRandomizeProviders enables provider randomization and shuffles all proxies,
// unless providers were already randomized (via config or an earlier call).
func (r *RPCGateway) SetRandomizeProviders(randomize bool) {
	if !randomize || r.config.RandomizeProviders {
		return
	}
	r.config.RandomizeProviders = true
	for _, p := range r.proxies {
		p.RandomizeProviders()
	}
	slog.Info("providers randomized from CLI flag")
}
