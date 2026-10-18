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
	"github.com/BuddyLabsCo/pin-glide-service/internal/snapshot"
)

const usage = "usage: pin-glide-db-tools <migrate|snapshot>"

func main() {
	if len(os.Args) != 2 || (os.Args[1] != "migrate" && os.Args[1] != "snapshot") {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}

	var err error
	if os.Args[1] == "migrate" {
		err = migrate()
	} else {
		err = createSnapshot()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "pin-glide-db-tools %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func createSnapshot() error {
	keep, err := strconv.Atoi(os.Getenv("PIN_GLIDE_SNAPSHOT_KEEP"))
	if err != nil || keep <= 0 {
		return fmt.Errorf("PIN_GLIDE_SNAPSHOT_KEEP must be a positive integer")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	path, err := snapshot.Snapshot(ctx, snapshot.Options{
		DatabaseURL: os.Getenv("PIN_GLIDE_DATABASE_URL"), Path: os.Getenv("PIN_GLIDE_SNAPSHOT_PATH"), Keep: keep,
	})
	if err != nil {
		return err
	}
	fmt.Printf("pin-glide-db-tools snapshot: created %s\n", path)
	return nil
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
