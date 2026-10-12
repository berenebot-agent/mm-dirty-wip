package gmail

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// UserInfo is Google's basic account profile, not a Gmail mailbox profile.
// Name is optional even when profile permission is granted.
type UserInfo struct {
	Name string `json:"name"`
}

// Identity fetches basic profile data using the OAuth access token. It bypasses
// mailbox executors: identity access belongs to direct Google OAuth, not One.
func (c *Client) Identity(ctx context.Context, token string) (UserInfo, error) {
	// Optional naming must not hold up the mailbox connection indefinitely.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	endpoint := c.UserInfoURL
	if endpoint == "" {
		endpoint = DefaultUserInfoURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return UserInfo{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return UserInfo{}, networkError("google userinfo", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return UserInfo{}, errorForResponse("google userinfo", resp)
	}
	var info UserInfo
	if err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&info); err != nil {
		return UserInfo{}, err
	}
	return info, nil
}
