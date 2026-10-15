// Package httpapi composes the HTTP surface of the service: Huma routes,
// middleware, the shared problem-details error model and the routing fallbacks.
package httpapi

import (
	"database/sql"
	"io"
	"log/slog"
	"net/http"

	deliveryapi "github.com/BuddyLabsCo/pin-glide-service/internal/api/httpapi/delivery"
	identityapi "github.com/BuddyLabsCo/pin-glide-service/internal/api/httpapi/identity"
	modelapi "github.com/BuddyLabsCo/pin-glide-service/internal/api/httpapi/model"
	pairingapi "github.com/BuddyLabsCo/pin-glide-service/internal/api/httpapi/pairing"
	"github.com/BuddyLabsCo/pin-glide-service/internal/delivery"
	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
	"github.com/BuddyLabsCo/pin-glide-service/internal/model"
	"github.com/BuddyLabsCo/pin-glide-service/internal/pairing"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/danielgtaylor/huma/v2"
	humago "github.com/danielgtaylor/huma/v2/adapters/humago"
)

const (
	apiTitle   = "Pin Glide Service API"
	apiVersion = "0.1.0"
)

// Dependencies are the external dependencies of the HTTP API.
type Dependencies struct {
	Database *sql.DB
	Identity *identity.Service
	Model    *model.Service
	Pairing  *pairing.Service
	Delivery *delivery.Service
	// Presence reports each Receiver's published liveness, which a Sender's
	// own Receiver list states; it is the transport's own process-local state.
	Presence pairingapi.ReceiverPresence
	Logger   *slog.Logger
}

// New builds the composed HTTP handler and its admission gate around mux. The
// WSS channels register their own routes on that mux, so one chain of
// middleware, one admission gate and one problem mapping answer both surfaces.
// The gate closes during shutdown so in-flight work drains inside a bounded
// window.
func New(mux *http.ServeMux, dependencies Dependencies) (http.Handler, *Admission) {
	dependencies.Logger = loggerOf(dependencies.Logger)

	newAPI(mux, dependencies)

	admission := NewAdmission()
	handler := withFallback(mux)
	handler = withNoStore(handler)
	handler = withCredential(handler)
	handler = withAdmission(handler, admission)
	handler = withAccessLog(handler, dependencies.Logger)
	handler = withRecovery(handler, dependencies.Logger)
	handler = withRequestID(handler)
	return handler, admission
}

// loggerOf keeps the composed surface working without diagnostics of its own.
func loggerOf(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return discardLogger()
	}
	return logger
}

// discardLogger returns the logger of a surface that only produces a document:
// nothing it does may reach the diagnostics of the caller that asked for it.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newAPI registers the service operations on mux with the shared error model.
func newAPI(mux *http.ServeMux, dependencies Dependencies) huma.API {
	configureErrorModel()
	api := humago.New(mux, apiConfig())
	registerSecuritySchemes(api)
	registerHealth(api, dependencies.Database, dependencies.Logger)
	identityapi.Register(api, dependencies.Identity, dependencies.Pairing)
	modelapi.Register(api, dependencies.Model)
	deliveryapi.Register(api, dependencies.Delivery)
	pairingapi.Register(api, pairingapi.Handlers{
		Sessions: dependencies.Pairing,
		Senders:  dependencies.Pairing,
		Presence: dependencies.Presence,
	})
	return api
}

// registerSecuritySchemes declares the bearer scheme every protected operation
// requires. The authority a credential carries differs per operation, so each
// operation states which credential it accepts.
func registerSecuritySchemes(api huma.API) {
	components := api.OpenAPI().Components
	if components.SecuritySchemes == nil {
		components.SecuritySchemes = map[string]*huma.SecurityScheme{}
	}
	components.SecuritySchemes["bearerAuth"] = &huma.SecurityScheme{
		Type:        "http",
		Scheme:      "bearer",
		Description: "One service credential in the Authorization header. The operation states which credential it accepts.",
	}
}

// apiConfig returns the Huma configuration for the shared HTTP contract. The
// default link transformer is removed because it injects $schema fields and
// Link headers that the contract does not define. Huma's schema, documentation
// and per-schema routes stay disabled because the published contract is an
// offline-generated release artifact rather than a route of the running
// process, which serves only its documented operations.
func apiConfig() huma.Config {
	config := huma.DefaultConfig(apiTitle, apiVersion)
	config.CreateHooks = nil
	config.Transformers = nil
	config.OpenAPIPath = ""
	config.DocsPath = ""
	config.SchemasPath = ""
	config.Info.Description = "The Pin Glide service HTTP surface. Whole-request failures carry an RFC 9457 problem detail with a stable machine-readable code; credentials belong in the Authorization header, never in a URL."
	config.Servers = []*huma.Server{{
		URL:         "https://{serviceHost}",
		Description: "The public origin supplied by the installation.",
		Variables: map[string]*huma.ServerVariable{
			"serviceHost": {
				Default:     "localhost:8080",
				Description: "Host and port of the public HTTPS origin.",
			},
		},
	}}
	return config
}

// withFallback answers requests that match no registered route with the shared
// problem model instead of the standard library's plain-text responses.
func withFallback(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handler, pattern := mux.Handler(request)
		if pattern != "" {
			// Dispatch through the mux rather than the handler it returned:
			// only ServeMux binds the path values a matched pattern declares.
			mux.ServeHTTP(writer, request)
			return
		}

		status, allow := routeStatus(handler, request)
		switch status {
		case http.StatusNotFound:
			writeProblem(writer, request, NewProblem(request.Context(), protocol.ResourceNotFound, ""))
		case http.StatusMethodNotAllowed:
			if allow != "" {
				writer.Header().Set("Allow", allow)
			}
			writeProblem(writer, request, NewProblem(request.Context(), protocol.MethodNotAllowed, ""))
		default:
			// Not a missing route: for example a canonical-path redirect keeps
			// the standard library's response.
			handler.ServeHTTP(writer, request)
		}
	})
}

// routeStatus asks the standard library's unmatched-request handler which
// status it would send, preserving its 404/405 distinction and Allow set. The
// default handlers write a short plain-text body, so probing is cheap.
func routeStatus(handler http.Handler, request *http.Request) (int, string) {
	probe := &statusProbe{}
	handler.ServeHTTP(probe, request)
	if probe.status == 0 {
		return http.StatusNotFound, ""
	}
	return probe.status, probe.header.Get("Allow")
}

// statusProbe records the status and headers of a response without its body.
type statusProbe struct {
	status int
	header http.Header
}

func (p *statusProbe) Header() http.Header {
	if p.header == nil {
		p.header = http.Header{}
	}
	return p.header
}

func (p *statusProbe) WriteHeader(status int) {
	if p.status == 0 {
		p.status = status
	}
}

func (p *statusProbe) Write(data []byte) (int, error) {
	if p.status == 0 {
		p.status = http.StatusOK
	}
	return len(data), nil
}
