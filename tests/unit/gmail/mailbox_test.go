package gmail_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/transport/gmail"
)

func TestProfile(t *testing.T) {
	var gotPath string
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"emailAddress":"me@example.com","messagesTotal":42,"threadsTotal":10,"historyId":"9001"}`)
	}))

	p, err := c.Profile(context.Background(), "tok")
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if gotPath != apiPrefix+"/profile" {
		t.Fatalf("path = %q", gotPath)
	}
	if p.EmailAddress != "me@example.com" || p.HistoryID != "9001" || p.MessagesTotal != 42 || p.ThreadsTotal != 10 {
		t.Fatalf("profile = %#v", p)
	}
}

func TestLabels(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"labels":[{"id":"INBOX","name":"INBOX","type":"system","messagesTotal":5,"messagesUnread":2},{"id":"Label_1","name":"Work","type":"user"}]}`)
	}))

	labels, err := c.Labels(context.Background(), "tok")
	if err != nil {
		t.Fatalf("Labels: %v", err)
	}
	if len(labels) != 2 || labels[0].ID != "INBOX" || labels[0].MessagesUnread != 2 || labels[1].Name != "Work" {
		t.Fatalf("labels = %#v", labels)
	}
}

func TestListPagination(t *testing.T) {
	var gotQuery map[string]string
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = map[string]string{
			"q":          r.URL.Query().Get("q"),
			"labelIds":   r.URL.Query().Get("labelIds"),
			"pageToken":  r.URL.Query().Get("pageToken"),
			"maxResults": r.URL.Query().Get("maxResults"),
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"messages":[{"id":"m1","threadId":"t1"},{"id":"m2","threadId":"t2"}],"nextPageToken":"p2","resultSizeEstimate":7}`)
	}))

	page, err := c.List(context.Background(), "tok", "is:unread", "INBOX", "p1", 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if gotQuery["q"] != "is:unread" || gotQuery["labelIds"] != "INBOX" || gotQuery["pageToken"] != "p1" || gotQuery["maxResults"] != "50" {
		t.Fatalf("query = %#v", gotQuery)
	}
	if len(page.Messages) != 2 || page.Messages[1].ID != "m2" || page.NextPageToken != "p2" || page.ResultSizeEstimate != 7 {
		t.Fatalf("page = %#v", page)
	}
}

func TestListClampsLimit(t *testing.T) {
	var maxResults string
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		maxResults = r.URL.Query().Get("maxResults")
		io.WriteString(w, `{}`)
	}))
	if _, err := c.List(context.Background(), "tok", "", "", "", 5000); err != nil {
		t.Fatalf("List: %v", err)
	}
	if maxResults != "500" {
		t.Fatalf("maxResults = %q want 500", maxResults)
	}
}

func TestGetMessageNestedParts(t *testing.T) {
	var gotFormat string
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotFormat = r.URL.Query().Get("format")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"id":"m1","threadId":"t1","labelIds":["INBOX","UNREAD"],
			"historyId":"123","snippet":"hi","sizeEstimate":2048,
			"internalDate":"1700000000000",
			"payload":{
				"mimeType":"multipart/mixed",
				"headers":[{"name":"Subject","value":"Hello"}],
				"body":{"size":0},
				"parts":[
					{"partId":"0","mimeType":"text/plain","body":{"size":5,"data":"aGVsbG8"}},
					{"partId":"1","mimeType":"application/pdf","filename":"q.pdf","headers":[{"name":"Content-Disposition","value":"attachment"}],"body":{"size":10,"attachmentId":"att-1"}}
				]
			}
		}`)
	}))

	m, err := c.Get(context.Background(), "tok", "m1", "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotFormat != "full" {
		t.Fatalf("format = %q", gotFormat)
	}
	if m.ID != "m1" || m.InternalDate != 1700000000000 || m.SizeEstimate != 2048 {
		t.Fatalf("message = %#v", m)
	}
	if m.Payload == nil || len(m.Payload.Parts) != 2 {
		t.Fatalf("payload = %#v", m.Payload)
	}
	if m.Payload.Parts[1].Filename != "q.pdf" || m.Payload.Parts[1].Body.AttachmentID != "att-1" {
		t.Fatalf("attachment part = %#v", m.Payload.Parts[1])
	}
	if m.Payload.Headers[0].Name != "Subject" || m.Payload.Headers[0].Value != "Hello" {
		t.Fatalf("headers = %#v", m.Payload.Headers)
	}
}

func TestRawStreamsDecoded(t *testing.T) {
	raw := "From: a@example.com\r\nSubject: Hi\r\n\r\nbody text"
	cases := map[string]string{
		"url": base64.RawURLEncoding.EncodeToString([]byte(raw)),
		"std": base64.StdEncoding.EncodeToString([]byte(raw)),
	}
	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("format") != "raw" {
					t.Errorf("format = %q", r.URL.Query().Get("format"))
				}
				w.Header().Set("Content-Type", "application/json")
				body, _ := json.Marshal(map[string]string{"raw": encoded})
				w.Write(body)
			}))
			var buf bytes.Buffer
			if err := c.Raw(context.Background(), "tok", "m1", &buf); err != nil {
				t.Fatalf("Raw: %v", err)
			}
			if buf.String() != raw {
				t.Fatalf("raw = %q want %q", buf.String(), raw)
			}
		})
	}
}

