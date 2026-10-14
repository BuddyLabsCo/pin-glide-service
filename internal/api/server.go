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
	"github.com/BuddyLabsCo/pin-glide-service/internal/api/wss"
	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
	"github.com/BuddyLabsCo/pin-glide-service/internal/pairing"
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
	// Presence is the Receiver liveness this process publishes. A nil value
	// composes the private registry of this run; a caller that also reads
	// presence supplies the registry the delivery channel writes.
	Presence *wss.Presence
}

// Serve runs the composed API on listener until ctx is canceled or serving
// fails. On cancellation it stops admitting work, drains within the bounded
// shutdown window and joins the serving goroutine before returning.
func Serve(ctx context.Context, listener net.Listener, dependencies Dependencies) error {
	service := &server{
		hub: wss.NewHub(),
	}

	identityService := identity.NewService(dependencies.Database, dependencies.WeChat)
	pairingService := pairing.NewService(
		dependencies.Database,
		pairing.NewProvider(dependencies.WeChat, identityService),
		pairing.NewAuthenticator(identityService),
	)
	socket := wss.NewPairingSocket(pairingService, service.hub, dependencies.Logger)
	pairingService.SetNotifier(socket)
	presence := dependencies.Presence
	if presence == nil {
		presence = wss.NewPresence(time.Now().UTC())
	}
	receiverSocket := wss.NewReceiverSocket(pairingService, presence, dependencies.Logger)
	service.receivers = receiverSocket

	handler, admission := httpapi.New(httpapi.Dependencies{
		Database:       dependencies.Database,
		Logger:         dependencies.Logger,
		Identity:       identityService,
		Pairing:        pairingService,
		PairingEvents:  socket,
		ReceiverEvents: receiverSocket,
	})
	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	service.http = httpServer

	served := make(chan error, 1)
	go func() {
		served <- httpServer.Serve(listener)
	}()

	select {
	case err := <-served:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
		return service.drain(admission)
	}
}

// server is one composed api run: the HTTP server and the live pairing
// connections that outlive their transport.
type server struct {
	hub       *wss.Hub
	receivers *wss.ReceiverSocket
	http      *http.Server
}

// drain closes admission, stops the server, ends every live pairing
// connection and joins work that outlives its transport inside one bounded
// window.
func (s *server) drain(admission *httpapi.Admission) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	admission.Close()
	shutdownErr := s.http.Shutdown(ctx)
	// A pairing connection outlives its transport, so shutdown ends it here
	// rather than waiting for a reader that would block until it failed.
	s.hub.Close()
	// The delivery channel has the same shape, so its connections end here too.
	s.receivers.Close()
	// A pairing connection holds its admission until it closes, so this wait
	// also joins the connections shutdown just ended.
	admissionErr := admission.Wait(ctx)
	if err := errors.Join(shutdownErr, admissionErr); err != nil {
		return fmt.Errorf("shut down api server: %w", err)
	}
	return nil
}
