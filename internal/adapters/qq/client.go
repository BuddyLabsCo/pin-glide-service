// Package qq implements the QQ Connect identity boundary for the mobile
// server-side authorization-code flow.
package qq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
)

const (
	tokenEndpoint  = "https://graph.qq.com/oauth2.0/token"
	openIDEndpoint = "https://graph.qq.com/oauth2.0/me"
	timeout        = 10 * time.Second
	maxBodyBytes   = 64 << 10
)

// Client verifies authorization codes for one configured QQ mobile
// application.
type Client struct {
	appID          string
	appKey         string
	httpClient     *http.Client
	tokenEndpoint  string
	openIDEndpoint string
}

// New returns the client for the configured QQ mobile application.
func New(appID, appKey string) *Client {
	return &Client{
		appID:          appID,
		appKey:         appKey,
		httpClient:     &http.Client{Timeout: timeout},
		tokenEndpoint:  tokenEndpoint,
		openIDEndpoint: openIDEndpoint,
	}
}

// Kind reports the provider this adapter verifies.
func (c *Client) Kind() identity.ProviderKind {
	return identity.ProviderQQ
}

// Verify exchanges one authorization code, resolves its OpenID and checks that
// the provider bound it to this configured application.
func (c *Client) Verify(ctx context.Context, code string) (identity.Identity, error) {
	if c.appID == "" || c.appKey == "" {
		return identity.Identity{}, fmt.Errorf("%w: QQ credentials are not configured",
			identity.ErrIdentityProvider)
	}

	accessToken, err := c.exchange(ctx, code)
	if err != nil {
		return identity.Identity{}, err
	}
	openID, err := c.openID(ctx, accessToken)
	if err != nil {
		return identity.Identity{}, err
	}
	return identity.Identity{
		Provider: identity.ProviderQQ,
		AppID:    c.appID,
		OpenID:   openID,
	}, nil
}

type tokenAnswer struct {
	AccessToken string `json:"access_token"`
	Error       int    `json:"error"`
}

type openIDAnswer struct {
	ClientID string `json:"client_id"`
	OpenID   string `json:"openid"`
	Error    int    `json:"error"`
}

// exchange performs the mobile authorization-code exchange. The mobile flow
// omits redirect_uri and asks for JSON because no website callback is involved.
func (c *Client) exchange(ctx context.Context, code string) (string, error) {
	var answer tokenAnswer
	query := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {c.appID},
		"client_secret": {c.appKey},
		"code":          {code},
		"fmt":           {"json"},
	}
	if err := c.getJSON(ctx, c.tokenEndpoint, query, &answer); err != nil {
		return "", err
	}
	if answer.Error != 0 {
		return "", authorizationOutcome(answer.Error)
	}
	if answer.AccessToken == "" {
		return "", fmt.Errorf("%w: QQ token answer has no access token",
			identity.ErrIdentityProvider)
	}
	return answer.AccessToken, nil
}

// openID resolves the identity and verifies the provider returned binding to
// the configured application.
func (c *Client) openID(ctx context.Context, accessToken string) (string, error) {
	var answer openIDAnswer
	if err := c.getJSON(ctx, c.openIDEndpoint, url.Values{
		"access_token": {accessToken},
		"fmt":          {"json"},
	}, &answer); err != nil {
		return "", err
	}
	if answer.Error != 0 {
		return "", authorizationOutcome(answer.Error)
	}
	if answer.ClientID != c.appID {
		return "", fmt.Errorf("%w: QQ OpenID is bound to another application",
			identity.ErrIdentityProvider)
	}
	if answer.OpenID == "" {
		return "", fmt.Errorf("%w: QQ OpenID answer has no OpenID",
			identity.ErrIdentityProvider)
	}
	return answer.OpenID, nil
}

// getJSON performs one provider GET and decodes only the fields the caller
// asked for. Provider text and exchanged values never enter an error.
func (c *Client) getJSON(ctx context.Context, endpoint string, query url.Values, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("%w: build the QQ provider request: %v",
			identity.ErrIdentityProvider, err)
	}
	request.URL.RawQuery = query.Encode()

	response, err := c.httpClient.Do(request)
	if err != nil {
		var transport *url.Error
		if errors.As(err, &transport) {
			err = transport.Err
		}
		return fmt.Errorf("%w: QQ provider call failed: %v",
			identity.ErrIdentityProvider, err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("%w: read the QQ provider answer: %v",
			identity.ErrIdentityProvider, err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: QQ provider returned HTTP %d",
			identity.ErrIdentityProvider, response.StatusCode)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("%w: QQ provider answer is not JSON",
			identity.ErrIdentityProvider)
	}
	return nil
}

// authorizationOutcome distinguishes an authorization the caller must obtain
// again from a provider failure.
func authorizationOutcome(code int) error {
	switch code {
	case 100019, 100020, 100067, 100068, // token exchange, reuse, expiry and invalid code
		100013, 100014, 100015, 100016, 100058, 100063: // unusable access token or OpenID
		return fmt.Errorf("%w: QQ provider error %d",
			identity.ErrQQAuthorizationInvalid, code)
	default:
		return fmt.Errorf("%w: QQ provider error %d",
			identity.ErrIdentityProvider, code)
	}
}
