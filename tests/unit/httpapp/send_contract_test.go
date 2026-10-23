package httpapp_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

func assertSendResponse(t *testing.T, rr *httptest.ResponseRecorder, pending bool) model.Message {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &fields); err != nil || rr.Code != 200 || len(fields) != 4 {
		t.Fatalf("send response: %d %s (%v)", rr.Code, rr.Body.String(), err)
	}
	for _, name := range []string{"queued", "messageId", "provider_message_id", "message"} {
		if _, ok := fields[name]; !ok {
			t.Fatalf("missing %s: %s", name, rr.Body.String())
		}
	}
	var result struct {
		Queued     bool          `json:"queued"`
		MessageID  string        `json:"messageId"`
		ProviderID string        `json:"provider_message_id"`
		Message    model.Message `json:"message"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Queued != pending || result.Message.ID == "" || result.MessageID != result.Message.RFCMessageID || result.ProviderID != result.Message.ProviderMessageID {
		t.Fatalf("inconsistent send result: %s", rr.Body.String())
	}
	return result.Message
}

func TestAPICommonSendResponse(t *testing.T) {
	t.Parallel()
	for _, wait := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued", true: "wait"}[wait], func(t *testing.T) {
			t.Parallel()
			svc, h, u, dom, box := httpFixture(t)
			ctx := context.Background()
			key := adminKey(t, svc, u)
			original := seedInbound(t, svc, box, "contract", "<contract@test>", "Original", "original body")
			if wait {
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"messageId":"<delivered@test>"}`)
				}))
				defer provider.Close()
				if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, dom.ID, "brevo", map[string]any{"api_key": "k", "api_base": provider.URL}); err != nil {
					t.Fatal(err)
				}
				worker := app.NewOutboxWorker(svc, nil)
				worker.SetPeriod(10 * time.Millisecond)
				worker.Start()
				defer worker.Stop()
			}
			post := func(path, body string) model.Message {
				t.Helper()
				if wait {
					path += "?wait=true"
				}
				m := assertSendResponse(t, apiDo(h, "POST", path, key, body), !wait)
				if wait && (m.Status != "sent" || m.ProviderMessageID != "<delivered@test>") {
					t.Fatalf("not delivered: %+v", m)
				}
				return m
			}
			body := `{"inbox_id":"` + box.ID + `","to":["friend@example.net"],"subject":"Hello","text":"body"}`
			post("/v1/send", body)
			post("/v1/messages/"+original.ID+"/reply", `{"text":"thanks"}`)
			post("/v1/messages/"+original.ID+"/forward", `{"to":"friend@example.net"}`)
			draft := createDraft(t, h, key, box.ID)
			post("/v1/drafts/"+draft.ID+"/send", `{}`)
			draft = createDraft(t, h, key, box.ID)
			if rr := apiDo(h, "POST", "/v1/drafts/"+draft.ID+"/request-send", key, `{}`); rr.Code != 200 {
				t.Fatalf("request send: %d %s", rr.Code, rr.Body.String())
			}
			post("/v1/drafts/"+draft.ID+"/approve", `{}`)
			post("/v1/drafts", strings.TrimSuffix(body, "}")+`,"action":"send"}`)
			draft = createDraft(t, h, key, box.ID)
			path := "/v1/drafts/" + draft.ID
			if wait {
				path += "?wait=true"
			}
			assertSendResponse(t, apiDo(h, "PATCH", path, key, `{"action":"send"}`), !wait)
		})
	}
}

