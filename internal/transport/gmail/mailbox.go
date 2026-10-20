package gmail

import (
	"context"
	"encoding/base64"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/dellarb/mailmoose/internal/model"
)

// maxListResults is Gmail's documented upper bound for messages.list.
const maxListResults = 500

// Profile returns the authenticated user's profile.
func (c *Client) Profile(ctx context.Context, token string) (Profile, error) {
	var p Profile
	if err := c.getJSON(ctx, "gmail users.getProfile", c.endpoint("/profile"), token, &p); err != nil {
		return Profile{}, err
	}
	return p, nil
}

// Labels lists every label on the account.
func (c *Client) Labels(ctx context.Context, token string) ([]Label, error) {
	var out struct {
		Labels []Label `json:"labels"`
	}
	if err := c.getJSON(ctx, "gmail labels.list", c.endpoint("/labels"), token, &out); err != nil {
		return nil, err
	}
	return out.Labels, nil
}

// List returns one page of messages matching the optional query and label. page
// is the opaque nextPageToken from a previous call. limit maps to maxResults and
// is clamped to Gmail's maximum of 500.
func (c *Client) List(ctx context.Context, token, query, labelID, page string, limit int) (MessagePage, error) {
	q := url.Values{}
	if query != "" {
		q.Set("q", query)
	}
	if labelID != "" {
		q.Set("labelIds", labelID)
	}
	if page != "" {
		q.Set("pageToken", page)
	}
	if limit > 0 {
		if limit > maxListResults {
			limit = maxListResults
		}
		q.Set("maxResults", strconv.Itoa(limit))
	}
	u := c.endpoint("/messages")
	if encoded := q.Encode(); encoded != "" {
		u += "?" + encoded
	}
	var p MessagePage
	if err := c.getJSON(ctx, "gmail messages.list", u, token, &p); err != nil {
		return MessagePage{}, err
	}
	return p, nil
}

// Get fetches a single message. format is one of "full", "metadata", "raw" or
// "minimal"; an empty format defaults to "full".
func (c *Client) Get(ctx context.Context, token, id, format string) (Message, error) {
	if strings.TrimSpace(id) == "" {
		return Message{}, invalid("gmail messages.get: message id is required")
	}
	if format == "" {
		format = "full"
	}
	u := c.endpoint("/messages/"+url.PathEscape(id)) + "?format=" + url.QueryEscape(format)
	if format == "metadata" {
		// Include MIME structure/attachment descriptors without retrieving body
		// bytes. Gmail's metadata format omits nested MIME structure, so request
		// full with an explicit partial-response mask that excludes body.data.
		u = c.endpoint("/messages/"+url.PathEscape(id)) + "?format=full&fields=" + url.QueryEscape("id,threadId,labelIds,historyId,snippet,sizeEstimate,internalDate,payload(partId,mimeType,filename,headers,body(size,attachmentId),parts(partId,mimeType,filename,headers,body(size,attachmentId),parts(partId,mimeType,filename,body(size,attachmentId))))")
	}
	var m Message
	if err := c.getJSON(ctx, "gmail messages.get", u, token, &m); err != nil {
		return Message{}, err
	}
	return m, nil
}

// Raw streams the decoded RFC 5322 message into w. The Gmail API returns the
// message base64url-encoded inside a JSON envelope, so the envelope is bounded
// to maxResponseBytes and the decoded bytes are streamed to w rather than held
// in memory.
func (c *Client) Raw(ctx context.Context, token, id string, w io.Writer) error {
	if strings.TrimSpace(id) == "" {
		return invalid("gmail messages.get raw: message id is required")
	}
	if w == nil {
		return invalid("gmail messages.get raw: writer is nil")
	}
	u := c.endpoint("/messages/"+url.PathEscape(id)) + "?format=raw"
	var body rawBody
	if err := c.getJSON(ctx, "gmail messages.get raw", u, token, &body); err != nil {
		return err
	}
	return decodeBase64(w, body.Raw)
}

// Attachment streams the decoded bytes of an attachment into w.
func (c *Client) Attachment(ctx context.Context, token, msgID, attachmentID string, w io.Writer) error {
	if strings.TrimSpace(msgID) == "" || strings.TrimSpace(attachmentID) == "" {
		return invalid("gmail attachments.get: message id and attachment id are required")
	}
	if w == nil {
		return invalid("gmail attachments.get: writer is nil")
	}
	u := c.endpoint("/messages/" + url.PathEscape(msgID) + "/attachments/" + url.PathEscape(attachmentID))
	var body attachmentBody
	if err := c.getJSON(ctx, "gmail attachments.get", u, token, &body); err != nil {
		return err
	}
	return decodeBase64(w, body.Data)
}

