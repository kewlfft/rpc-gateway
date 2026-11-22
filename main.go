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

func main() {
	configPath := flag.String("config", "", "Path to the configuration file")
	randomizeProviders := flag.Bool("randomize-providers", false, "Randomize providers at startup (overrides config file)")
	showVersion := flag.Bool("version", false, "Show version information")
	flag.Parse()

	if *showVersion {
		fmt.Printf("rpcgateway v%s (git: %s, built: %s)\n", Version, GitCommit, BuildTime)
		os.Exit(0)
	}

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "Error: --config flag is required")
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
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	if *randomizeProviders {
		service.SetRandomizeProviders(true)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := service.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	<-ctx.Done()
	slog.Info("received shutdown signal")
	service.Stop(context.Background())
}