func TestAPIForwardAndReplyComposeFields(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	key := adminKey(t, svc, u)
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	original, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"original@example.net"}, Subject: "Original", Text: "original body", Attachments: []app.SendAttachment{{Filename: "original.txt", Content: []byte("original bytes")}}}, "")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"to":"friend@example.net","cc":["cc@example.net"],"bcc":"bcc@example.net","subject":"Custom","text":"note","html":"<p>note</p>","attachments":[{"filename":"extra.txt","content":"ZXh0cmE="}]}`
	for _, action := range []string{"reply", "forward"} {
		path := "/v1/messages/" + original.Message.ID + "/" + action
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", action+"-compose")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		m := assertSendResponse(t, rr, true)
		if m.Subject != "Custom" || len(m.To) != 1 || m.To[0] != "friend@example.net" || len(m.CC) != 1 || len(m.BCC) != 1 || m.HTML != "<p>note</p>" {
			t.Fatalf("compose fields: %+v", m)
		}
		atts, err := svc.Store.ListAttachments(ctx, p, m.ID)
		if err != nil {
			t.Fatal(err)
		}
		if action == "forward" {
			if m.ThreadID == original.Message.ThreadID || m.InReplyTo != "" || !strings.HasPrefix(m.Text, "note\n\n---------- Forwarded message") || !strings.Contains(m.Text, "original body") || len(atts) != 2 {
				t.Fatalf("forward contents: %+v attachments=%+v", m, atts)
			}
			for _, attachment := range atts {
				want := "extra"
				if attachment.Filename == "original.txt" {
					want = "original bytes"
				}
				if rr := apiGet(t, h, "/v1/attachments/"+attachment.ID, key); rr.Code != 200 || rr.Body.String() != want {
					t.Fatalf("forward attachment bytes: %d %q, want %q", rr.Code, rr.Body.String(), want)
				}
			}
		} else if m.ThreadID != original.Message.ThreadID || len(atts) != 1 {
			t.Fatalf("reply contents: %+v attachments=%+v", m, atts)
		}
		// Replaying the same key returns the same message rather than copying attachments again.
		req = httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", action+"-compose")
		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if replay := assertSendResponse(t, rr, true); replay.ID != m.ID {
			t.Fatalf("duplicate send: %s != %s", replay.ID, m.ID)
		}
	}
}

func TestAPIForwardValidationAndScope(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	key := adminKey(t, svc, u)
	original := seedInbound(t, svc, box, "validation", "<validation@test>", "Original", "body")
	path := "/v1/messages/" + original.ID + "/forward"
	for _, role := range []string{"read", "assistant"} {
		_, scoped, err := svc.Store.CreateAPIKey(ctx, u.AccountID, role, false, map[string]string{box.ID: role})
		if err != nil {
			t.Fatal(err)
		}
		if rr := apiDo(h, "POST", path, scoped, `{"to":"friend@example.net"}`); rr.Code != 403 {
			t.Fatalf("%s forwarded: %d %s", role, rr.Code, rr.Body.String())
		}
	}
	_, owner, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "owner", false, map[string]string{box.ID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	assertSendResponse(t, apiDo(h, "POST", path, owner, `{"to":"friend@example.net"}`), true)
	// A key from another account in this store must not discover or forward the source.
	otherUser, err := svc.Store.CreateAccountAndAdmin(ctx, "Other", "other@example.test", "correct horse battery staple", svc.Config.DefaultQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	otherKey := adminKey(t, svc, otherUser)
	if rr := apiDo(h, "POST", path, otherKey, `{"to":"friend@example.net"}`); rr.Code != 404 {
		t.Fatalf("cross-account forward: %d %s", rr.Code, rr.Body.String())
	}
	for _, tc := range []struct {
		path, key, body string
		status          int
	}{
		{path, "", `{"to":"friend@example.net"}`, 401},
		{path, key, `{}`, 400},
		{path, key, `{"to":"friend@example.net\r\nBcc: victim@example.net"}`, 400},
		{path, key, `{"to":123}`, 400},
		{path, key, `{"to":"friend@example.net","sender":"outsider@outside.test"}`, 403},
		{"/v1/messages/missing/forward", key, `{"to":"friend@example.net"}`, 404},
	} {
		if rr := apiDo(h, "POST", tc.path, tc.key, tc.body); rr.Code != tc.status {
			t.Fatalf("%s %s: %d %s, want %d", tc.path, tc.body, rr.Code, rr.Body.String(), tc.status)
		}
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	if _, _, err := svc.Store.SetMessageSpam(ctx, p, original.ID, true); err != nil {
		t.Fatal(err)
	}
	if rr := apiDo(h, "POST", path, key, `{"to":"friend@example.net"}`); rr.Code != 409 {
		t.Fatalf("spam forwarded: %d %s", rr.Code, rr.Body.String())
	}
	if _, _, err := svc.Store.SetMessageSpam(ctx, p, original.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetAccountStorageQuota(ctx, u.AccountID, 1); err != nil {
		t.Fatal(err)
	}
	if rr := apiDo(h, "POST", path, key, `{"to":"friend@example.net"}`); rr.Code != 507 {
		t.Fatalf("over-quota forwarded: %d %s", rr.Code, rr.Body.String())
	}
}

type unavailableForwardSource struct {
	app.RemoteMessageResolver
}

func TestAPIIdempotencyRejectsDifferentRequests(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	key := adminKey(t, svc, u)
	ctx := context.Background()
	original := seedInbound(t, svc, box, "idem-source", "<idem-source@test>", "Original", "body")
	other := seedInbound(t, svc, box, "idem-other", "<idem-other@test>", "Other", "body")
	body := `{"to":"friend@example.net","subject":"Note","text":"body","attachments":[{"filename":"note.txt","content_type":"text/plain","content":"YQ=="}]}`
	post := func(path, body, idem string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", idem)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	for _, action := range []string{"forward", "reply", "send"} {
		t.Run(action, func(t *testing.T) {
			path := "/v1/messages/" + original.ID + "/" + action
			requestBody := body
			if action == "send" {
				path = "/v1/send"
				requestBody = strings.TrimSuffix(body, "}") + `,"inbox_id":"` + box.ID + `"}`
			}
			first := assertSendResponse(t, post(path, requestBody, action), true)
			assertConflict := func(path, body string) {
				t.Helper()
				rr := post(path, body, action)
				var result struct {
					Code string `json:"code"`
				}
				if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil || rr.Code != 409 || result.Code != "conflict" {
					t.Fatalf("mismatch: %d %s (%v)", rr.Code, rr.Body.String(), err)
				}
			}
			for _, next := range []string{"forward", "reply"} {
				if next != action {
					assertConflict("/v1/messages/"+original.ID+"/"+next, body)
				}
				assertConflict("/v1/messages/"+other.ID+"/"+next, body)
			}
			if action != "send" {
				assertConflict("/v1/send", strings.TrimSuffix(body, "}")+`,"inbox_id":"`+box.ID+`"}`)
			}
			for _, change := range [][2]string{
				{"friend@example.net", "other@example.net"},
				{`"Note"`, `"Changed"`},
				{`"body"`, `"changed"`},
				{"note.txt", "other.txt"},
				{"text/plain", "text/csv"},
				{"YQ==", "Yg=="},
			} {
				assertConflict(path, strings.Replace(requestBody, change[0], change[1], 1))
			}
			// Parsed inputs, not JSON formatting or recipient syntax, identify a retry.
			retryBody := "\n" + strings.Replace(requestBody, `"to":"friend@example.net"`, `"to":["friend@example.net"]`, 1)
			if replay := assertSendResponse(t, post(path, retryBody, action), true); replay.ID != first.ID {
				t.Fatalf("replay ID=%s, want %s", replay.ID, first.ID)
			}
		})
	}
	// Old completed keys have no fingerprint. Never resend or claim they match.
	if _, _, err := svc.Store.CommitOutbound(ctx, store.OutboundRecord{
		Inbox: box, From: model.Address{Address: box.Address}, To: []string{"friend@example.net"},
		Subject: "Legacy", Text: "body", RawPath: "messages/legacy.eml", IdemKey: "legacy",
	}); err != nil {
		t.Fatal(err)
	}
	if rr := post("/v1/messages/"+original.ID+"/reply", body, "legacy"); rr.Code != 409 {
		t.Fatalf("legacy replay: %d %s", rr.Code, rr.Body.String())
	}
	outbound, err := svc.Store.ListMessages(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, store.MessageFilter{InboxID: box.ID, Direction: "outbound", Limit: 10})
	if err != nil || len(outbound) != 4 {
		t.Fatalf("mismatches enqueued mail: %d messages, %v", len(outbound), err)
	}
}

func (unavailableForwardSource) ResolveRemoteForward(context.Context, string, string) (model.Message, []app.SendAttachment, bool, error) {
	return model.Message{}, nil, true, model.NewMailboxError(model.ErrKindUnavailable, "source body unavailable", true, nil)
}

func TestAPIForwardRemoteFetchFailure(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box, fake := standaloneFixture(t)
	key := adminKey(t, svc, u)
	fake.add("INBOX", contractMIME, "<source@test>", "Original")
	if rr := apiGet(t, h, "/v1/inboxes/"+box.ID+"/messages", key); rr.Code != 200 {
		t.Fatalf("reconcile: %d %s", rr.Code, rr.Body.String())
	}
	ctx := context.Background()
	messages, err := svc.Store.ListRemoteMessagesFiltered(ctx, u.AccountID, box.ID, store.RemoteMessageFilter{})
	if err != nil || len(messages) != 1 {
		t.Fatalf("source: %+v %v", messages, err)
	}
	svc.RemoteMessages = unavailableForwardSource{svc.RemoteMessages}
	rr := apiDo(h, "POST", "/v1/messages/"+messages[0].ID+"/forward", key, `{"to":"friend@example.net"}`)
	if rr.Code != 503 || !strings.Contains(rr.Body.String(), "source body unavailable") {
		t.Fatalf("remote fetch failure: %d %s", rr.Code, rr.Body.String())
	}
	outbound, err := svc.Store.ListMessages(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, store.MessageFilter{InboxID: box.ID, Direction: "outbound", Limit: 10})
	if err != nil || len(outbound) != 0 {
		t.Fatalf("partial forward enqueued: %+v %v", outbound, err)
	}
}
