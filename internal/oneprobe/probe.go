// Package oneprobe implements the opt-in One/Gmail compatibility gate. It is
// developer tooling, not a connector or an application-accessible proxy.
package oneprobe

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dellarb/mammothmail/internal/transport/netutil"
)

const origin = "https://api.withone.ai"
const prefix = "/gmail/v1/users/{{userId}}"
const responseLimit = 4 << 20

// Options deliberately has no configurable origin. Tests can inject an HTTP
// transport without weakening the production destination or redirect policy.
type Options struct {
	APIKey       string
	ConnectionID string
	WriteDrafts  bool
	SendApproved bool
	SendTo       string
	HTTP         *http.Client
	// Wait is an injectable clock wait for deterministic fixtures. The CLI
	// always leaves it nil, enforcing 15 requests/minute for live runs.
	Wait func(context.Context, time.Duration) error
}

// Result contains only safe operation metadata, never credentials, identities,
// message IDs, cursors, payloads or arbitrary remote error messages.
type Result struct {
	Operation   string            `json:"operation"`
	Method      string            `json:"method"`
	Path        string            `json:"path"`
	ActionID    string            `json:"action_id,omitempty"`
	Status      int               `json:"status"`
	LatencyMS   int64             `json:"latency_ms"`
	Headers     map[string]string `json:"headers,omitempty"`
	ErrorSource string            `json:"error_source,omitempty"`
}

type Report struct {
	Results          []Result `json:"results"`
	Environment      string   `json:"environment,omitempty"`
	IdentityVerified bool     `json:"identity_verified"`
	IdentityMatches  bool     `json:"vault_identity_matches_profile"`
	ReadPassed       bool     `json:"read_passed"`
	DraftPassed      bool     `json:"draft_passed"`
	SendPassed       bool     `json:"send_passed"`
	ReleaseGate      bool     `json:"release_gate"`
	CleanupRequired  bool     `json:"cleanup_required"`
}

type connection struct {
	ID          string `json:"id"`
	Key         string `json:"key"`
	Platform    string `json:"platform"`
	Identity    string `json:"identity"`
	Active      bool   `json:"active"`
	State       string `json:"state"`
	Environment string `json:"environment"`
}

type action struct {
	ID     string `json:"systemId"`
	Method string `json:"method"`
	Path   string `json:"path"`
}

// The gate checks all operations the current Gmail client uses, plus draft
// lifecycle and sending required by the specification. It excludes permanent
// message deletion and other unrelated catalogue actions.
var required = []action{
	{Method: "GET", Path: prefix + "/profile"},
	{Method: "GET", Path: prefix + "/messages"},
	{Method: "GET", Path: prefix + "/messages/{{id}}"},
	{Method: "GET", Path: prefix + "/history"},
	{Method: "GET", Path: prefix + "/labels"},
	{Method: "POST", Path: prefix + "/labels"},
	{Method: "PATCH", Path: prefix + "/labels/{{id}}"},
	{Method: "DELETE", Path: prefix + "/labels/{{id}}"},
	{Method: "GET", Path: prefix + "/messages/{{messageId}}/attachments/{{id}}"},
	{Method: "POST", Path: prefix + "/messages/{{id}}/modify"},
	{Method: "POST", Path: prefix + "/messages/{{id}}/trash"},
	{Method: "POST", Path: prefix + "/messages/{{id}}/untrash"},
	{Method: "GET", Path: prefix + "/drafts"},
	{Method: "POST", Path: prefix + "/drafts"},
	{Method: "GET", Path: prefix + "/drafts/{{id}}"},
	{Method: "PUT", Path: prefix + "/drafts/{{id}}"},
	{Method: "DELETE", Path: prefix + "/drafts/{{id}}"},
	{Method: "POST", Path: prefix + "/drafts/send"},
	{Method: "POST", Path: prefix + "/messages/send"},
}

type probe struct {
	opts       Options
	http       *http.Client
	report     Report
	connection connection
	actions    map[string]action
	next       time.Time
}

// Run never retries a request, particularly a non-idempotent write. Draft writes
// and sends require independent explicit opt-ins. Requests are paced at 15/min.
func Run(ctx context.Context, opts Options) (report Report, err error) {
	if opts.APIKey == "" || strings.ContainsAny(opts.APIKey, "\r\n") {
		return report, fmt.Errorf("ONE_API_KEY is required")
	}
	if opts.SendApproved && (!opts.WriteDrafts || opts.SendTo == "") {
		return report, fmt.Errorf("sending requires draft gate and an explicitly approved recipient")
	}
	h := opts.HTTP
	if h == nil {
		h = netutil.HTTPClient()
	}
	copy := *h
	copy.Timeout = 30 * time.Second
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	p := &probe{opts: opts, http: &copy, actions: make(map[string]action)}
	err = p.run(ctx)
	return p.report, err
}

