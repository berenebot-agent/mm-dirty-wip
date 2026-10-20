// Package one implements the backend-only One vault and Gmail passthrough API.
// It is deliberately not a generic URL proxy or an OAuth token exchange.
package one

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/transport"
	"github.com/dellarb/mailmoose/internal/transport/gmail"
	"github.com/dellarb/mailmoose/internal/transport/netutil"
)

const Origin = "https://api.withone.ai"
const prefix = "/gmail/v1/users/{{userId}}"
const responseLimit = 64 << 20

type Connection struct {
	ID          string `json:"id"`
	Key         string `json:"-"`
	Platform    string `json:"platform"`
	Identity    string `json:"identity"`
	Active      bool   `json:"active"`
	State       string `json:"state"`
	Environment string `json:"environment"`
	Name        string `json:"name"`
}

func (c *Connection) UnmarshalJSON(b []byte) error {
	type plain Connection
	var v struct {
		plain
		Key string `json:"key"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*c = Connection(v.plain)
	c.Key = v.Key
	return nil
}

func (c Connection) Usable() bool {
	return c.Platform == "gmail" && c.Active && c.State == "operational" && c.ID != "" && c.Key != ""
}

type Action struct {
	ID     string `json:"systemId"`
	Method string `json:"method"`
	Path   string `json:"path"`
}

// Operations is the exact native Gmail allowlist. Provider IDs are discovered,
// not guessed. No permanent message delete, arbitrary host or convenience action.
func Operations() []Action {
	var out []Action
	for _, op := range []string{
		"GET /profile", "GET /messages", "GET /messages/{{id}}", "GET /history", "GET /labels",
		"POST /labels", "PATCH /labels/{{id}}", "DELETE /labels/{{id}}",
		"GET /messages/{{messageId}}/attachments/{{id}}", "POST /messages/{{id}}/modify",
		"POST /messages/{{id}}/trash", "POST /messages/{{id}}/untrash", "GET /drafts", "POST /drafts",
		"GET /drafts/{{id}}", "PUT /drafts/{{id}}", "DELETE /drafts/{{id}}", "POST /drafts/send", "POST /messages/send",
	} {
		method, path, _ := strings.Cut(op, " ")
		out = append(out, Action{Method: method, Path: prefix + path})
	}
	return out
}

type backgroundKey struct{}
type bindingCheckKey struct{}

// Background gives explicit UI/API requests priority over sync waiters.
func Background(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundKey{}, true)
}

// Limiter is shared by every operation for an owning One configuration. A key
// rotation must retain it: One quotas belong to the account, not the key.
type Limiter struct {
	mu       sync.Mutex
	next     time.Time
	changed  chan struct{}
	waiters  []*waiter
	Interval time.Duration // 0 uses 4 seconds (15/min); tests may inject a clock rate.
}

type waiter struct{ background bool }

func (l *Limiter) notify() {
	if l.changed != nil {
		close(l.changed)
	}
	l.changed = make(chan struct{})
}

func (l *Limiter) Wait(ctx context.Context) error {
	w := &waiter{background: ctx.Value(backgroundKey{}) == true}
	l.mu.Lock()
	l.waiters = append(l.waiters, w)
	l.notify()
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		for i, pending := range l.waiters {
			if pending == w {
				l.waiters = append(l.waiters[:i], l.waiters[i+1:]...)
				l.notify()
				break
			}
		}
	}()
	for {
		l.mu.Lock()
		first := l.waiters[0]
		for _, pending := range l.waiters {
			if !pending.background {
				first = pending
				break
			}
		}
		delay := time.Until(l.next)
		if ctx.Err() == nil && first == w && delay <= 0 {
			interval := l.Interval
			if interval <= 0 {
				interval = 4 * time.Second
			}
			l.next = time.Now().Add(interval)
			l.mu.Unlock()
			return nil
		}
		changed := l.changed
		l.mu.Unlock()
		if delay <= 0 || first != w {
			delay = time.Hour
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return safe(model.ErrKindRetryable, "One request queued; no request was submitted", true)
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (l *Limiter) Backoff(d time.Duration) {
	if d < time.Minute {
		d = time.Minute
	}
	// Small jitter prevents simultaneously resumed accounts from hammering One.
	d += time.Duration(time.Now().UnixNano() % int64(3*time.Second))
	l.mu.Lock()
	defer l.mu.Unlock()
	if until := time.Now().Add(d); until.After(l.next) {
		l.next = until
	}
	l.notify()
}

type Client struct {
	http             *http.Client
	secret           string
	limiter          *Limiter
	mu               sync.Mutex
	actions          map[string]Action
	senderNameAction *Action
}

func NewClient(secret string, h *http.Client, limiter *Limiter) *Client {
	if h == nil {
		h = netutil.HTTPClient()
	}
	copy := *h
	copy.Timeout = 30 * time.Second
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if limiter == nil {
		limiter = &Limiter{}
	}
	return &Client{http: &copy, secret: secret, limiter: limiter}
}

func safe(kind, message string, retry bool) error {
	return model.NewMailboxError(kind, message, retry, nil)
}

func (c *Client) request(ctx context.Context, method, path, actionID, selector string, body io.Reader) (*http.Response, error) {
	if c.secret == "" || strings.ContainsAny(c.secret+selector, "\r\n") {
		return nil, safe(model.ErrKindAuth, "One credentials unavailable; test or replace the key", false)
	}
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	if check, ok := ctx.Value(bindingCheckKey{}).(func(context.Context) error); ok {
		if err := check(ctx); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, Origin+path, body)
	if err != nil {
		return nil, safe(model.ErrKindInvalid, "Invalid One operation", false)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-One-Secret", c.secret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if actionID != "" {
		req.Header.Set("X-One-Connection-Key", selector)
		req.Header.Set("X-One-Action-Id", actionID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		e := safe(model.ErrKindUnavailable, "Cannot reach One; local data is unchanged", true)
		if method != "GET" && method != "DELETE" {
			return nil, &transport.AmbiguousError{Err: e}
		}
		return nil, e
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Bound reads even when the caller is not the Gmail JSON decoder.
		resp.Body = &boundedBody{ReadCloser: resp.Body, remaining: responseLimit + 1}
		return resp, nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	var e struct {
		Error struct {
			Code   int    `json:"code"`
			Status string `json:"status"`
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	_ = json.Unmarshal(b, &e)
	if resp.StatusCode == 429 {
		d := time.Minute
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
			d = time.Duration(min(seconds, 86400)) * time.Second
		} else if date, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
			d = min(time.Until(date), 24*time.Hour)
		}
		c.limiter.Backoff(d)
		return nil, safe(model.ErrKindRetryable, "One rate limit reached; operations remain queued", true)
	}
	if actionID != "" && e.Error.Code == resp.StatusCode && e.Error.Status != "" {
		for _, detail := range e.Error.Errors {
			if detail.Reason == "rateLimitExceeded" || detail.Reason == "userRateLimitExceeded" {
				c.limiter.Backoff(time.Minute)
				return nil, safe(model.ErrKindRetryable, "Gmail rate limit reached; operations remain queued", true)
			}
		}
		if resp.StatusCode >= 500 && method == "GET" {
			c.limiter.Backoff(time.Minute)
		}
		if resp.StatusCode == 401 {
			return nil, safe(model.ErrKindAuth, "One connection revoked or expired; reconnect the original account in One", false)
		}
		if resp.StatusCode == 403 {
			return nil, safe(model.ErrKindForbidden, "One permission missing; reconnect in One and grant the required mailbox permissions", false)
		}
		if resp.StatusCode >= 500 && method != "GET" && method != "DELETE" {
			return nil, &transport.AmbiguousError{Err: safe(model.ErrKindUnavailable, "Gmail write outcome unknown; do not retry automatically", false)}
		}
		// Gmail's native envelope is the only source allowed to trigger the
		// existing Gmail expired-history behaviour. One/unknown 404s fail closed.
		resp.Body = io.NopCloser(bytes.NewReader(b))
		return resp, nil
	}
	switch resp.StatusCode {
	case 401, 403:
		return nil, safe(model.ErrKindAuth, "One rejected access; test the key and reconnect in One", false)
	case 404:
		return nil, safe(model.ErrKindUnavailable, "One connection or action unavailable; refresh and revalidate actions", false)
	case 402:
		return nil, safe(model.ErrKindQuota, "One account quota exhausted; review the One dashboard", false)
	}
	if resp.StatusCode >= 500 && method != "GET" && method != "DELETE" {
		return nil, &transport.AmbiguousError{Err: safe(model.ErrKindUnavailable, "One write outcome unknown; do not retry automatically", false)}
	}
	if resp.StatusCode >= 500 {
		c.limiter.Backoff(time.Minute)
	}
	return nil, safe(model.ErrKindUnavailable, "One request failed; local data is unchanged", resp.StatusCode >= 500)
}

type boundedBody struct {
	io.ReadCloser
	remaining int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, safe(model.ErrKindInvalid, "One response exceeds size limit", false)
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	resp, err := c.request(ctx, "GET", path, "", "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return safe(model.ErrKindUnavailable, "One returned invalid discovery data", false)
	}
	return nil
}

func (c *Client) Connections(ctx context.Context) ([]Connection, error) {
	out := []Connection{}
	for page := 1; page <= 100; page++ {
		var result struct {
			Page  int          `json:"page"`
			Pages int          `json:"pages"`
			Rows  []Connection `json:"rows"`
		}
		if err := c.get(ctx, "/v1/vault/connections?limit=100&page="+strconv.Itoa(page), &result); err != nil {
			return nil, err
		}
		if page == 1 && result.Page == 1 && result.Pages == 0 && result.Rows != nil && len(result.Rows) == 0 {
			return out, nil
		}
		if result.Page != page || result.Pages < page || result.Pages > 100 || result.Rows == nil {
			return nil, safe(model.ErrKindUnavailable, "One returned invalid connection pagination", false)
		}
		out = append(out, result.Rows...)
		if page == result.Pages {
			return out, nil
		}
	}
	return nil, safe(model.ErrKindUnavailable, "One discovery exceeds safe pagination limit", false)
}

// ValidateActions atomically replaces the mapping only after every required
// native operation has exactly one match. Existing mappings never silently drift.
func (c *Client) ValidateActions(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	mapping := make(map[string]Action)
	var senderNameAction *Action
	senderNameMatches := 0
	for page := 1; page <= 100; page++ {
		var result struct {
			Page  int      `json:"page"`
			Pages int      `json:"pages"`
			Rows  []Action `json:"rows"`
		}
		if err := c.get(ctx, "/v1/available-actions/gmail?limit=150&page="+strconv.Itoa(page), &result); err != nil {
			return err
		}
		if result.Page != page || result.Pages < page || result.Pages > 100 || result.Rows == nil {
			return safe(model.ErrKindUnavailable, "One returned invalid action pagination", false)
		}
		for _, a := range result.Rows {
			if a.Method == "GET" && a.Path == prefix+"/settings/sendAs" {
				senderNameMatches++
				if a.ID != "" && !strings.ContainsAny(a.ID, "\r\n") {
					copy := a
					senderNameAction = &copy
				}
			}
			for _, wanted := range Operations() {
				if a.Method == wanted.Method && a.Path == wanted.Path {
					key := a.Method + " " + a.Path
					if _, duplicate := mapping[key]; duplicate || a.ID == "" || strings.ContainsAny(a.ID, "\r\n") {
						return safe(model.ErrKindUnsupported, "One Gmail action mapping is ambiguous; revalidation required", false)
					}
					mapping[key] = a
				}
			}
		}
		if page == result.Pages {
			break
		}
	}
	if len(mapping) != len(Operations()) {
		return safe(model.ErrKindUnsupported, "One lacks a mandatory Gmail action; connector unavailable", false)
	}
	c.actions = mapping
	c.senderNameAction = nil
	if senderNameMatches == 1 {
		c.senderNameAction = senderNameAction
	}
	return nil
}

// SenderName resolves the configured Gmail display name for the already
// verified mailbox address. This optional action may lack settings permission.
// Never treat another alias's display name as the mailbox identity.
func (c *Client) SenderName(ctx context.Context, selector, email string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	name, settingsErr := c.settingsSenderName(ctx, selector, email)
	if name != "" {
		return name, nil
	}
	// Gmail can inherit the Google account name while returning an empty
	// sendAs displayName. Sent metadata exposes the name actually used, without
	// requiring a Google userinfo token or downloading message bodies.
	client := c.Gmail(selector, nil, nil)
	page, err := client.List(ctx, "", "", "SENT", "", 3)
	if err != nil {
		return "", err
	}
	for i, item := range page.Messages {
		if i >= 3 {
			break
		}
		message, err := client.Get(ctx, "", item.ID, "metadata")
		if err != nil || message.Payload == nil {
			continue
		}
		for _, header := range message.Payload.Headers {
			if !strings.EqualFold(header.Name, "From") {
				continue
			}
			sender, err := mail.ParseAddress(header.Value)
			if err == nil && strings.EqualFold(sender.Address, email) {
				name := strings.TrimSpace(sender.Name)
				if name != "" && len(name) <= 256 {
					return name, nil
				}
			}
		}
	}
	return "", settingsErr
}

func (c *Client) settingsSenderName(ctx context.Context, selector, email string) (string, error) {
	if err := c.ensureActions(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	action := c.senderNameAction
	c.mu.Unlock()
	if action == nil {
		return "", safe(model.ErrKindUnsupported, "One Gmail sender-name lookup unavailable", false)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := c.request(ctx, "GET", "/v1/passthrough/gmail/v1/users/me/settings/sendAs", action.ID, selector, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", safe(model.ErrKindUnsupported, "Gmail sender-name lookup unavailable", false)
	}
	var result struct {
		SendAs []struct {
			Email string `json:"sendAsEmail"`
			Name  string `json:"displayName"`
		} `json:"sendAs"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&result); err != nil {
		return "", safe(model.ErrKindUnavailable, "Invalid Gmail sender identity", false)
	}
	for _, sender := range result.SendAs {
		if strings.EqualFold(strings.TrimSpace(sender.Email), email) {
			name := strings.TrimSpace(sender.Name)
			if len(name) <= 256 {
				return name, nil
			}
		}
	}
	return "", nil
}

