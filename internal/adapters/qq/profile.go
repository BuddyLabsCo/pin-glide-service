package qq

import (
	"context"
	"net/url"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
)

// profile ignores failed or incomplete answers so login keeps the known cache.
// QQ marks partially lost data as unsuitable for caching with is_lost=1.
func (c *Client) profile(ctx context.Context, accessToken, openID string) *identity.DisplayProfile {
	var answer struct {
		Ret      *int   `json:"ret"`
		IsLost   int    `json:"is_lost"`
		Nickname string `json:"nickname"`
		Avatar   string `json:"figureurl_qq_2"`
		Small    string `json:"figureurl_qq_1"`
	}
	if err := c.getJSON(ctx, c.profileEndpoint, url.Values{
		"access_token": {accessToken}, "oauth_consumer_key": {c.appID}, "openid": {openID},
	}, &answer); err != nil {
		return nil
	}
	if answer.Ret == nil || *answer.Ret != 0 || answer.IsLost != 0 || answer.Nickname == "" {
		return nil
	}
	avatar := answer.Avatar
	if avatar == "" {
		avatar = answer.Small
	}
	parsed, err := url.Parse(avatar)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil {
		return nil
	}
	return &identity.DisplayProfile{Nickname: answer.Nickname, AvatarURL: avatar}
}