func (p *probe) request(ctx context.Context, op, method, path, safePath, actionID string, body any, out any) error {
	if delay := time.Until(p.next); delay > 0 {
		wait := p.opts.Wait
		if wait == nil {
			wait = waitFor
		}
		if wait(ctx, delay) != nil {
			return fmt.Errorf("probe canceled before %s", op)
		}
	}
	p.next = time.Now().Add(4 * time.Second)
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("could not encode %s", op)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, origin+path, reader)
	if err != nil {
		return fmt.Errorf("could not build %s", op)
	}
	req.Header.Set("X-One-Secret", p.opts.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if actionID != "" {
		req.Header.Set("X-One-Connection-Key", p.connection.Key)
		req.Header.Set("X-One-Action-Id", actionID)
	}
	start := time.Now()
	resp, err := p.http.Do(req)
	r := Result{Operation: op, Method: method, Path: safePath, ActionID: actionID, LatencyMS: time.Since(start).Milliseconds()}
	if err != nil {
		p.report.Results = append(p.report.Results, r)
		return fmt.Errorf("%s network failure; outcome may be unknown; do not blindly retry writes", op)
	}
	defer resp.Body.Close()
	r.Status = resp.StatusCode
	r.Headers = make(map[string]string)
	for _, name := range []string{"Retry-After", "X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "Content-Type"} {
		if v := resp.Header.Get(name); v != "" {
			r.Headers[name] = v
		}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, responseLimit+1))
	if err != nil || len(b) > responseLimit {
		p.report.Results = append(p.report.Results, r)
		return fmt.Errorf("%s response unreadable or too large", op)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Code int `json:"code"`
			} `json:"error"`
			Key string `json:"key"`
		}
		_ = json.Unmarshal(b, &e)
		r.ErrorSource = "unknown"
		if e.Error.Code == resp.StatusCode {
			r.ErrorSource = "gmail"
		} else if e.Key != "" {
			r.ErrorSource = "one"
		}
		p.report.Results = append(p.report.Results, r)
		return fmt.Errorf("%s rejected: HTTP %d (%s)", op, resp.StatusCode, r.ErrorSource)
	}
	p.report.Results = append(p.report.Results, r)
	if out != nil && json.Unmarshal(b, out) != nil {
		return fmt.Errorf("%s invalid JSON", op)
	}
	return nil
}

