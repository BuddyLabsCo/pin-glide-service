package wss

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
)

// PairingChannel serves one authenticated pairing subscription until its
// connection closes. It returns the failure of a subscription the service
// rejected before upgrading, so the router owns reporting it.
type PairingChannel interface {
	Serve(writer http.ResponseWriter, request *http.Request, sessionID string, release func()) error
}

// ReceiverChannel serves one R-authenticated delivery subscription until its
// connection closes, reporting a refused subscription like PairingChannel.
type ReceiverChannel interface {
	Serve(writer http.ResponseWriter, request *http.Request, release func()) error
}

// Router registers the WSS surface on the HTTP mux. Both channels keep their
// own upgrade, authentication and frame handling; this package owns only where
// they answer and what one refused subscription reports.
type Router struct {
	// Pairing serves the restricted pairing channel; a nil value leaves its
	// route unregistered.
	Pairing PairingChannel
	// Receiver serves the R-authenticated delivery channel; a nil value leaves
	// its route unregistered.
	Receiver ReceiverChannel
	// Release returns the session admission release of the current request, so
	// a connection that outlives its transport holds that admission until the
	// work is really finished.
	Release func(ctx context.Context) func()
	// Reject writes the shared problem document of one subscription this
	// surface refused before upgrading, and reports the code it published so
	// the refusal is logged with what the client receives.
	Reject func(writer http.ResponseWriter, request *http.Request, reason error) protocol.ErrorCode
	Logger *slog.Logger
}

// Register routes both channels on mux. An upgrade is a plain GET handler
// rather than a Huma operation because a hijacked connection has no JSON
// response Huma could describe, and protocol/asyncapi.yaml states each route's
// credential and message exchange.
func Register(mux *http.ServeMux, router Router) {
	if router.Logger == nil {
		router.Logger = slog.New(slog.NewTextHandler(discardOutput{}, nil))
	}
	if router.Pairing != nil {
		mux.HandleFunc("GET /v1/pairing-sessions/{id}/events", func(writer http.ResponseWriter, request *http.Request) {
			sessionID := request.PathValue("id")
			err := router.Pairing.Serve(writer, request, sessionID, router.release(request.Context()))
			if err != nil {
				router.reject(writer, request, "pairing subscription rejected", err,
					slog.String("session_id", sessionID))
			}
		})
	}
	if router.Receiver != nil {
		mux.HandleFunc("GET /v1/receiver/events", func(writer http.ResponseWriter, request *http.Request) {
			err := router.Receiver.Serve(writer, request, router.release(request.Context()))
			if err != nil {
				router.reject(writer, request, "receiver subscription rejected", err)
			}
		})
	}
}

// release returns the admission release of one request. A request that reached
// the route without the admission middleware has nothing to release, so it
// reports a no-op.
func (r Router) release(ctx context.Context) func() {
	if r.Release != nil {
		if release := r.Release(ctx); release != nil {
			return release
		}
	}
	return func() {}
}

// reject answers one subscription the service refused before upgrading with the
// shared problem document, then reports the refusal with the code the client
// received and the channel's own attributes.
func (r Router) reject(writer http.ResponseWriter, request *http.Request, message string, reason error, attributes ...any) {
	code := r.Reject(writer, request, reason)
	attributes = append([]any{
		slog.String("request_id", observability.RequestIDFrom(request.Context())),
		slog.String("code", string(code)),
	}, attributes...)
	r.Logger.WarnContext(request.Context(), message, attributes...)
}