func TestAttachmentStreamsDecoded(t *testing.T) {
	payload := []byte("pdf-bytes-here")
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	var gotPath string
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(map[string]any{"data": encoded, "size": len(payload)})
		w.Write(body)
	}))

	var buf bytes.Buffer
	if err := c.Attachment(context.Background(), "tok", "m1", "att-1", &buf); err != nil {
		t.Fatalf("Attachment: %v", err)
	}
	if gotPath != apiPrefix+"/messages/m1/attachments/att-1" {
		t.Fatalf("path = %q", gotPath)
	}
	if !bytes.Equal(buf.Bytes(), payload) {
		t.Fatalf("payload = %q", buf.Bytes())
	}
}

func TestModify(t *testing.T) {
	var gotMethod, gotPath string
	var got struct {
		Add    []string `json:"addLabelIds"`
		Remove []string `json:"removeLabelIds"`
	}
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"m1","labelIds":["INBOX","STARRED"]}`)
	}))

	m, err := c.Modify(context.Background(), "tok", "m1", []string{"STARRED"}, []string{"UNREAD"})
	if err != nil {
		t.Fatalf("Modify: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != apiPrefix+"/messages/m1/modify" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	if len(got.Add) != 1 || got.Add[0] != "STARRED" || len(got.Remove) != 1 || got.Remove[0] != "UNREAD" {
		t.Fatalf("payload = %#v", got)
	}
	if len(m.LabelIDs) != 2 || m.LabelIDs[1] != "STARRED" {
		t.Fatalf("message = %#v", m)
	}
}

func TestTrashAndUntrash(t *testing.T) {
	var paths []string
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"m1"}`)
	}))

	if _, err := c.Trash(context.Background(), "tok", "m1", false); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if _, err := c.Trash(context.Background(), "tok", "m1", true); err != nil {
		t.Fatalf("Untrash: %v", err)
	}
	want := []string{
		"POST " + apiPrefix + "/messages/m1/trash",
		"POST " + apiPrefix + "/messages/m1/untrash",
	}
	if len(paths) != 2 || paths[0] != want[0] || paths[1] != want[1] {
		t.Fatalf("paths = %#v", paths)
	}
}

func TestLabelCRUD(t *testing.T) {
	type call struct {
		method string
		path   string
		name   string
	}
	var calls []call
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls = append(calls, call{method: r.Method, path: r.URL.Path, name: body.Name})
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			io.WriteString(w, `{"id":"Label_9","name":"`+body.Name+`","type":"user"}`)
		}
	}))

	if _, err := c.CreateLabel(context.Background(), "tok", "Work"); err != nil {
		t.Fatalf("CreateLabel: %v", err)
	}
	if _, err := c.RenameLabel(context.Background(), "tok", "Label_9", "Work 2"); err != nil {
		t.Fatalf("RenameLabel: %v", err)
	}
	if err := c.DeleteLabel(context.Background(), "tok", "Label_9"); err != nil {
		t.Fatalf("DeleteLabel: %v", err)
	}
	want := []call{
		{http.MethodPost, apiPrefix + "/labels", "Work"},
		{http.MethodPatch, apiPrefix + "/labels/Label_9", "Work 2"},
		{http.MethodDelete, apiPrefix + "/labels/Label_9", ""},
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %#v", calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("call[%d] = %#v want %#v", i, calls[i], want[i])
		}
	}
}

func TestHistoryPagination(t *testing.T) {
	var gotStart, gotPage string
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotStart = r.URL.Query().Get("startHistoryId")
		gotPage = r.URL.Query().Get("pageToken")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"history":[{
				"id":"5",
				"messagesAdded":[{"message":{"id":"m1","threadId":"t1"},"labelIds":["INBOX"]}],
				"labelsRemoved":[{"message":{"id":"m2"},"labelIds":["UNREAD"]}]
			}],
			"nextPageToken":"p3",
			"historyId":"200"
		}`)
	}))

	hp, err := c.History(context.Background(), "tok", "100", "p2")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if gotStart != "100" || gotPage != "p2" {
		t.Fatalf("query start=%q page=%q", gotStart, gotPage)
	}
	if len(hp.History) != 1 || hp.HistoryID != "200" || hp.NextPageToken != "p3" {
		t.Fatalf("page = %#v", hp)
	}
	h := hp.History[0]
	if len(h.MessagesAdded) != 1 || h.MessagesAdded[0].Message.ID != "m1" || h.MessagesAdded[0].LabelIDs[0] != "INBOX" {
		t.Fatalf("messagesAdded = %#v", h.MessagesAdded)
	}
	if len(h.LabelsRemoved) != 1 || h.LabelsRemoved[0].Message.ID != "m2" {
		t.Fatalf("labelsRemoved = %#v", h.LabelsRemoved)
	}
}

func TestHistoryNotFound(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"code":404,"status":"NOT_FOUND","message":"startHistoryId 1 is too old"}}`)
	}))

	_, err := c.History(context.Background(), "tok", "1", "")
	assertKind(t, err, model.ErrKindNotFound)
	if contains(err.Error(), "too old") {
		t.Fatalf("error leaked provider message: %q", err.Error())
	}
}

func TestInvalidInputs(t *testing.T) {
	c := gmail.NewClient(nil)
	ctx := context.Background()
	_, err := c.Get(ctx, "tok", "", "")
	assertKind(t, err, model.ErrKindInvalid)
	_, err = c.History(ctx, "tok", "", "")
	assertKind(t, err, model.ErrKindInvalid)
	_, err = c.CreateLabel(ctx, "tok", "  ")
	assertKind(t, err, model.ErrKindInvalid)
	err = c.DeleteLabel(ctx, "tok", "")
	assertKind(t, err, model.ErrKindInvalid)
	_, err = c.ListDrafts(ctx, "tok", "", 0)
	if err == nil {
		t.Fatal("ListDrafts against no server should fail")
	}
}