func waitFor(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (p *probe) call(ctx context.Context, op, method, template string, params map[string]string, query url.Values, body, out any) error {
	a, ok := p.actions[method+" "+prefix+template]
	if !ok {
		return fmt.Errorf("%s has no verified action", op)
	}
	path := strings.ReplaceAll(a.Path, "{{userId}}", "me")
	for name, value := range params {
		if value == "" || strings.ContainsAny(value, "/\\\r\n") || value == "." || value == ".." {
			return fmt.Errorf("%s invalid resource ID", op)
		}
		path = strings.ReplaceAll(path, "{{"+name+"}}", url.PathEscape(value))
	}
	if strings.Contains(path, "{{") {
		return fmt.Errorf("%s unresolved path parameter", op)
	}
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return p.request(ctx, op, method, "/v1/passthrough"+path, a.Path, a.ID, body, out)
}

func (p *probe) discover(ctx context.Context) error {
	var connections []connection
	for page := 1; page <= 100; page++ {
		var result struct {
			Page  int          `json:"page"`
			Pages int          `json:"pages"`
			Rows  []connection `json:"rows"`
		}
		path := "/v1/vault/connections?platform=gmail&limit=100&page=" + strconv.Itoa(page)
		if err := p.request(ctx, "vault.list", "GET", path, "/v1/vault/connections", "", nil, &result); err != nil {
			return err
		}
		if result.Page != page || result.Pages < page || result.Pages > 100 || result.Rows == nil {
			return fmt.Errorf("invalid vault pagination")
		}
		connections = append(connections, result.Rows...)
		if page == result.Pages {
			break
		}
	}
	var candidates []connection
	for _, c := range connections {
		if c.Platform == "gmail" && c.Active && c.State == "operational" && c.ID != "" && c.Key != "" && (p.opts.ConnectionID == "" || p.opts.ConnectionID == c.ID) {
			candidates = append(candidates, c)
		}
	}
	if len(candidates) != 1 {
		return fmt.Errorf("need exactly one usable Gmail connection; select ONE_CONNECTION_ID explicitly when multiple are available")
	}
	p.connection = candidates[0]
	p.report.Environment = p.connection.Environment
	for page := 1; page <= 100; page++ {
		var result struct {
			Page  int      `json:"page"`
			Pages int      `json:"pages"`
			Rows  []action `json:"rows"`
		}
		path := "/v1/available-actions/gmail?limit=150&page=" + strconv.Itoa(page)
		if err := p.request(ctx, "actions.list", "GET", path, "/v1/available-actions/gmail", "", nil, &result); err != nil {
			return err
		}
		if result.Page != page || result.Pages < page || result.Pages > 100 || result.Rows == nil {
			return fmt.Errorf("invalid action pagination")
		}
		for _, a := range result.Rows {
			for _, wanted := range required {
				if a.Method == wanted.Method && a.Path == wanted.Path {
					key := a.Method + " " + a.Path
					if _, exists := p.actions[key]; exists || a.ID == "" {
						return fmt.Errorf("ambiguous or invalid action for %s", key)
					}
					p.actions[key] = a
				}
			}
		}
		if page == result.Pages {
			break
		}
	}
	for _, a := range required {
		if _, ok := p.actions[a.Method+" "+a.Path]; !ok {
			return fmt.Errorf("mandatory action missing: %s %s", a.Method, a.Path)
		}
	}
	return nil
}

type message struct {
	ID       string   `json:"id"`
	ThreadID string   `json:"threadId"`
	Raw      string   `json:"raw"`
	LabelIDs []string `json:"labelIds"`
	Payload  *part    `json:"payload"`
}

type part struct {
	Filename string `json:"filename"`
	Body     struct {
		AttachmentID string `json:"attachmentId"`
		Data         string `json:"data"`
		Size         int    `json:"size"`
	} `json:"body"`
	Parts []part `json:"parts"`
}

type draft struct {
	ID      string  `json:"id"`
	Message message `json:"message"`
}

func (p *probe) run(ctx context.Context) error {
	if err := p.discover(ctx); err != nil {
		return err
	}
	var profile struct {
		EmailAddress string `json:"emailAddress"`
		HistoryID    string `json:"historyId"`
	}
	if err := p.call(ctx, "profile", "GET", "/profile", nil, nil, nil, &profile); err != nil {
		return err
	}
	if !strings.Contains(profile.EmailAddress, "@") || strings.ContainsAny(profile.EmailAddress, "\r\n") || profile.HistoryID == "" {
		return fmt.Errorf("profile lacks mailbox identity or history cursor")
	}
	p.report.IdentityVerified = true
	p.report.IdentityMatches = strings.EqualFold(profile.EmailAddress, p.connection.Identity)
	var list struct {
		Messages []message `json:"messages"`
		Next     string    `json:"nextPageToken"`
	}
	query := url.Values{"maxResults": {"1"}, "q": {"smaller:65536"}}
	if err := p.call(ctx, "messages.list", "GET", "/messages", nil, query, nil, &list); err != nil {
		return err
	}
	if len(list.Messages) != 1 || list.Messages[0].ID == "" {
		return fmt.Errorf("need a small disposable message to prove full/raw reads")
	}
	params := map[string]string{"id": list.Messages[0].ID}
	var full, raw message
	if err := p.call(ctx, "messages.get.full", "GET", "/messages/{{id}}", params, url.Values{"format": {"full"}}, nil, &full); err != nil {
		return err
	}
	if err := p.call(ctx, "messages.get.raw", "GET", "/messages/{{id}}", params, url.Values{"format": {"raw"}}, nil, &raw); err != nil {
		return err
	}
	mime, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(raw.Raw, "="))
	if err != nil || full.Payload == nil || raw.ID != full.ID || raw.ThreadID != full.ThreadID || !bytes.Contains(mime, []byte("\r\n\r\n")) {
		return fmt.Errorf("native full/raw response compatibility failed")
	}
	if list.Next != "" {
		query.Set("pageToken", list.Next)
		var next struct {
			Messages []message `json:"messages"`
		}
		if err := p.call(ctx, "messages.list.page2", "GET", "/messages", nil, query, nil, &next); err != nil {
			return err
		}
		if len(next.Messages) == 0 || next.Messages[0].ID == full.ID {
			return fmt.Errorf("message pagination did not advance")
		}
	}
	var labels struct {
		Labels []struct {
			ID string `json:"id"`
		} `json:"labels"`
	}
	if err := p.call(ctx, "labels.list", "GET", "/labels", nil, nil, nil, &labels); err != nil {
		return err
	}
	if len(labels.Labels) == 0 || labels.Labels[0].ID == "" {
		return fmt.Errorf("native labels response missing")
	}
	if err := p.history(ctx, profile.HistoryID, false); err != nil {
		return err
	}
	var drafts struct {
		Drafts []draft `json:"drafts"`
	}
	if err := p.call(ctx, "drafts.list", "GET", "/drafts", nil, url.Values{"maxResults": {"2"}}, nil, &drafts); err != nil {
		return err
	}
	p.report.ReadPassed = true
	if !p.opts.WriteDrafts {
		return nil
	}
	if err := p.draftGate(ctx, profile.EmailAddress); err != nil {
		return err
	}
	if err := p.history(ctx, profile.HistoryID, true); err != nil {
		return err
	}
	p.report.DraftPassed = true
	if p.opts.SendApproved {
		if err := p.sendGate(ctx, profile.EmailAddress); err != nil {
			return err
		}
		p.report.SendPassed = true
	}
	p.report.ReleaseGate = p.report.ReadPassed && p.report.DraftPassed && p.report.SendPassed
	return nil
}

