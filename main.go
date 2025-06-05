package main

import (
	"context"
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
	// Check for version flag
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		printVersion()
	}

	if len(os.Args) != 3 || os.Args[1] != "--config" {
		fmt.Fprintf(os.Stderr, "Usage: %s --config <config-file>\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "       %s --version\n", os.Args[0])
		os.Exit(1)
	}

	configPath := os.Args[2]
	slog.Info("starting rpc-gateway", 
		"config", configPath,
		"version", Version,
		"git_commit", GitCommit,
		"build_time", BuildTime)

	service, err := rpcgateway.NewRPCGatewayFromConfigFile(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
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
