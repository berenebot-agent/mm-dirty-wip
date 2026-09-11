package app_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/mailparse"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
	"gatehouse-mail/internal/transport"
)

const approverAddress = "ben@approver.test"

var approveTokenRe = regexp.MustCompile(`\[GH-APPROVE:([A-Za-z0-9_-]{16,})\]`)

// mgControlRequest builds a Mailgun multipart webhook with a chosen sender,
// subject and body so the approval control path can be exercised end to end.
func mgControlRequest(t *testing.T, key, deliveryID, recipient, sender, subject, body string) *http.Request {
	t.Helper()
	return mgControlRequestFrom(t, key, deliveryID, recipient, sender, sender, subject, body)
}

// mgControlRequestFrom builds a Mailgun control webhook where the
// provider-attested envelope sender (the `sender` form field) and the MIME
// From header can be set independently. An empty envelopeFrom omits the field.
func mgControlRequestFrom(t *testing.T, key, deliveryID, recipient, envelopeFrom, headerFrom, subject, body string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	raw := "From: " + headerFrom + "\r\nTo: " + recipient + "\r\nSubject: " + subject + "\r\nMessage-ID: <ctl@test>\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\n" + body
	fields := map[string]string{"timestamp": ts, "token": deliveryID, "signature": mgSig(key, ts, deliveryID), "recipient": recipient, "Message-Id": "<ctl@test>"}
	if envelopeFrom != "" {
		fields["sender"] = envelopeFrom
	}
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	p, _ := mw.CreateFormField("body-mime")
	_, _ = io.WriteString(p, raw)
	_ = mw.Close()
	r := httptest.NewRequest("POST", "/internal/ingest/mailgun/raw-mime", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r
}

// approvalToken reads the latest queued approval-request email and extracts the
// approve control token from its text body.
func approvalToken(t *testing.T, svc *app.Service, accountID, inboxID string) string {
	t.Helper()
	ctx := context.Background()
	box, err := svc.Store.GetInboxInternal(ctx, accountID, inboxID)
	if err != nil {
		t.Fatal(err)
	}
	m := queuedApprovalEmail(t, svc, ctx, accountID, box.ID)
	parsed, err := mailparse.ParseFile(filepath.Join(svc.Config.DataDir, filepath.FromSlash(m.RawPath)))
	if err != nil {
		t.Fatal(err)
	}
	if match := approveTokenRe.FindStringSubmatch(parsed.Text); len(match) == 2 {
		return match[1]
	}
	t.Fatal("no approval token found in queued approval email")
	return ""
}

// queuedApprovalEmail returns the queued approval-request message. Approval
// mail is workflow mail and is deliberately hidden from the mailbox read
// surface, so it is located through the send request that queued it.
func queuedApprovalEmail(t *testing.T, svc *app.Service, ctx context.Context, accountID, inboxID string) model.Message {
	t.Helper()
	srs, err := svc.Store.ListSendRequests(ctx, model.Principal{AccountID: accountID, Admin: true}, inboxID, true, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, sr := range srs {
		if sr.ApprovalMessageID == "" {
			continue
		}
		m, err := svc.Store.GetMessageByID(ctx, accountID, sr.ApprovalMessageID)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	t.Fatal("no queued approval email found")
	return model.Message{}
}

// approvalEmailText returns the plain-text body of the latest queued
// approval-request email.
func approvalEmailText(t *testing.T, svc *app.Service, accountID, inboxID string) string {
	t.Helper()
	ctx := context.Background()
	box, err := svc.Store.GetInboxInternal(ctx, accountID, inboxID)
	if err != nil {
		t.Fatal(err)
	}
	m := queuedApprovalEmail(t, svc, ctx, accountID, box.ID)
	parsed, err := mailparse.ParseFile(filepath.Join(svc.Config.DataDir, filepath.FromSlash(m.RawPath)))
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Text
}

func TestApproverImpliesExternalRequest(t *testing.T) {
	svc, u, _, box := testService(t)
	svc.Config.ApprovalExpiryHours = 48
	ctx := context.Background()
	if err := svc.Store.SetInboxApprover(ctx, u.AccountID, box.ID, approverAddress); err != nil {
		t.Fatal(err)
	}
	asst := assistantPrincipal(u.AccountID, box.ID)
	d, err := svc.Store.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "auto", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	// No explicit external flag: the configured approver makes it external.
	got, err := svc.RequestSend(ctx, asst, d.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.SendRequest == nil || got.SendRequest.ApproverEmail != approverAddress || got.SendRequest.TokenExpiresAt == nil {
		t.Fatalf("request not external: %+v", got.SendRequest)
	}
	// The approval email is queued and carries the control token.
	token := approvalToken(t, svc, u.AccountID, box.ID)
	if token == "" {
		t.Fatal("no approval token in queued email")
	}
	if len(token) != 16 {
		t.Fatalf("token length = %d, want 16", len(token))
	}
	text := approvalEmailText(t, svc, u.AccountID, box.ID)
	// It must tell the approver that feedback does not alter the draft.
	if !strings.Contains(text, "does not change the email being sent") {
		t.Fatalf("approval email missing feedback clarification:\n%s", text)
	}
	// The reply subject includes the token and the draft subject.
	if want := "[GH-APPROVE:" + token + "] auto"; !strings.Contains(text, want) {
		t.Fatalf("approval email missing reply subject %q:\n%s", want, text)
	}
}

func TestExternalRequestRequiresConfiguredApprover(t *testing.T) {
	svc, u, _, box := testService(t)
	ctx := context.Background()
	asst := assistantPrincipal(u.AccountID, box.ID)
	d, err := svc.Store.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s", Text: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RequestSend(ctx, asst, d.ID, true); err == nil {
		t.Fatal("external request accepted without a configured approver")
	}
}

func TestExternalApprovalApproveByEmail(t *testing.T) {
	svc, u, dom, box := testService(t)
	svc.Config.ApprovalExpiryHours = 48
	seedInbound(t, svc, u.AccountID, dom.ID, "mailgun", map[string]any{"signing_key": testMailgunKey})
	if err := svc.Store.SetInboxApprover(context.Background(), u.AccountID, box.ID, approverAddress); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	asst := assistantPrincipal(u.AccountID, box.ID)

	d, err := svc.Store.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "proposal", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.RequestSend(ctx, asst, d.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.SendRequest == nil || got.SendRequest.ApproverEmail != approverAddress || got.SendRequest.TokenExpiresAt == nil {
		t.Fatalf("request %+v", got.SendRequest)
	}
	token := approvalToken(t, svc, u.AccountID, box.ID)

	body := "[GH-FEEDBACK-BEGIN]\r\n\r\n\r\n[GH-FEEDBACK-END]\r\n"
	req := mgControlRequest(t, testMailgunKey, "ctl-1", box.Address, "Ben <"+approverAddress+">", "[GH-APPROVE:"+token+"]", body)
	msg, dup, err := svc.IngestInbound(ctx, "mailgun", req)
	if !errors.Is(err, transport.ErrInboundIgnored) {
		t.Fatalf("control ingest err=%v msg=%+v dup=%v", err, msg, dup)
	}

	// The frozen draft was approved and consumed, and the original message is
	// queued to the draft's recipient.
	if _, err = svc.Store.GetDraftInternal(ctx, u.AccountID, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("draft not consumed: %v", err)
	}
	sr, err := svc.Store.GetSendRequestByDraft(context.Background(), asst, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sr.Status != model.SendRequestApproved || sr.DecisionMethod != model.DecisionMethodEmail || sr.DecisionActor != approverAddress {
		t.Fatalf("request after email approve %+v", sr)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	msgs, err := svc.Store.ListOutbox(ctx, p, box.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	foundDraft := false
	for _, m := range msgs {
		if len(m.To) == 1 && m.To[0] == "x@y.test" && m.Subject == "proposal" {
			foundDraft = true
		}
	}
	if !foundDraft {
		t.Fatalf("approved draft was not queued: %+v", msgs)
	}
	// The control email is not stored as mailbox content.
	inbound, err := svc.Store.ListMessages(ctx, p, store.MessageFilter{InboxID: box.ID, Direction: "inbound", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(inbound) != 0 {
		t.Fatalf("control mail became inbox content: %+v", inbound)
	}
	controls, err := svc.Store.ListControlMessages(ctx, u.AccountID, box.ID, 10)
	if err != nil || len(controls) != 1 || controls[0].Outcome != "approved" {
		t.Fatalf("control records %+v err=%v", controls, err)
	}
}

func TestExternalApprovalRejectByEmailCarriesFeedback(t *testing.T) {
	svc, u, dom, box := testService(t)
	svc.Config.ApprovalExpiryHours = 48
	seedInbound(t, svc, u.AccountID, dom.ID, "mailgun", map[string]any{"signing_key": testMailgunKey})
	if err := svc.Store.SetInboxApprover(context.Background(), u.AccountID, box.ID, approverAddress); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	asst := assistantPrincipal(u.AccountID, box.ID)
	d, err := svc.Store.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "proposal", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RequestSend(ctx, asst, d.ID, true); err != nil {
		t.Fatal(err)
	}
	token := approvalToken(t, svc, u.AccountID, box.ID)
	body := "[GH-FEEDBACK-BEGIN]\r\n\r\nPlease use the FY27 figures.\r\n\r\n[GH-FEEDBACK-END]\r\n-- \r\nsignature that must be ignored"
	req := mgControlRequest(t, testMailgunKey, "ctl-rej", box.Address, "Ben <"+approverAddress+">", "[GH-REJECT:"+token+"]", body)
	if _, _, err := svc.IngestInbound(ctx, "mailgun", req); !errors.Is(err, transport.ErrInboundIgnored) {
		t.Fatalf("control ingest err=%v", err)
	}
	sr, err := svc.Store.GetSendRequestByDraft(ctx, asst, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sr.Status != model.SendRequestRejected || sr.Feedback != "Please use the FY27 figures." || sr.DecisionActor != approverAddress {
		t.Fatalf("reject %+v", sr)
	}
	got, err := svc.Store.GetDraft(ctx, asst, d.ID)
	if err != nil || got.Status != model.DraftStatusRejected {
		t.Fatalf("draft after reject %+v err=%v", got, err)
	}
}

func TestExternalApprovalSenderMismatchIsConsumedButIgnored(t *testing.T) {
	svc, u, dom, box := testService(t)
	svc.Config.ApprovalExpiryHours = 48
	seedInbound(t, svc, u.AccountID, dom.ID, "mailgun", map[string]any{"signing_key": testMailgunKey})
	if err := svc.Store.SetInboxApprover(context.Background(), u.AccountID, box.ID, approverAddress); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	asst := assistantPrincipal(u.AccountID, box.ID)
	d, err := svc.Store.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "proposal", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RequestSend(ctx, asst, d.ID, true); err != nil {
		t.Fatal(err)
	}
	token := approvalToken(t, svc, u.AccountID, box.ID)
	req := mgControlRequest(t, testMailgunKey, "ctl-bad", box.Address, "Mallory <mallory@evil.test>", "[GH-APPROVE:"+token+"]", "[GH-FEEDBACK-BEGIN]\r\n\r\n[GH-FEEDBACK-END]")
	if _, _, err := svc.IngestInbound(ctx, "mailgun", req); !errors.Is(err, transport.ErrInboundIgnored) {
		t.Fatalf("control ingest err=%v", err)
	}
	sr, err := svc.Store.GetSendRequestByDraft(ctx, asst, d.ID)
	if err != nil || sr.Status != model.SendRequestPending {
		t.Fatalf("mismatched sender changed state: %+v err=%v", sr, err)
	}
	controls, err := svc.Store.ListControlMessages(ctx, u.AccountID, box.ID, 10)
	if err != nil || len(controls) != 1 || controls[0].Outcome != "invalid" {
		t.Fatalf("control records %+v err=%v", controls, err)
	}
}

func TestExternalApprovalSurvivesInboxApproverChange(t *testing.T) {
	svc, u, dom, box := testService(t)
	svc.Config.ApprovalExpiryHours = 48
	seedInbound(t, svc, u.AccountID, dom.ID, "mailgun", map[string]any{"signing_key": testMailgunKey})
	ctx := context.Background()
	if err := svc.Store.SetInboxApprover(ctx, u.AccountID, box.ID, approverAddress); err != nil {
		t.Fatal(err)
	}
	asst := assistantPrincipal(u.AccountID, box.ID)
	d, err := svc.Store.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "proposal", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RequestSend(ctx, asst, d.ID, true); err != nil {
		t.Fatal(err)
	}
	token := approvalToken(t, svc, u.AccountID, box.ID)
	// The inbox approver changes after the request was created.
	if err = svc.Store.SetInboxApprover(ctx, u.AccountID, box.ID, "replacement@approver.test"); err != nil {
		t.Fatal(err)
	}
	// The original approver's reply still authorizes the frozen request.
	req := mgControlRequest(t, testMailgunKey, "ctl-old", box.Address, "Ben <"+approverAddress+">", "[GH-APPROVE:"+token+"]", "[GH-FEEDBACK-BEGIN]\r\n\r\n[GH-FEEDBACK-END]")
	if _, _, err := svc.IngestInbound(ctx, "mailgun", req); !errors.Is(err, transport.ErrInboundIgnored) {
		t.Fatalf("control ingest err=%v", err)
	}
	sr, err := svc.Store.GetSendRequestByDraft(ctx, asst, d.ID)
	if err != nil || sr.Status != model.SendRequestApproved || sr.DecisionActor != approverAddress {
		t.Fatalf("request after approver change %+v err=%v", sr, err)
	}
}

func TestFrozenFingerprintCoversAttachmentBytes(t *testing.T) {
	svc, u, _, box := testService(t)
	ctx := context.Background()
	asst := assistantPrincipal(u.AccountID, box.ID)
	admin := model.Principal{AccountID: u.AccountID, Admin: true}
	d, err := svc.Store.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s", Text: "b"})
	if err != nil {
		t.Fatal(err)
	}
	rel := "drafts/tamper.bin"
	path := filepath.Join(svc.Config.DataDir, filepath.FromSlash(rel))
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("AAAA"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Store.AddDraftAttachment(ctx, asst, d.ID, model.DraftAttachment{Filename: "a.bin", ContentType: "application/octet-stream", Size: 4, RawPath: rel}); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RequestSend(ctx, asst, d.ID, false); err != nil {
		t.Fatal(err)
	}
	// Swap the bytes with the same length; the metadata fingerprint would not
	// notice, but the byte hash must.
	if err = os.WriteFile(path, []byte("BBBB"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.ApproveDraft(ctx, admin, d.ID, "", model.DecisionMethodUI, ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("approve after byte swap err=%v", err)
	}
}

// controlRequestFor builds a pending external request and returns its live token.
func controlRequestFor(t *testing.T, svc *app.Service, u model.User, box model.Inbox) string {
	t.Helper()
	if err := svc.Store.SetInboxApprover(context.Background(), u.AccountID, box.ID, approverAddress); err != nil {
		t.Fatal(err)
	}
	asst := assistantPrincipal(u.AccountID, box.ID)
	d, err := svc.Store.CreateDraft(context.Background(), asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "proposal", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RequestSend(context.Background(), asst, d.ID, true); err != nil {
		t.Fatal(err)
	}
	return approvalToken(t, svc, u.AccountID, box.ID)
}

// TestExternalApprovalEnvelopeBinding proves the approval decision is bound to
// the provider-attested envelope sender, not the spoofable MIME From header.
func TestExternalApprovalEnvelopeBinding(t *testing.T) {
	ctx := context.Background()

	t.Run("spoofed MIME From is rejected", func(t *testing.T) {
		svc, u, dom, box := testService(t)
		svc.Config.ApprovalExpiryHours = 48
		seedInbound(t, svc, u.AccountID, dom.ID, "mailgun", map[string]any{"signing_key": testMailgunKey})
		token := controlRequestFor(t, svc, u, box)
		body := "[GH-FEEDBACK-BEGIN]\r\n\r\n[GH-FEEDBACK-END]"
		// Envelope is the attacker; the header claims the approver.
		req := mgControlRequestFrom(t, testMailgunKey, "ctl-spoof", box.Address, "mallory@evil.test", "Ben <"+approverAddress+">", "[GH-APPROVE:"+token+"]", body)
		if _, _, err := svc.IngestInbound(ctx, "mailgun", req); !errors.Is(err, transport.ErrInboundIgnored) {
			t.Fatalf("ingest err=%v", err)
		}
		if !pendingRequestStillOpen(t, svc, u, box) {
			t.Fatal("spoofed MIME From authorized the request")
		}
	})

	t.Run("spoofed header with approver envelope is rejected", func(t *testing.T) {
		svc, u, dom, box := testService(t)
		svc.Config.ApprovalExpiryHours = 48
		seedInbound(t, svc, u.AccountID, dom.ID, "mailgun", map[string]any{"signing_key": testMailgunKey})
		token := controlRequestFor(t, svc, u, box)
		body := "[GH-FEEDBACK-BEGIN]\r\n\r\n[GH-FEEDBACK-END]"
		req := mgControlRequestFrom(t, testMailgunKey, "ctl-hdr", box.Address, approverAddress, "Mallory <mallory@evil.test>", "[GH-APPROVE:"+token+"]", body)
		if _, _, err := svc.IngestInbound(ctx, "mailgun", req); !errors.Is(err, transport.ErrInboundIgnored) {
			t.Fatalf("ingest err=%v", err)
		}
		if !pendingRequestStillOpen(t, svc, u, box) {
			t.Fatal("spoofed header authorized the request")
		}
	})

	t.Run("missing envelope is rejected", func(t *testing.T) {
		svc, u, dom, box := testService(t)
		svc.Config.ApprovalExpiryHours = 48
		seedInbound(t, svc, u.AccountID, dom.ID, "mailgun", map[string]any{"signing_key": testMailgunKey})
		token := controlRequestFor(t, svc, u, box)
		body := "[GH-FEEDBACK-BEGIN]\r\n\r\n[GH-FEEDBACK-END]"
		req := mgControlRequestFrom(t, testMailgunKey, "ctl-none", box.Address, "", "Ben <"+approverAddress+">", "[GH-APPROVE:"+token+"]", body)
		if _, _, err := svc.IngestInbound(ctx, "mailgun", req); !errors.Is(err, transport.ErrInboundIgnored) {
			t.Fatalf("ingest err=%v", err)
		}
		if !pendingRequestStillOpen(t, svc, u, box) {
			t.Fatal("missing envelope authorized the request")
		}
	})

	t.Run("matching envelope and header approves", func(t *testing.T) {
		svc, u, dom, box := testService(t)
		svc.Config.ApprovalExpiryHours = 48
		seedInbound(t, svc, u.AccountID, dom.ID, "mailgun", map[string]any{"signing_key": testMailgunKey})
		token := controlRequestFor(t, svc, u, box)
		body := "[GH-FEEDBACK-BEGIN]\r\n\r\n[GH-FEEDBACK-END]"
		req := mgControlRequestFrom(t, testMailgunKey, "ctl-ok", box.Address, approverAddress, "Ben <"+approverAddress+">", "[GH-APPROVE:"+token+"]", body)
		if _, _, err := svc.IngestInbound(ctx, "mailgun", req); !errors.Is(err, transport.ErrInboundIgnored) {
			t.Fatalf("ingest err=%v", err)
		}
		if pendingRequestStillOpen(t, svc, u, box) {
			t.Fatal("valid envelope did not authorize the request")
		}
	})
}

// pendingRequestStillOpen reports whether the most recent send request for the
// inbox is still awaiting a decision.
func pendingRequestStillOpen(t *testing.T, svc *app.Service, u model.User, box model.Inbox) bool {
	t.Helper()
	srs, err := svc.Store.ListSendRequests(context.Background(), model.Principal{AccountID: u.AccountID, Admin: true}, box.ID, false, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(srs) == 0 {
		t.Fatal("no send request found")
	}
	for _, sr := range srs {
		if sr.Status == model.SendRequestPending {
			return true
		}
	}
	return false
}

// TestInternalApprovalMailCannotBeForwarded proves the hidden approval email
// cannot be exfiltrated through the reply/forward send path.
func TestInternalApprovalMailCannotBeForwarded(t *testing.T) {
	svc, u, _, box := testService(t)
	ctx := context.Background()
	if err := svc.Store.SetInboxApprover(ctx, u.AccountID, box.ID, approverAddress); err != nil {
		t.Fatal(err)
	}
	asst := assistantPrincipal(u.AccountID, box.ID)
	d, err := svc.Store.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "proposal", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RequestSend(ctx, asst, d.ID, true); err != nil {
		t.Fatal(err)
	}
	internalID := queuedApprovalEmail(t, svc, ctx, u.AccountID, box.ID).ID
	owner := ownerPrincipal(u.AccountID, box.ID)
	if _, err = svc.Send(ctx, owner, app.SendInput{InboxID: box.ID, ForwardOfMessageID: internalID, To: []string{"leak@outside.test"}, Text: "x"}, ""); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("forward of internal mail err=%v", err)
	}
	if _, err = svc.Send(ctx, owner, app.SendInput{InboxID: box.ID, ReplyToMessageID: internalID, To: []string{"leak@outside.test"}, Text: "x"}, ""); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("reply to internal mail err=%v", err)
	}
}
