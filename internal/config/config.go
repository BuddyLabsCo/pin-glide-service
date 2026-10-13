// Package config loads process configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Environment selects the deployment environment a process runs in.
type Environment string

const (
	// Development runs against local, disposable infrastructure.
	Development Environment = "development"
	// Production runs against the deployed database.
	Production Environment = "production"
)

const defaultDatabaseURL = "postgres://pin_glide:pin_glide@127.0.0.1:5432/pin_glide?sslmode=disable"

// Config is the process configuration for one api run.
type Config struct {
	Environment Environment
	APIAddress  string
	DatabaseURL string
}

// Load reads process configuration from getenv. Development carries defaults so
// a fresh checkout needs no credentials; production requires an explicit
// database URL so the service cannot silently attach to another database.
func Load(getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	environment := Environment(strings.TrimSpace(getenv("PIN_GLIDE_ENV")))
	if environment == "" {
		environment = Development
	}
	if environment != Development && environment != Production {
		return Config{}, fmt.Errorf("invalid PIN_GLIDE_ENV %q: want %s or %s", environment, Development, Production)
	}

	apiAddress := strings.TrimSpace(getenv("PIN_GLIDE_API_ADDRESS"))
	if apiAddress == "" {
		apiAddress = ":8080"
	}

	databaseURL := strings.TrimSpace(getenv("PIN_GLIDE_DATABASE_URL"))
	if databaseURL == "" {
		if environment == Production {
			return Config{}, errors.New("PIN_GLIDE_DATABASE_URL is required in production")
		}
		databaseURL = defaultDatabaseURL
	}

	return Config{
		Environment: environment,
		APIAddress:  apiAddress,
		DatabaseURL: databaseURL,
	}, nil
}
