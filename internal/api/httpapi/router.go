// Package httpapi composes the HTTP surface of the service: Huma routes,
// middleware, the shared problem-details error model and the routing fallbacks.
package httpapi

import (
	"database/sql"
	"io"
	"log/slog"
	"net/http"

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
	Logger   *slog.Logger
}

// New builds the composed HTTP handler and its admission gate. The gate closes
// during shutdown so in-flight work drains inside a bounded window.
func New(dependencies Dependencies) (http.Handler, *Admission) {
	logger := dependencies.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	mux := http.NewServeMux()
	newAPI(mux, dependencies.Database, logger)

	admission := NewAdmission()
	handler := withFallback(mux)
	handler = withNoStore(handler)
	handler = withAdmission(handler, admission)
	handler = withAccessLog(handler, logger)
	handler = withRecovery(handler, logger)
	handler = withRequestID(handler)
	return handler, admission
}

// newAPI registers the service operations on mux with the shared error model.
func newAPI(mux *http.ServeMux, database *sql.DB, logger *slog.Logger) huma.API {
	configureErrorModel()
	api := humago.New(mux, apiConfig())
	registerHealth(api, database, logger)
	return api
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
	return config
}

// withFallback answers requests that match no registered route with the shared
// problem model instead of the standard library's plain-text responses.
func withFallback(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handler, pattern := mux.Handler(request)
		if pattern != "" {
			handler.ServeHTTP(writer, request)
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
