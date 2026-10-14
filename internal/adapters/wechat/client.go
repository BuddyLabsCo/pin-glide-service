// Package wechat implements the WeChat identity boundary: one authorization
// code exchange against the configured approved mobile application.
package wechat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
)

const (
	// endpoint is the WeChat Open Platform authorization-code exchange.
	endpoint = "https://api.weixin.qq.com/sns/oauth2/access_token"
	// timeout bounds one exchange; a hanging provider must not hold a request
	// open indefinitely.
	timeout = 10 * time.Second
	// maxResponseBytes bounds how much of a provider answer is read.
	maxResponseBytes = 64 << 10
	// invalidCode and usedCode are provider errcodes that mean the caller's
	// authorization is unusable; every other errcode is a provider failure.
	invalidCode = 40029
	usedCode    = 40163
)

// Client verifies authorization codes for one configured application.
type Client struct {
	appID      string
	appSecret  string
	httpClient *http.Client
	endpoint   string
}

// New returns the client for the configured approved mobile application.
func New(appID, appSecret string) *Client {
	return &Client{
		appID:      appID,
		appSecret:  appSecret,
		httpClient: &http.Client{Timeout: timeout},
		endpoint:   endpoint,
	}
}

// exchange is the provider answer. Access and refresh tokens are deliberately
// not decoded: the service needs only the verified identity, and a value it
// never reads cannot reach a log, a response or the database.
type exchange struct {
	ErrCode int    `json:"errcode"`
	OpenID  string `json:"openid"`
	UnionID string `json:"unionid"`
}

// Verify exchanges one authorization code and reports the verified identity of
// the configured application.
func (c *Client) Verify(ctx context.Context, wechatCode string) (identity.WeChatIdentity, error) {
	if c.appID == "" || c.appSecret == "" {
		return identity.WeChatIdentity{}, fmt.Errorf("%w: WeChat credentials are not configured",
			identity.ErrIdentityProvider)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return identity.WeChatIdentity{}, fmt.Errorf("%w: build the provider request: %v",
			identity.ErrIdentityProvider, err)
	}
	request.URL.RawQuery = url.Values{
		"appid":      {c.appID},
		"secret":     {c.appSecret},
		"code":       {wechatCode},
		"grant_type": {"authorization_code"},
	}.Encode()

	response, err := c.httpClient.Do(request)
	if err != nil {
		// The request URL carries the code and the AppSecret, so only the
		// transport failure itself is reported.
		return identity.WeChatIdentity{}, fmt.Errorf("%w: provider call failed: %v",
			identity.ErrIdentityProvider, err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return identity.WeChatIdentity{}, fmt.Errorf("%w: read the provider answer: %v",
			identity.ErrIdentityProvider, err)
	}
	if response.StatusCode != http.StatusOK {
		return identity.WeChatIdentity{}, fmt.Errorf("%w: provider returned HTTP %d",
			identity.ErrIdentityProvider, response.StatusCode)
	}

	var answer exchange
	if err := json.Unmarshal(body, &answer); err != nil {
		return identity.WeChatIdentity{}, fmt.Errorf("%w: provider answer is not JSON",
			identity.ErrIdentityProvider)
	}
	if answer.ErrCode == invalidCode || answer.ErrCode == usedCode {
		return identity.WeChatIdentity{}, fmt.Errorf("%w: provider errcode %d",
			identity.ErrWeChatAuthorizationInvalid, answer.ErrCode)
	}
	if answer.ErrCode != 0 {
		// The numeric code names the provider failure; its message can restate
		// the request and stays out of every error.
		return identity.WeChatIdentity{}, fmt.Errorf("%w: provider errcode %d",
			identity.ErrIdentityProvider, answer.ErrCode)
	}
	if answer.OpenID == "" {
		return identity.WeChatIdentity{}, fmt.Errorf("%w: provider answer has no OpenID",
			identity.ErrIdentityProvider)
	}

	return identity.WeChatIdentity{
		AppID:   c.appID,
		OpenID:  answer.OpenID,
		UnionID: answer.UnionID,
	}, nil
}
