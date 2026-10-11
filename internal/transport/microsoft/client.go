// Package microsoft implements delegated Microsoft Graph mailbox operations.
package microsoft

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/transport"
)

const RedirectURI = "http://localhost"
const metadata = "id,conversationId,parentFolderId,internetMessageId,subject,bodyPreview,from,sender,toRecipients,ccRecipients,receivedDateTime,sentDateTime,isRead,isDraft,hasAttachments,categories,flag,internetMessageHeaders"

type Token struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}
type User struct{ ID, Mail, UserPrincipalName string }
type Recipient struct {
	EmailAddress struct{ Name, Address string }
}
type Header struct{ Name, Value string }
type Message struct {
	ID, ConversationID, ParentFolderID, InternetMessageID, Subject, BodyPreview string
	From, Sender                                                                Recipient
	ToRecipients, CCRecipients                                                  []Recipient
	ReceivedDateTime, SentDateTime                                              string
	IsRead, IsDraft, HasAttachments                                             bool
	Categories                                                                  []string
	Flag                                                                        struct{ FlagStatus string }
	InternetMessageHeaders                                                      []Header
	Removed                                                                     *struct{ Reason string } `json:"@removed"`
}
type Folder struct {
	ID, DisplayName, ParentFolderID                   string
	ChildFolderCount, TotalItemCount, UnreadItemCount int64
}
type MessagePage struct {
	Messages  []Message `json:"value"`
	NextLink  string    `json:"@odata.nextLink"`
	DeltaLink string    `json:"@odata.deltaLink"`
}
type Client struct {
	HTTP                 *http.Client
	BaseURL, AuthBaseURL string
	mu                   sync.Mutex
	retryAt              time.Time
}

func NewClient(h *http.Client) *Client {
	if h == nil {
		h = &http.Client{Timeout: 45 * time.Second}
	}
	copy := *h
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{HTTP: &copy, BaseURL: "https://graph.microsoft.com/v1.0", AuthBaseURL: "https://login.microsoftonline.com"}
}
func Scope(shared bool) string {
	s := "offline_access User.Read Mail.ReadWrite Mail.Send"
	if shared {
		s += " Mail.ReadWrite.Shared Mail.Send.Shared"
	}
	return s
}
func ValidTenant(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}
func failure(kind, text string, retry bool) error {
	return model.NewMailboxError(kind, "Microsoft: "+text, retry, nil)
}
func (c *Client) token(ctx context.Context, tenant string, v url.Values) (Token, error) {
	var t Token
	if !ValidTenant(tenant) {
		return t, failure(model.ErrKindInvalid, "invalid tenant", false)
	}
	r, e := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.AuthBaseURL, "/")+"/"+tenant+"/oauth2/v2.0/token", strings.NewReader(v.Encode()))
	if e != nil {
		return t, e
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, e := c.HTTP.Do(r)
	if e != nil {
		return t, failure(model.ErrKindUnavailable, "authorization server unavailable", true)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return t, failure(model.ErrKindAuth, "authorization failed; connect again or check tenant consent", false)
	}
	e = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t)
	if e != nil || t.AccessToken == "" || t.ExpiresIn <= 0 {
		return Token{}, failure(model.ErrKindAuth, "incomplete authorization response", false)
	}
	return t, nil
}
func (c *Client) Exchange(ctx context.Context, tenant, id, redirect, code, verifier string) (Token, error) {
	return c.token(ctx, tenant, url.Values{"client_id": {id}, "redirect_uri": {redirect}, "code": {code}, "code_verifier": {verifier}, "grant_type": {"authorization_code"}})
}
func (c *Client) Refresh(ctx context.Context, tenant, id, refresh string) (Token, error) {
	return c.token(ctx, tenant, url.Values{"client_id": {id}, "refresh_token": {refresh}, "grant_type": {"refresh_token"}})
}
func mailboxPath(mailbox string) string { return "/users/" + url.PathEscape(mailbox) }

