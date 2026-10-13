// Command pin-glide-api runs the Pin Glide HTTP and WebSocket service.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/BuddyLabsCo/pin-glide-service/internal/api"
	"github.com/BuddyLabsCo/pin-glide-service/internal/config"
	"github.com/BuddyLabsCo/pin-glide-service/internal/db"
	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "pin-glide-api: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configuration, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, configuration.DatabaseURL)
	if err != nil {
		return err
	}
	defer func() { _ = pool.Close() }()

	listener, err := net.Listen("tcp", configuration.APIAddress)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", configuration.APIAddress, err)
	}

	logger := observability.NewLogger(os.Stderr, string(configuration.Environment))
	logger.Info("api listening", slog.String("address", listener.Addr().String()))

	return api.Serve(ctx, listener, api.Dependencies{Database: pool, Logger: logger})
}