func (c *Client) ensureActions(ctx context.Context) error {
	c.mu.Lock()
	ok := c.actions != nil
	c.mu.Unlock()
	if ok {
		return nil
	}
	return c.ValidateActions(ctx)
}

type binding struct {
	client   *Client
	selector string
	check    func(context.Context) error
	status   func(context.Context, error)
}

// Gmail binds a backend-resolved selector to the existing Gmail client. Check
// runs before every operation to stop removed/rotated bindings, including queued
// sends; status persists only safe health classifications supplied by the app.
func (c *Client) Gmail(selector string, check func(context.Context) error, status func(context.Context, error)) *gmail.Client {
	client := gmail.NewClient(nil)
	client.Executor = &binding{client: c, selector: selector, check: check, status: status}
	return client
}

func (b *binding) Execute(ctx context.Context, op string, req *http.Request) (resp *http.Response, err error) {
	if b.check != nil {
		if err = b.check(ctx); err != nil {
			return nil, err
		}
	}
	if b.check != nil {
		ctx = context.WithValue(ctx, bindingCheckKey{}, b.check)
	}
	if b.status != nil {
		defer func() { b.status(ctx, err) }()
	}
	if req.URL.Scheme != "https" || req.URL.Host != "gmail.googleapis.com" || req.URL.User != nil || req.URL.Fragment != "" {
		return nil, safe(model.ErrKindForbidden, "One accepts only known Gmail operations", false)
	}
	if err = b.client.ensureActions(ctx); err != nil {
		return nil, err
	}
	path := req.URL.EscapedPath()
	var matched *Action
	b.client.mu.Lock()
	for _, a := range b.client.actions {
		if a.Method == req.Method && matches(a.Path, path) {
			copy := a
			if matched != nil {
				b.client.mu.Unlock()
				return nil, safe(model.ErrKindForbidden, "Ambiguous One Gmail operation", false)
			}
			matched = &copy
		}
	}
	b.client.mu.Unlock()
	if matched == nil {
		return nil, safe(model.ErrKindForbidden, "One Gmail operation not allowed", false)
	}
	if req.URL.RawQuery != "" {
		query := req.URL.Query()
		if matched.Path == prefix+"/history" {
			query.Set("maxResults", "5")
		}
		path += "?" + query.Encode()
	}
	return b.client.request(ctx, req.Method, "/v1/passthrough"+path, matched.ID, b.selector, req.Body)
}

func matches(template, path string) bool {
	t := strings.Split(template, "/")
	p := strings.Split(path, "/")
	if len(t) != len(p) {
		return false
	}
	for i, segment := range t {
		decoded, err := url.PathUnescape(p[i])
		if err != nil || strings.ContainsAny(decoded, "/\\\r\n") || decoded == "." || decoded == ".." {
			return false
		}
		if segment == "{{userId}}" {
			if decoded != "me" {
				return false
			}
		} else if strings.HasPrefix(segment, "{{") {
			if decoded == "" {
				return false
			}
		} else if decoded != segment {
			return false
		}
	}
	return true
}
