// Package api composes the Pin Glide service process: the composed HTTP
// surface and its bounded cancellation and shutdown lifecycle.
package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/api/httpapi"
	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
)

// shutdownTimeout is the total window in which admitted work must finish after
// the process is asked to stop.
const shutdownTimeout = 5 * time.Second

// Dependencies are the external dependencies of one api run.
type Dependencies struct {
	Database *sql.DB
	Logger   *slog.Logger
	// WeChat verifies login authorization codes; the web adapter satisfies it
	// and tests substitute a fake at this external boundary.
	WeChat identity.Provider
}

// Serve runs the composed API on listener until ctx is canceled or serving
// fails. On cancellation it stops admitting work, drains within the bounded
// shutdown window and joins the serving goroutine before returning.
func Serve(ctx context.Context, listener net.Listener, dependencies Dependencies) error {
	handler, admission := httpapi.New(httpapi.Dependencies{
		Database: dependencies.Database,
		Logger:   dependencies.Logger,
		Identity: identity.NewService(dependencies.Database, dependencies.WeChat),
	})
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	served := make(chan error, 1)
	go func() {
		served <- server.Serve(listener)
	}()

	select {
	case err := <-served:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
		return drain(server, admission)
	}
}

// drain closes admission, stops the server and joins work that outlives its
// transport inside one bounded window. Both errors are reported when the
// window expires with work still running.
func drain(server *http.Server, admission *httpapi.Admission) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	admission.Close()
	shutdownErr := server.Shutdown(ctx)
	// Work that outlives its transport, such as future WSS connections and
	// admitted Deliveries, still holds admission until it finishes.
	admissionErr := admission.Wait(ctx)
	if err := errors.Join(shutdownErr, admissionErr); err != nil {
		return fmt.Errorf("shut down api server: %w", err)
	}
	return nil
}
