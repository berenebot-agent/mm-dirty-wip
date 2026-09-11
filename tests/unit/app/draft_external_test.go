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

var approveTokenRe = regexp.MustCompile(`\[GH-APPROVE:([A-Za-z0-9_-]{20,})\]`)

// mgControlRequest builds a Mailgun multipart webhook with a chosen sender,
// subject and body so the approval control path can be exercised end to end.
func mgControlRequest(t *testing.T, key, deliveryID, recipient, sender, subject, body string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	raw := "From: " + sender + "\r\nTo: " + recipient + "\r\nSubject: " + subject + "\r\nMessage-ID: <ctl@test>\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\n" + body
	fields := map[string]string{"timestamp": ts, "token": deliveryID, "signature": mgSig(key, ts, deliveryID), "sender": sender, "recipient": recipient, "Message-Id": "<ctl@test>"}
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
	p := model.Principal{AccountID: accountID, Admin: true}
	msgs, err := svc.Store.ListOutbox(ctx, p, box.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if !strings.HasPrefix(m.Subject, "Approval required:") {
			continue
		}
		parsed, err := mailparse.ParseFile(filepath.Join(svc.Config.DataDir, filepath.FromSlash(m.RawPath)))
		if err != nil {
			t.Fatal(err)
		}
		if match := approveTokenRe.FindStringSubmatch(parsed.Text); len(match) == 2 {
			return match[1]
		}
	}
	t.Fatal("no approval token found in queued approval email")
	return ""
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