func (p *probe) history(ctx context.Context, cursor string, requireChanges bool) error {
	query := url.Values{"startHistoryId": {cursor}, "maxResults": {"1"}}
	seen := make(map[string]bool)
	changes := false
	for page := 0; page < 100; page++ {
		var h struct {
			History   []json.RawMessage `json:"history"`
			HistoryID string            `json:"historyId"`
			Next      string            `json:"nextPageToken"`
		}
		if err := p.call(ctx, "history.list", "GET", "/history", nil, query, nil, &h); err != nil {
			return err
		}
		if h.HistoryID == "" {
			return fmt.Errorf("history response lacks cursor")
		}
		changes = changes || len(h.History) > 0
		if h.Next == "" {
			if requireChanges && !changes {
				return fmt.Errorf("draft writes not visible in history")
			}
			return nil
		}
		if seen[h.Next] {
			return fmt.Errorf("history pagination repeated cursor")
		}
		seen[h.Next] = true
		query.Set("pageToken", h.Next)
	}
	return fmt.Errorf("history exceeds bounded probe pagination")
}

func fixture(from, to, subject string, attachment []byte) ([]byte, string, error) {
	if strings.ContainsAny(from+to, "\r\n") || !strings.Contains(to, "@") {
		return nil, "", fmt.Errorf("invalid fixture recipient")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, "", fmt.Errorf("could not generate fixture ID")
	}
	id := "<mammothmail-one-probe-" + hex.EncodeToString(nonce[:]) + "@example.invalid>"
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMessage-ID: %s\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=%q\r\n\r\n", from, to, subject, id, w.Boundary())
	body, err := w.CreatePart(textproto.MIMEHeader{"Content-Type": {"text/plain; charset=utf-8"}})
	if err != nil {
		return nil, "", err
	}
	_, _ = io.WriteString(body, "Mammoth Mail One compatibility probe. Disposable test data only.\r\n")
	if len(attachment) > 0 {
		a, err := w.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/octet-stream"}, "Content-Disposition": {`attachment; filename="one-probe.bin"`}, "Content-Transfer-Encoding": {"base64"}})
		if err != nil {
			return nil, "", err
		}
		encoded := base64.StdEncoding.EncodeToString(attachment)
		for len(encoded) > 0 {
			n := min(76, len(encoded))
			_, _ = io.WriteString(a, encoded[:n]+"\r\n")
			encoded = encoded[n:]
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return b.Bytes(), id, nil
}

func envelope(raw []byte) any {
	return map[string]any{"message": map[string]string{"raw": base64.RawURLEncoding.EncodeToString(raw)}}
}

func (p *probe) draftGate(ctx context.Context, email string) (err error) {
	attachment := bytes.Repeat([]byte("disposable-one-probe-fixture\n"), 3000)
	raw, _, err := fixture(email, email, "Mammoth Mail One compatibility probe — disposable draft", attachment)
	if err != nil {
		return err
	}
	var d draft
	// Mark cleanup uncertain before a non-idempotent create: a timeout may leave
	// an upstream draft whose ID we never received. Do not create another copy.
	p.report.CleanupRequired = true
	if err = p.call(ctx, "drafts.create", "POST", "/drafts", nil, nil, envelope(raw), &d); err != nil {
		return err
	}
	if d.ID == "" || d.Message.ID == "" {
		return fmt.Errorf("draft create lacks native IDs; inspect disposable draft in One/Gmail")
	}
	params := map[string]string{"id": d.ID}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
		defer cancel()
		if e := p.call(cleanup, "drafts.delete.fixture", "DELETE", "/drafts/{{id}}", params, nil, nil, nil); e != nil {
			if err == nil {
				err = e
			}
		} else {
			p.report.CleanupRequired = false
		}
	}()
	var got draft
	if err = p.call(ctx, "drafts.get.raw", "GET", "/drafts/{{id}}", params, url.Values{"format": {"raw"}}, nil, &got); err != nil {
		return err
	}
	decoded, e := base64.RawURLEncoding.DecodeString(strings.TrimRight(got.Message.Raw, "="))
	if e != nil || got.ID != d.ID || !bytes.Contains(decoded, []byte("one-probe.bin")) || !bytes.Contains(decoded, []byte("Disposable test data only")) {
		return fmt.Errorf("draft raw MIME round trip failed")
	}
	if err = p.call(ctx, "drafts.get.full", "GET", "/drafts/{{id}}", params, url.Values{"format": {"full"}}, nil, &got); err != nil {
		return err
	}
	var attachmentID string
	var walk func(*part)
	walk = func(part *part) {
		if part == nil {
			return
		}
		if part.Filename == "one-probe.bin" {
			attachmentID = part.Body.AttachmentID
		}
		for i := range part.Parts {
			walk(&part.Parts[i])
		}
	}
	walk(got.Message.Payload)
	if attachmentID == "" {
		return fmt.Errorf("test attachment did not expose attachment ID")
	}
	var att struct {
		Data string `json:"data"`
		Size int    `json:"size"`
	}
	if err = p.call(ctx, "attachments.get", "GET", "/messages/{{messageId}}/attachments/{{id}}", map[string]string{"messageId": got.Message.ID, "id": attachmentID}, nil, nil, &att); err != nil {
		return err
	}
	decoded, e = base64.RawURLEncoding.DecodeString(strings.TrimRight(att.Data, "="))
	if e != nil || !bytes.Equal(decoded, attachment) || att.Size != len(attachment) {
		return fmt.Errorf("attachment byte round trip failed")
	}
	updated, _, e := fixture(email, email, "Mammoth Mail One compatibility probe — updated disposable draft", nil)
	if e != nil {
		return e
	}
	if err = p.call(ctx, "drafts.update", "PUT", "/drafts/{{id}}", params, nil, envelope(updated), &got); err != nil {
		return err
	}
	if err = p.call(ctx, "drafts.get.updated", "GET", "/drafts/{{id}}", params, url.Values{"format": {"raw"}}, nil, &got); err != nil {
		return err
	}
	decoded, e = base64.RawURLEncoding.DecodeString(strings.TrimRight(got.Message.Raw, "="))
	if e != nil || !bytes.Contains(decoded, []byte("updated disposable draft")) {
		return fmt.Errorf("updated draft readback failed")
	}
	return nil
}

func (p *probe) sendGate(ctx context.Context, email string) error {
	raw, _, err := fixture(email, p.opts.SendTo, "Mammoth Mail One compatibility probe — approved test send", nil)
	if err != nil {
		return err
	}
	var sent message
	if err := p.call(ctx, "messages.send.approved", "POST", "/messages/send", nil, nil, map[string]string{"raw": base64.RawURLEncoding.EncodeToString(raw)}, &sent); err != nil {
		return err
	}
	if sent.ID == "" {
		return fmt.Errorf("send returned no ID; outcome unknown; do not retry")
	}
	var got message
	if err := p.call(ctx, "messages.get.sent", "GET", "/messages/{{id}}", map[string]string{"id": sent.ID}, url.Values{"format": {"full"}}, nil, &got); err != nil {
		return err
	}
	for _, label := range got.LabelIDs {
		if label == "SENT" && got.ID == sent.ID {
			return nil
		}
	}
	return fmt.Errorf("sent message not visible in Sent; do not retry send")
}
