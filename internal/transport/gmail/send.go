package gmail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/dellarb/mailmoose/internal/model"
)

// SendRaw sends an RFC 5322 message. threadID, when non-empty, files the sent
// message into an existing thread. The message is streamed and base64url-encoded
// into Gmail's JSON envelope without buffering the whole message in memory.
//
// A definitive Gmail rejection is returned as a *model.MailboxError; a network
// failure or timeout before a response is readable is returned as a
// *transport.AmbiguousError because the message may already have been sent.
func (c *Client) SendRaw(ctx context.Context, token string, r io.Reader, threadID string) (Message, error) {
	if r == nil {
		return Message{}, invalid("gmail messages.send: message body is required")
	}
	var m Message
	if err := c.streamRaw(ctx, "gmail messages.send", c.endpoint("/messages/send"), token, r, threadID, false, &m); err != nil {
		return Message{}, err
	}
	return m, nil
}

// CreateDraft creates a draft from an RFC 5322 message. Like SendRaw it streams
// the message and marks an unknown outcome as ambiguous.
func (c *Client) CreateDraft(ctx context.Context, token string, r io.Reader, threadID string) (Draft, error) {
	if r == nil {
		return Draft{}, invalid("gmail drafts.create: message body is required")
	}
	var d Draft
	if err := c.streamRaw(ctx, "gmail drafts.create", c.endpoint("/drafts"), token, r, threadID, true, &d); err != nil {
		return Draft{}, err
	}
	return d, nil
}

// ListDrafts returns one page of drafts. limit maps to maxResults.
func (c *Client) ListDrafts(ctx context.Context, token, page string, limit int) (DraftPage, error) {
	q := url.Values{}
	if page != "" {
		q.Set("pageToken", page)
	}
	if limit > 0 {
		if limit > maxListResults {
			limit = maxListResults
		}
		q.Set("maxResults", strconv.Itoa(limit))
	}
	u := c.endpoint("/drafts")
	if encoded := q.Encode(); encoded != "" {
		u += "?" + encoded
	}
	var p DraftPage
	if err := c.getJSON(ctx, "gmail drafts.list", u, token, &p); err != nil {
		return DraftPage{}, err
	}
	return p, nil
}

// GetDraft fetches a single draft. format defaults to "full".
func (c *Client) GetDraft(ctx context.Context, token, id, format string) (Draft, error) {
	if strings.TrimSpace(id) == "" {
		return Draft{}, invalid("gmail drafts.get: draft id is required")
	}
	if format == "" {
		format = "full"
	}
	u := c.endpoint("/drafts/"+url.PathEscape(id)) + "?format=" + url.QueryEscape(format)
	var d Draft
	if err := c.getJSON(ctx, "gmail drafts.get", u, token, &d); err != nil {
		return Draft{}, err
	}
	return d, nil
}

// streamRaw posts a streamed JSON envelope (send or draft) and decodes the
// response. It distinguishes a local body error (definitive) from a transport
// failure after the request was built (ambiguous).
func (c *Client) streamRaw(ctx context.Context, op, endpoint, token string, r io.Reader, threadID string, draft bool, out any) error {
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		werr := writeRawEnvelope(pw, r, threadID, draft)
		if werr != nil {
			_ = pw.CloseWithError(werr)
		} else {
			_ = pw.Close()
		}
		errc <- werr
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, pr)
	if err != nil {
		_ = pr.Close()
		<-errc
		return model.NewMailboxError(model.ErrKindInternal, op+": could not build request", false, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.httpClient().Do(req)
	// Ensure the producer is unblocked whether or not the transport closed the
	// request body.
	_ = pr.Close()
	writeErr := <-errc
	if err != nil {
		if writeErr != nil && !errors.Is(writeErr, io.ErrClosedPipe) {
			return bodyError(op, writeErr)
		}
		return ambiguousSend(op, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errorForResponse(op, resp)
	}
	if e := decodeJSON(op, resp, out); e != nil {
		return ambiguousSend(op, e)
	}
	return nil
}

// writeRawEnvelope writes {"raw":"<base64url>","threadId":"..."} (or the draft
// shape) to w, streaming r through a base64url encoder. The source read is
// bounded by maxMessageBytes.
func writeRawEnvelope(w io.Writer, r io.Reader, threadID string, draft bool) error {
	head := `{"raw":"`
	tail := `"}`
	if draft {
		head = `{"message":{"raw":"`
		tail = `"}}`
	}
	if threadID != "" {
		tid, err := json.Marshal(threadID)
		if err != nil {
			return err
		}
		if draft {
			tail = `","threadId":` + string(tid) + `}}`
		} else {
			tail = `","threadId":` + string(tid) + `}`
		}
	}
	if _, err := io.WriteString(w, head); err != nil {
		return err
	}
	enc := base64.NewEncoder(base64.RawURLEncoding, w)
	if _, err := io.Copy(enc, &boundedReader{r: r, max: maxMessageBytes}); err != nil {
		_ = enc.Close()
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	_, err := io.WriteString(w, tail)
	return err
}

// boundedReader reads from r and fails with errMessageTooLarge once more than
// max bytes have been read.
type boundedReader struct {
	r   io.Reader
	n   int64
	max int64
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.n > b.max {
		return 0, errMessageTooLarge
	}
	remaining := b.max - b.n + 1
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := b.r.Read(p)
	b.n += int64(n)
	if b.n > b.max {
		return n, errMessageTooLarge
	}
	return n, err
}
