package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/danielgtaylor/huma/v2"
)

// healthTimeout bounds the database check so health answers even while the
// database hangs rather than fails fast.
const healthTimeout = 2 * time.Second

// healthOutput is the exact accepted health success response.
type healthOutput struct {
	Body struct {
		Status string `json:"status" example:"ok" doc:"ok when the service and its database are available"`
	}
}

// registerHealth exposes the anonymous sanitized health operation. The response
// never carries SQL, credentials, environment names or failure details; the
// cause stays in the logs with the request identifier.
func registerHealth(api huma.API, database *sql.DB, logger *slog.Logger) {
	huma.Register(api, huma.Operation{
		OperationID: "GetHealth",
		Tags:        []string{TagSystem},
		Method:      http.MethodGet,
		Path:        "/health",
		Summary:     "Report sanitized service health",
		Description: "Reports ok while the service and its database are available, and a sanitized 503 otherwise.",
		Errors:      []int{http.StatusServiceUnavailable},
		// An empty requirement states that health is deliberately anonymous; a
		// later credential-protected operation cannot inherit it silently.
		Security: []map[string][]string{},
	}, func(ctx context.Context, _ *struct{}) (*healthOutput, error) {
		if err := databaseReady(ctx, database); err != nil {
			logger.WarnContext(ctx, "health check failed",
				slog.String("request_id", observability.RequestIDFrom(ctx)),
				slog.String("cause", err.Error()),
			)
			return nil, NewProblem(ctx, protocol.ServiceUnavailable, "")
		}
		output := &healthOutput{}
		output.Body.Status = "ok"
		return output, nil
	})
}

// databaseReady reports whether the database answers within the health bound.
func databaseReady(ctx context.Context, database *sql.DB) error {
	if database == nil {
		return errors.New("database is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	return database.PingContext(ctx)
}
