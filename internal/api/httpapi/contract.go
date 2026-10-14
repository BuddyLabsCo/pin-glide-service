package httpapi

import (
	"fmt"
	"io"
	"net/http"
)

// Domain tags group operations by the service area they belong to. Every
// operation carries exactly one, and the values are part of the published
// contract, so a new tag means a new domain rather than a renamed one.
const (
	// TagSystem covers service-level operations such as health.
	TagSystem = "system"
)

// descriptionHeader marks the published artifact as generated, so a maintainer
// changes the registrations instead of the checked-in YAML.
const descriptionHeader = "# Generated from the service's Huma registrations; do not edit.\n"

// WriteOpenAPI writes the published HTTP description to writer as YAML. The
// description is derived from the same Huma registrations the composed service
// answers, so it cannot announce a route the service does not serve, and it
// needs neither a listener nor a database.
func WriteOpenAPI(writer io.Writer) error {
	mux := http.NewServeMux()
	// The export composes the same registrations without runtime dependencies:
	// it serves nothing, so the operations are described, not called.
	api := newAPI(mux, Dependencies{Logger: discardLogger()})

	encoded, err := api.OpenAPI().YAML()
	if err != nil {
		return fmt.Errorf("encode HTTP description: %w", err)
	}
	if _, err := writer.Write(append([]byte(descriptionHeader), encoded...)); err != nil {
		return fmt.Errorf("write HTTP description: %w", err)
	}
	return nil
}
