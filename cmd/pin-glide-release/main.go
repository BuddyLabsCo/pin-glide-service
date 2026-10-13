package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/BuddyLabsCo/pin-glide-service/internal/config"
	"github.com/BuddyLabsCo/pin-glide-service/internal/db"
	"github.com/BuddyLabsCo/pin-glide-service/internal/release"
)

const usage = "usage: pin-glide-release migrate"

func main() {
	if len(os.Args) != 2 || os.Args[1] != "migrate" {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}

	if err := migrate(); err != nil {
		fmt.Fprintf(os.Stderr, "pin-glide-release migrate: %v\n", err)
		os.Exit(1)
	}
}

func migrate() error {
	configuration, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, configuration.DatabaseURL)
	if err != nil {
		return err
	}
	defer func() { _ = pool.Close() }()

	options := release.Options{Version: os.Getenv("PIN_GLIDE_RELEASE_VERSION")}
	for name, target := range map[string]*bool{"PIN_GLIDE_RELEASE_INITIAL": &options.Initial, "PIN_GLIDE_RELEASE_INITIAL_RETRY": &options.InitialRetry} {
		if value := os.Getenv(name); value != "" {
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("%s must be true or false", name)
			}
			*target = parsed
		}
	}
	return release.Migrate(ctx, pool, configuration.DatabaseURL, options)
}