// continuation retains Microsoft's opaque query but never permits a different
// origin, mailbox, operation, or query-supplied bearer destination.
func (c *Client) continuation(raw, path string) (string, error) {
	u, e := url.Parse(raw)
	base, be := url.Parse(c.BaseURL)
	expected, _ := url.PathUnescape(path)
	// Graph can return equivalent OData key syntax in delta links.
	canonical := func(p string) string {
		return regexp.MustCompile(`/(users|mailFolders)\('([^']+)'\)`).ReplaceAllString(p, "/$1/$2")
	}
	if e != nil || be != nil || u.User != nil || u.Fragment != "" || u.Scheme != base.Scheme || u.Host != base.Host || canonical(u.Path) != base.Path+expected {
		return "", failure(model.ErrKindInvalid, "invalid provider cursor", false)
	}
	return u.String(), nil
}
func (c *Client) request(ctx context.Context, t, method, path, cursor string, body io.Reader, ct string, out any, stream io.Writer, ambiguous bool) error {
	c.mu.Lock()
	blocked := time.Now().Before(c.retryAt)
	c.mu.Unlock()
	if blocked {
		return failure(model.ErrKindRetryable, "provider backoff in progress", true)
	}
	u := strings.TrimRight(c.BaseURL, "/") + path
	if cursor != "" {
		var e error
		u, e = c.continuation(cursor, strings.Split(path, "?")[0])
		if e != nil {
			return e
		}
	}
	r, e := http.NewRequestWithContext(ctx, method, u, body)
	if e != nil {
		return failure(model.ErrKindInvalid, "invalid request", false)
	}
	r.Header.Set("Authorization", "Bearer "+t)
	r.Header.Set("Prefer", `IdType="ImmutableId"`)
	if ct != "" {
		r.Header.Set("Content-Type", ct)
	}
	resp, e := c.HTTP.Do(r)
	if e != nil {
		err := failure(model.ErrKindUnavailable, "request outcome unavailable", true)
		if ambiguous {
			return &transport.AmbiguousError{Err: err}
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == 429 || resp.StatusCode == 503 {
			delay := time.Minute
			if seconds, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil && seconds > 0 {
				delay = time.Duration(seconds) * time.Second
			} else if date, e := http.ParseTime(resp.Header.Get("Retry-After")); e == nil && time.Until(date) > 0 {
				delay = time.Until(date)
			}
			c.mu.Lock()
			c.retryAt = time.Now().Add(delay)
			c.mu.Unlock()
		}
		kind, retry := model.ErrKindInvalid, false
		switch resp.StatusCode {
		case 401:
			kind = model.ErrKindAuth
		case 403:
			kind = model.ErrKindForbidden
		case 404:
			kind = model.ErrKindNotFound
		case 410:
			kind = model.ErrKindConflict
		case 409, 412:
			kind = model.ErrKindConflict
		case 429:
			kind = model.ErrKindRetryable
			retry = true
		default:
			if resp.StatusCode >= 500 {
				kind = model.ErrKindUnavailable
				retry = true
			}
		}
		err := failure(kind, "provider rejected the request", retry)
		if ambiguous && resp.StatusCode >= 500 {
			return &transport.AmbiguousError{Err: err}
		}
		return err
	}
	if stream != nil {
		_, e = io.Copy(stream, resp.Body)
	} else if out != nil {
		e = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
	}
	if e != nil {
		err := failure(model.ErrKindUnavailable, "incomplete provider response", true)
		if ambiguous {
			return &transport.AmbiguousError{Err: err}
		}
		return err
	}
	return nil
}
func (c *Client) json(ctx context.Context, t, method, path string, v, out any, ambiguous bool) error {
	var b io.Reader
	if v != nil {
		raw, e := json.Marshal(v)
		if e != nil {
			return e
		}
		b = bytes.NewReader(raw)
	}
	return c.request(ctx, t, method, path, "", b, "application/json", out, nil, ambiguous)
}
func (c *Client) Identity(ctx context.Context, t string) (User, error) {
	var u User
	e := c.json(ctx, t, "GET", "/me?$select=id,mail,userPrincipalName", nil, &u, false)
	return u, e
}
func (c *Client) Folder(ctx context.Context, t, mailbox, id string) (Folder, error) {
	var f Folder
	e := c.json(ctx, t, "GET", mailboxPath(mailbox)+"/mailFolders/"+url.PathEscape(id), nil, &f, false)
	return f, e
}
func (c *Client) Folders(ctx context.Context, t, mailbox, parent string) ([]Folder, error) {
	path := mailboxPath(mailbox) + "/mailFolders"
	if parent != "" {
		path += "/" + url.PathEscape(parent) + "/childFolders"
	}
	var all []Folder
	cursor := ""
	for n := 0; n < 100; n++ {
		var page struct {
			Value []Folder
			Next  string `json:"@odata.nextLink"`
		}
		if e := c.request(ctx, t, "GET", path+"?$top=100", cursor, nil, "", &page, nil, false); e != nil {
			return nil, e
		}
		all = append(all, page.Value...)
		cursor = page.Next
		if cursor == "" {
			return all, nil
		}
	}
	return nil, failure(model.ErrKindUnavailable, "folder enumeration exceeds bounded pass", true)
}
func (c *Client) Message(ctx context.Context, t, mailbox, id string) (Message, error) {
	var m Message
	e := c.json(ctx, t, "GET", mailboxPath(mailbox)+"/messages/"+url.PathEscape(id)+"?$select="+metadata, nil, &m, false)
	return m, e
}
func (c *Client) Messages(ctx context.Context, t, mailbox, folder, query, cursor string, limit int) (MessagePage, error) {
	path := mailboxPath(mailbox)
	if folder != "" {
		path += "/mailFolders/" + url.PathEscape(folder)
	}
	path += "/messages"
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	v := url.Values{"$select": {metadata}, "$top": {"100"}}
	if query != "" {
		v.Set("$search", query)
	} else {
		v.Set("$orderby", "receivedDateTime desc")
	}
	var p MessagePage
	e := c.request(ctx, t, "GET", path+"?"+v.Encode(), cursor, nil, "", &p, nil, false)
	return p, e
}
func (c *Client) Delta(ctx context.Context, t, mailbox, folder, cursor string) (MessagePage, error) {
	var p MessagePage
	path := mailboxPath(mailbox) + "/mailFolders/" + url.PathEscape(folder) + "/messages/delta"
	e := c.request(ctx, t, "GET", path+"?$select="+metadata+"&$top=100", cursor, nil, "", &p, nil, false)
	return p, e
}
func (c *Client) Raw(ctx context.Context, t, mailbox, id string, w io.Writer) error {
	return c.request(ctx, t, "GET", mailboxPath(mailbox)+"/messages/"+url.PathEscape(id)+"/$value", "", nil, "", nil, w, false)
}
func (c *Client) Patch(ctx context.Context, t, mailbox, id string, v map[string]any) (Message, error) {
	var m Message
	e := c.json(ctx, t, "PATCH", mailboxPath(mailbox)+"/messages/"+url.PathEscape(id), v, &m, false)
	return m, e
}
func (c *Client) Move(ctx context.Context, t, mailbox, id, dest string) (Message, error) {
	var m Message
	e := c.json(ctx, t, "POST", mailboxPath(mailbox)+"/messages/"+url.PathEscape(id)+"/move", map[string]string{"destinationId": dest}, &m, false)
	return m, e
}
func (c *Client) Delete(ctx context.Context, t, mailbox, id string) error {
	return c.json(ctx, t, "POST", mailboxPath(mailbox)+"/messages/"+url.PathEscape(id)+"/permanentDelete", nil, nil, false)
}
func (c *Client) CreateFolder(ctx context.Context, t, mailbox, parent, name string) (Folder, error) {
	path := mailboxPath(mailbox) + "/mailFolders"
	if parent != "" {
		path += "/" + url.PathEscape(parent) + "/childFolders"
	}
	var f Folder
	e := c.json(ctx, t, "POST", path, map[string]string{"displayName": name}, &f, false)
	return f, e
}
func (c *Client) RenameFolder(ctx context.Context, t, mailbox, id, name string) (Folder, error) {
	var f Folder
	e := c.json(ctx, t, "PATCH", mailboxPath(mailbox)+"/mailFolders/"+url.PathEscape(id), map[string]string{"displayName": name}, &f, false)
	return f, e
}
func (c *Client) DeleteFolder(ctx context.Context, t, mailbox, id string) error {
	return c.json(ctx, t, "DELETE", mailboxPath(mailbox)+"/mailFolders/"+url.PathEscape(id), nil, nil, false)
}
func (c *Client) CreateDraft(ctx context.Context, t, mailbox string, raw io.Reader) (Message, error) {
	b, e := io.ReadAll(io.LimitReader(raw, (30<<20)+1))
	if e != nil || len(b) > 30<<20 {
		return Message{}, failure(model.ErrKindInvalid, "message exceeds size limit or cannot be read", false)
	}
	path := mailboxPath(mailbox) + "/messages"
	if msg, e := mail.ReadMessage(bytes.NewReader(b)); e == nil && msg.Header.Get("In-Reply-To") != "" {
		ref := msg.Header.Get("In-Reply-To")
		v := url.Values{"$filter": {"internetMessageId eq '" + strings.ReplaceAll(ref, "'", "''") + "'"}, "$select": {"id"}, "$top": {"2"}}
		var page MessagePage
		if e = c.request(ctx, t, "GET", path+"?"+v.Encode(), "", nil, "", &page, nil, false); e != nil {
			return Message{}, e
		}
		if len(page.Messages) == 1 && page.NextLink == "" {
			path += "/" + url.PathEscape(page.Messages[0].ID) + "/createReply"
		}
	}
	return c.mimeDraft(ctx, t, path, bytes.NewReader(b))
}
func (c *Client) CreateReply(ctx context.Context, t, mailbox, id string, raw io.Reader) (Message, error) {
	return c.mimeDraft(ctx, t, mailboxPath(mailbox)+"/messages/"+url.PathEscape(id)+"/createReply", raw)
}
func (c *Client) mimeDraft(ctx context.Context, t, path string, raw io.Reader) (Message, error) {
	b, e := io.ReadAll(io.LimitReader(raw, (30<<20)+1))
	if e != nil || len(b) > 30<<20 {
		return Message{}, failure(model.ErrKindInvalid, "message exceeds size limit or cannot be read", false)
	}
	var m Message
	e = c.request(ctx, t, "POST", path, "", strings.NewReader(base64.StdEncoding.EncodeToString(b)), "text/plain", &m, nil, true)
	return m, e
}
func (c *Client) SendDraft(ctx context.Context, t, mailbox, id string) error {
	return c.json(ctx, t, "POST", mailboxPath(mailbox)+"/messages/"+url.PathEscape(id)+"/send", nil, nil, true)
}
