package main

import (
	"context"
	"flag"
	"fmt"
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

func printVersion() {
	fmt.Printf("rpcgateway v%s (git: %s, built: %s)\n", Version, GitCommit, BuildTime)
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
		fmt.Fprintf(os.Stderr, "Error: --config flag is required\n")
		fmt.Fprintf(os.Stderr, "Usage: %s --config <config-file> [--randomize-providers] [--version]\n", os.Args[0])
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
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Wait for context cancellation
	<-ctx.Done()
	
	// Use a fresh context for shutdown
	service.Stop(context.Background())
}
