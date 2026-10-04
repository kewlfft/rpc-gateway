package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/kewlfft/rpc-gateway/internal/rpcgateway"
)

// Version information
var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildTime = "unknown"
)

// Helper function for writing error messages to stderr
func writeError(msg string) {
	os.Stderr.Write([]byte(msg + "\n"))
}

func printVersion() {
	os.Stdout.Write([]byte("rpcgateway v" + Version + " (git: " + GitCommit + ", built: " + BuildTime + ")\n"))
	os.Exit(0)
}

func main() {
	// Define command line flags
	configPath := flag.String("config", "", "Path to the configuration file")
	randomizeProviders := flag.Bool("randomize-providers", false, "Randomize providers at startup (overrides config file)")
	showVersion := flag.Bool("version", false, "Show version information")

	// Parse flags
	flag.Parse()

	// Check for version flag
	if *showVersion {
		printVersion()
	}

	// Validate required flags
	if *configPath == "" {
		writeError("Error: --config flag is required")
		writeError("Usage: " + os.Args[0] + " --config <config-file> [--randomize-providers] [--version]")
		os.Exit(1)
	}

	slog.Info("starting rpc-gateway",
		"version", Version,
		"git_commit", GitCommit,
		"build_time", BuildTime,
		"config", *configPath,
		"randomize_providers", *randomizeProviders)

	service, err := rpcgateway.NewRPCGatewayFromConfigFile(*configPath)
	if err != nil {
		writeError("error: " + err.Error())
		os.Exit(1)
	}

	// Override randomizeProviders from config if flag is set
	if *randomizeProviders {
		service.SetRandomizeProviders(true)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := service.Start(ctx); err != nil {
		writeError("error: " + err.Error())
		os.Exit(1)
	}

	<-ctx.Done()
	slog.Info("received shutdown signal")

	// Use a fresh context for shutdown
	service.Stop(context.Background())
}