// Modify applies label additions and removals to a message and returns the
// updated resource.
func (c *Client) Modify(ctx context.Context, token, id string, add, remove []string) (Message, error) {
	if strings.TrimSpace(id) == "" {
		return Message{}, invalid("gmail messages.modify: message id is required")
	}
	u := c.endpoint("/messages/" + url.PathEscape(id) + "/modify")
	var m Message
	if err := c.postJSON(ctx, "gmail messages.modify", u, token, modifyPayload{AddLabelIDs: add, RemoveLabelIDs: remove}, &m); err != nil {
		return Message{}, err
	}
	return m, nil
}

// Trash moves a message to the trash, or restores it from the trash when
// restore is true, and returns the updated resource.
func (c *Client) Trash(ctx context.Context, token, id string, restore bool) (Message, error) {
	if strings.TrimSpace(id) == "" {
		return Message{}, invalid("gmail messages.trash: message id is required")
	}
	action := "trash"
	if restore {
		action = "untrash"
	}
	u := c.endpoint("/messages/" + url.PathEscape(id) + "/" + action)
	var m Message
	if err := c.postJSON(ctx, "gmail messages."+action, u, token, nil, &m); err != nil {
		return Message{}, err
	}
	return m, nil
}

// CreateLabel creates a user label with the supplied display name.
func (c *Client) CreateLabel(ctx context.Context, token, name string) (Label, error) {
	if strings.TrimSpace(name) == "" {
		return Label{}, invalid("gmail labels.create: label name is required")
	}
	var l Label
	if err := c.postJSON(ctx, "gmail labels.create", c.endpoint("/labels"), token, labelPayload{Name: name}, &l); err != nil {
		return Label{}, err
	}
	return l, nil
}

// RenameLabel renames an existing label.
func (c *Client) RenameLabel(ctx context.Context, token, id, name string) (Label, error) {
	if strings.TrimSpace(id) == "" {
		return Label{}, invalid("gmail labels.update: label id is required")
	}
	if strings.TrimSpace(name) == "" {
		return Label{}, invalid("gmail labels.update: label name is required")
	}
	var l Label
	if err := c.patchJSON(ctx, "gmail labels.update", c.endpoint("/labels/"+url.PathEscape(id)), token, labelPayload{Name: name}, &l); err != nil {
		return Label{}, err
	}
	return l, nil
}

// DeleteLabel deletes a label.
func (c *Client) DeleteLabel(ctx context.Context, token, id string) error {
	if strings.TrimSpace(id) == "" {
		return invalid("gmail labels.delete: label id is required")
	}
	return c.delete(ctx, "gmail labels.delete", c.endpoint("/labels/"+url.PathEscape(id)), token)
}

// History returns one page of mailbox changes since start (a historyId). page is
// the opaque nextPageToken from a previous call. A too-old startHistoryId
// surfaces as a not_found *model.MailboxError.
func (c *Client) History(ctx context.Context, token, start, page string) (HistoryPage, error) {
	if strings.TrimSpace(start) == "" {
		return HistoryPage{}, invalid("gmail history.list: start history id is required")
	}
	q := url.Values{}
	q.Set("startHistoryId", start)
	if page != "" {
		q.Set("pageToken", page)
	}
	u := c.endpoint("/history") + "?" + q.Encode()
	var hp HistoryPage
	if err := c.getJSON(ctx, "gmail history.list", u, token, &hp); err != nil {
		return HistoryPage{}, err
	}
	return hp, nil
}

// decodeBase64 streams a base64 payload into w. Gmail returns base64url; the
// input is normalized to the standard alphabet with padding stripped so both
// the URL-safe and the standard encodings decode.
func decodeBase64(w io.Writer, data string) error {
	if data == "" {
		return nil
	}
	normalized := strings.TrimRight(strings.NewReplacer("-", "+", "_", "/").Replace(data), "=")
	dec := base64.NewDecoder(base64.RawStdEncoding, strings.NewReader(normalized))
	if _, err := io.Copy(w, dec); err != nil {
		return model.NewMailboxError(model.ErrKindInternal, "gmail: could not decode message content", false, err)
	}
	return nil
}
