// Package bailian implements the Bailian temporary-key boundary: one request
// per issued provider credential against the workspace-specific Beijing
// endpoint.
package bailian

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/model"
)

const (
	// temporaryKeyPath is the provider's temporary-key API, documented for the
	// Beijing region on the shared DashScope host and served by that region's
	// workspace-specific endpoint as well.
	temporaryKeyPath = "/api/v1/tokens"
	// temporaryKeyParameter carries the requested lifetime in seconds.
	temporaryKeyParameter = "expire_in_seconds"
	// timeout bounds one issuance; a hanging provider must not hold a client
	// request open indefinitely.
	timeout = 10 * time.Second
	// maxResponseBytes bounds how much of a provider answer is read.
	maxResponseBytes = 64 << 10
)

// Client issues temporary keys for one configured parent API key. The parent
// key is the protected server configuration and stays here; only the temporary
// key leaves the service.
type Client struct {
	parentKey  string
	httpClient *http.Client
	// endpoint builds the request URL of one workspace.
	endpoint func(workspaceID string) string
}

// New returns the client for the configured parent API key. That key is
// restricted to the selected model by the provider's own key permissions, so
// the issued temporary key inherits exactly those permissions.
func New(parentKey string) *Client {
	return &Client{
		parentKey:  parentKey,
		httpClient: &http.Client{Timeout: timeout},
		endpoint: func(workspaceID string) string {
			return model.Endpoint(workspaceID, temporaryKeyPath)
		},
	}
}

// issuance is the provider's answer. Only the temporary key and its expiry are
// decoded: a provider message or request identifier the service never reads
// cannot reach a log, a response or an error.
type issuance struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

// TemporaryKey issues one temporary key for workspaceID and reports the
// provider's actual expiry. Every failure is an unusable issuance reported as
// the provider outcome, and no failure carries the parent key, the provider's
// answer or another internal detail.
func (c *Client) TemporaryKey(ctx context.Context, workspaceID string) (model.Credential, error) {
	if c.parentKey == "" {
		return model.Credential{}, fmt.Errorf("%w: the parent API key is not configured",
			model.ErrProvider)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(workspaceID), nil)
	if err != nil {
		return model.Credential{}, fmt.Errorf("%w: build the provider request: %v",
			model.ErrProvider, err)
	}
	request.URL.RawQuery = url.Values{
		temporaryKeyParameter: {strconv.FormatInt(int64(model.TemporaryKeyLifetime.Seconds()), 10)},
	}.Encode()
	request.Header.Set("Authorization", "Bearer "+c.parentKey)

	response, err := c.httpClient.Do(request)
	if err != nil {
		// A transport failure embeds the request URL and can embed the request,
		// which carries the parent key, so only the failure beneath it is
		// reported.
		var transport *url.Error
		if errors.As(err, &transport) {
			err = transport.Err
		}
		return model.Credential{}, fmt.Errorf("%w: provider call failed: %v",
			model.ErrProvider, err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return model.Credential{}, fmt.Errorf("%w: read the provider answer: %v",
			model.ErrProvider, err)
	}
	if response.StatusCode != http.StatusOK {
		return model.Credential{}, fmt.Errorf("%w: provider returned HTTP %d",
			model.ErrProvider, response.StatusCode)
	}

	var answer issuance
	if err := json.Unmarshal(body, &answer); err != nil {
		// The provider's answer is untrusted and may echo the request, so the
		// decoding failure is not repeated.
		return model.Credential{}, fmt.Errorf("%w: provider answer is not a temporary key",
			model.ErrProvider)
	}
	if answer.Token == "" {
		return model.Credential{}, fmt.Errorf("%w: provider answer has no temporary key",
			model.ErrProvider)
	}
	// The provider states the key's real lifetime, which the service reports
	// instead of the lifetime it requested. A key that is already expired
	// cannot be cached usefully, so an unusable expiry is an invalid answer
	// rather than a credential.
	expiresAt := time.Unix(answer.ExpiresAt, 0).UTC()
	if answer.ExpiresAt <= 0 || !expiresAt.After(time.Now().UTC()) {
		return model.Credential{}, fmt.Errorf("%w: provider answer expiry is not in the future",
			model.ErrProvider)
	}

	return model.Credential{APIKey: answer.Token, ExpiresAt: expiresAt}, nil
}
