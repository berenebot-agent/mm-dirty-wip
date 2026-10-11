package gmail

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/transport/netutil"
)

// Production endpoints. BaseURL is the Gmail REST host; the per-user API prefix
// is appended by Client.endpoint. TokenURL is Google's OAuth 2.0 token
// endpoint.
const (
	DefaultBaseURL  = "https://gmail.googleapis.com"
	DefaultTokenURL = "https://oauth2.googleapis.com/token"

	// usersPrefix is the Gmail REST prefix for the authenticated user.
	usersPrefix = "/gmail/v1/users/me"

	// maxResponseBytes bounds a JSON response body so a hostile or corrupt
	// endpoint cannot exhaust memory. Raw and attachment payloads are bounded by
	// the same limit before base64 decoding.
	maxResponseBytes = 64 << 20

	// maxMessageBytes bounds an uploaded MIME message before base64 expansion.
	maxMessageBytes = 64 << 20
)

// Client is a Gmail API and OAuth client. HTTP, BaseURL and TokenURL are public
// so a caller (or a test) can inject a custom *http.Client and deterministic
// endpoint URLs. The production defaults are the fixed Google endpoints.
type Client struct {
	HTTP     *http.Client
	BaseURL  string
	TokenURL string
}

// NewClient builds a Client using the supplied HTTP client and the production
// Google endpoints. A nil client falls back to the shared guarded outbound
// client (see netutil.HTTPClient).
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = netutil.HTTPClient()
	}
	return &Client{HTTP: httpClient, BaseURL: DefaultBaseURL, TokenURL: DefaultTokenURL}
}

func (c *Client) httpClient() *http.Client {
	var h *http.Client
	if c != nil && c.HTTP != nil {
		h = c.HTTP
	} else {
		h = http.DefaultClient
	}
	copy := *h
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}

func (c *Client) baseURL() string {
	b := strings.TrimRight(c.BaseURL, "/")
	if b == "" {
		b = DefaultBaseURL
	}
	return b
}

func (c *Client) tokenEndpoint() string {
	u := strings.TrimRight(c.TokenURL, "/")
	if u == "" {
		u = DefaultTokenURL
	}
	return u
}

// endpoint builds a per-user Gmail API URL for path (which must begin with "/").
func (c *Client) endpoint(path string) string {
	return c.baseURL() + usersPrefix + path
}

// Token is an OAuth 2.0 token response.
type Token struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	TokenType    string `json:"token_type"`
}

// Exchange trades an authorization code for tokens. verifier is the PKCE
// code_verifier and is omitted from the request when empty.
func (c *Client) Exchange(ctx context.Context, clientID, clientSecret, redirectURI, code, verifier string) (Token, error) {
	if strings.TrimSpace(clientID) == "" || strings.TrimSpace(code) == "" {
		return Token{}, invalid("gmail oauth: client id and authorization code are required")
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
	}
	if verifier != "" {
		form.Set("code_verifier", verifier)
	}
	return c.tokenRequest(ctx, form)
}

// Refresh trades a refresh token for a fresh access token.
func (c *Client) Refresh(ctx context.Context, clientID, clientSecret, refreshToken string) (Token, error) {
	if strings.TrimSpace(clientID) == "" || strings.TrimSpace(refreshToken) == "" {
		return Token{}, invalid("gmail oauth: client id and refresh token are required")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	}
	return c.tokenRequest(ctx, form)
}

func (c *Client) tokenRequest(ctx context.Context, form url.Values) (Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenEndpoint(), strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, model.NewMailboxError(model.ErrKindInternal, "gmail oauth: could not build request", false, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return Token{}, networkError("gmail oauth", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Token{}, errorForResponse("gmail oauth", resp)
	}
	var tok Token
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&tok); err != nil {
		return Token{}, model.NewMailboxError(model.ErrKindInternal, "gmail oauth: invalid token response", false, err)
	}
	if tok.AccessToken == "" && tok.RefreshToken == "" {
		return Token{}, model.NewMailboxError(model.ErrKindAuth, "gmail oauth: token response carried no credentials", false, nil)
	}
	return tok, nil
}

// do issues an authenticated JSON request. A non-2xx response is normalized to
// a *model.MailboxError; the response body is not retained in the error.
func (c *Client) do(ctx context.Context, op, method, endpoint, token string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, model.NewMailboxError(model.ErrKindInternal, op+": could not build request", false, err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, networkError(op, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, errorForResponse(op, resp)
	}
	return resp, nil
}

func (c *Client) getJSON(ctx context.Context, op, endpoint, token string, out any) error {
	resp, err := c.do(ctx, op, http.MethodGet, endpoint, token, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeJSON(op, resp, out)
}

func (c *Client) postJSON(ctx context.Context, op, endpoint, token string, body, out any) error {
	return c.sendJSON(ctx, op, http.MethodPost, endpoint, token, body, out)
}

func (c *Client) patchJSON(ctx context.Context, op, endpoint, token string, body, out any) error {
	return c.sendJSON(ctx, op, http.MethodPatch, endpoint, token, body, out)
}

func (c *Client) sendJSON(ctx context.Context, op, method, endpoint, token string, body, out any) error {
	var r io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return model.NewMailboxError(model.ErrKindInternal, op+": could not encode request", false, err)
		}
		r = bytes.NewReader(encoded)
	}
	resp, err := c.do(ctx, op, method, endpoint, token, r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeJSON(op, resp, out)
}

func (c *Client) delete(ctx context.Context, op, endpoint, token string) error {
	resp, err := c.do(ctx, op, http.MethodDelete, endpoint, token, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

func decodeJSON(op string, resp *http.Response, out any) error {
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
		return model.NewMailboxError(model.ErrKindInternal, op+": invalid Gmail response", false, err)
	}
	return nil
}
