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

	// Create a channel to receive the signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Create a context that we can cancel
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signal in a separate goroutine
	go func() {
		<-sigChan
		slog.Info("received shutdown signal")
		cancel()
	}()

	if err := service.Start(ctx); err != nil {
		writeError("error: " + err.Error())
		os.Exit(1)
	}

	// Wait for context cancellation
	<-ctx.Done()
	
	// Use a fresh context for shutdown
	service.Stop(context.Background())
}
