// Package gmail implements a self-contained, standard-library-only client for
// Google's Gmail JSON API and the OAuth 2.0 token endpoint.
//
// The client is transport-injectable: the HTTP client, the API base URL and the
// OAuth token URL are all public fields on Client, so production uses the fixed
// Google endpoints while tests point them at a deterministic fixture server.
// No third-party dependency is used.
package gmail

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Message is a Gmail message resource. InternalDate is decoded from Gmail's
// millisecond-precision epoch string into an int64; see Message.UnmarshalJSON.
type Message struct {
	ID           string   `json:"id"`
	ThreadID     string   `json:"threadId"`
	LabelIDs     []string `json:"labelIds"`
	HistoryID    string   `json:"historyId"`
	Snippet      string   `json:"snippet"`
	SizeEstimate int64    `json:"sizeEstimate"`
	InternalDate int64    `json:"-"`
	Payload      *Part    `json:"payload,omitempty"`
}

// UnmarshalJSON decodes a Gmail message, tolerating internalDate being either a
// decimal string (Gmail's wire form) or a JSON number.
func (m *Message) UnmarshalJSON(data []byte) error {
	type plain Message
	var decoded struct {
		plain
		InternalDate json.RawMessage `json:"internalDate"`
	}
	decoded.plain = plain(*m)
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*m = Message(decoded.plain)
	m.InternalDate = parseEpochMillis(decoded.InternalDate)
	return nil
}

func parseEpochMillis(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	s := strings.Trim(string(raw), `"`)
	if s == "" || s == "null" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// Part is one node of a message's MIME tree.
type Part struct {
	PartID   string   `json:"partId"`
	MIMEType string   `json:"mimeType"`
	Filename string   `json:"filename"`
	Headers  []Header `json:"headers"`
	Body     Body     `json:"body"`
	Parts    []Part   `json:"parts"`
}

// Header is a single RFC 5322 header on a part.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Body is the inline or attachment body descriptor of a part.
type Body struct {
	Size         int    `json:"size"`
	Data         string `json:"data"`
	AttachmentID string `json:"attachmentId"`
}

// Profile is the result of Gmail's users.getProfile call. MessagesTotal and
// ThreadsTotal are part of the Gmail resource and are surfaced as a convenience
// beyond the required EmailAddress and HistoryID.
type Profile struct {
	EmailAddress  string `json:"emailAddress"`
	HistoryID     string `json:"historyId"`
	MessagesTotal int    `json:"messagesTotal"`
	ThreadsTotal  int    `json:"threadsTotal"`
}

// Label is a Gmail label resource.
type Label struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Type           string `json:"type"`
	MessagesTotal  int    `json:"messagesTotal"`
	MessagesUnread int    `json:"messagesUnread"`
}

// MessagePage is one page of Gmail's messages.list result.
type MessagePage struct {
	Messages           []Message `json:"messages"`
	NextPageToken      string    `json:"nextPageToken"`
	ResultSizeEstimate int       `json:"resultSizeEstimate"`
}

// HistoryPage is one page of Gmail's history.list result.
type HistoryPage struct {
	History       []History `json:"history"`
	NextPageToken string    `json:"nextPageToken"`
	HistoryID     string    `json:"historyId"`
}

// History is a single change record from Gmail's history.list.
type History struct {
	ID              string           `json:"id"`
	MessagesAdded   []HistoryMessage `json:"messagesAdded"`
	MessagesDeleted []HistoryMessage `json:"messagesDeleted"`
	LabelsAdded     []HistoryMessage `json:"labelsAdded"`
	LabelsRemoved   []HistoryMessage `json:"labelsRemoved"`
}

// HistoryMessage pairs a message with the labels affected by a history record.
type HistoryMessage struct {
	Message  Message  `json:"message"`
	LabelIDs []string `json:"labelIds"`
}

// Draft is a Gmail draft resource.
type Draft struct {
	ID      string  `json:"id"`
	Message Message `json:"message"`
}

// DraftPage is one page of Gmail's drafts.list result.
type DraftPage struct {
	Drafts             []Draft `json:"drafts"`
	NextPageToken      string  `json:"nextPageToken"`
	ResultSizeEstimate int     `json:"resultSizeEstimate"`
}

// rawBody is the response of messages.get with format=raw.
type rawBody struct {
	Raw string `json:"raw"`
}

// attachmentBody is the response of messages.attachments.get.
type attachmentBody struct {
	Data string `json:"data"`
	Size int64  `json:"size"`
}

// rawPayload is the request shape of messages.send.
type rawPayload struct {
	Raw      string `json:"raw"`
	ThreadID string `json:"threadId,omitempty"`
}

// draftPayload is the request shape of drafts.create.
type draftPayload struct {
	Message rawPayload `json:"message"`
}

// modifyPayload is the request shape of messages.modify.
type modifyPayload struct {
	AddLabelIDs    []string `json:"addLabelIds"`
	RemoveLabelIDs []string `json:"removeLabelIds"`
}

// labelPayload is the request shape of labels.create and labels.update.
type labelPayload struct {
	Name string `json:"name"`
}
