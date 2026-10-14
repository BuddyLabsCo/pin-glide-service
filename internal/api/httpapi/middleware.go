package httpapi

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
)

// withRequestID assigns every operation a correlation identifier and exposes it
// to logs and problem details. Request bodies, headers and credentials are
// never read here.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestID := observability.NewRequestID()
		writer.Header().Set(observability.RequestIDHeader, requestID)
		ctx := observability.WithRequestID(request.Context(), requestID)
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

// withCredential exposes the Authorization header of the current request to the
// handlers that resolve their own credential. The header name is stated once;
// the value is never logged or copied elsewhere.
func withCredential(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		credential := request.Header.Get(observability.CredentialHeader)
		next.ServeHTTP(writer, request.WithContext(observability.WithCredential(request.Context(), credential)))
	})
}

// withAccessLog records one completion line per request. It logs the method,
// path, status, duration and request ID only, so payloads, Authorization and
// provider secrets cannot reach the logs from here.
func withAccessLog(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		status := &statusRecorder{ResponseWriter: writer}
		started := time.Now()
		next.ServeHTTP(status, request)
		logger.InfoContext(request.Context(), "http request",
			slog.String("request_id", observability.RequestIDFrom(request.Context())),
			slog.String("method", request.Method),
			slog.String("path", request.URL.Path),
			slog.Int("status", status.Code()),
			slog.Int64("duration_ms", time.Since(started).Milliseconds()),
		)
	})
}

// withRecovery turns a panic in the composed chain into the standard internal
// error. Only the panic type and stack reach the logs; the value itself never
// reaches the logs or the client, so its contents cannot leak.
func withRecovery(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			logger.ErrorContext(request.Context(), "recovered from panic",
				slog.String("request_id", observability.RequestIDFrom(request.Context())),
				slog.String("panic_type", fmt.Sprintf("%T", recovered)),
				slog.String("stack", string(debug.Stack())),
			)
			writeProblem(writer, request, NewProblem(request.Context(), protocol.InternalError, ""))
		}()
		next.ServeHTTP(writer, request)
	})
}

// withAdmission rejects requests once shutdown has closed the admission gate,
// so draining work cannot be extended by new callers.
func withAdmission(next http.Handler, admission *Admission) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		release, admitted := admission.Begin()
		if !admitted {
			writeProblem(writer, request, NewProblem(request.Context(), protocol.ServiceUnavailable, ""))
			return
		}
		// The release travels in the context so work that outlives its
		// transport, such as an upgraded pairing connection, can hold it until
		// that work really ends.
		releaseOnce := sync.OnceFunc(release)
		defer releaseOnce()
		ctx := WithSessionAdmission(request.Context(), releaseOnce)
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

// withNoStore keeps credential-bearing responses of the v1 surface out of
// caches. A route that can prove a weaker policy can relax it later.
func withNoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/v1/") {
			writer.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(writer, request)
	})
}

// writeProblem writes a problem detail with its actual status. It is used by
// middleware and routing fallbacks that sit outside Huma's error handling.
func writeProblem(writer http.ResponseWriter, request *http.Request, problem *Problem) {
	problem.RequestID = observability.RequestIDFrom(request.Context())
	writer.Header().Set("Content-Type", "application/problem+json")
	writer.WriteHeader(problem.Status)
	_ = json.NewEncoder(writer).Encode(problem)
}

// statusRecorder remembers the response status for the access log. Unwrap keeps
// response-controller features such as hijacking and deadlines available to
// handlers that upgrade a connection.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

// Code returns the response status, defaulting to 200 like net/http.
func (r *statusRecorder) Code() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
