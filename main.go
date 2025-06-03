package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/0xProject/rpc-gateway/internal/rpcgateway"
)

func main() {
	if len(os.Args) != 3 || os.Args[1] != "--config" {
		fmt.Fprintf(os.Stderr, "Usage: %s --config <config-file>\n", os.Args[0])
		os.Exit(1)
	}

	configPath := os.Args[2]
	slog.Info("starting rpc-gateway", "config", configPath)

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

	if err := service.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	<-sigChan
	slog.Info("received shutdown signal")
	
	// Cancel the context to stop all background operations
	cancel()
	
	service.Stop(ctx)
	os.Exit(0)
}
