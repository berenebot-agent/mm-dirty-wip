package app_test

import (
	"context"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/mxwire"
)

// mxControlRaw builds a control reply MIME with the given envelope/header
// sender and a control subject, for the MX control path.
func mxControlRaw(from, to, subject, body string) string {
	return "From: " + from + "\r\nTo: " + to + "\r\nSubject: " + subject +
		"\r\nMessage-ID: <ctl@mx>\r\nDate: Mon, 07 Sep 2026 10:00:00 +0000\r\n\r\n" + body
}

// mxControlInput stages a control message and returns it as an MX ingest input
// with a chosen envelope sender and auth evidence.
func mxControlInput(t *testing.T, svc *app.Service, recipient, envelopeFrom, raw string, auth mxwire.AuthResults) app.MXIngestInput {
	t.Helper()
	return app.MXIngestInput{
		Recipients:    []string{recipient},
		EnvelopeFrom:  envelopeFrom,
		RawPath:       stageMX(t, svc, raw),
		Size:          int64(len(raw)),
		ContentDigest: mxwire.BodyDigest([]byte(raw)),
		AuthResults:   auth,
		TrustedAuth:   true,
	}
}

// TestMXControlApprovalRequiresAuthEvidence covers vuln-0002: an MX approval
// whose From domain is not authenticated by the edge's trusted SPF/DKIM/DMARC
// evidence must not authorise the send, and authenticated evidence must. It
// also covers vuln-0001: a byte-identical replay is deduplicated by the
// recorded control receipt.
func TestMXControlApprovalRequiresAuthEvidence(t *testing.T) {
	svc, u, _, box := mxService(t)
	svc.Config.ApprovalExpiryHours = 48
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
	handOffWorkflow(t, svc, u.AccountID, box.ID)
	token := approvalToken(t, svc, u.AccountID, box.ID)
	subject := "[GH-APPROVE:" + token + "]"

	// No trusted auth evidence: the decision must not run.
	raw := mxControlRaw(approverAddress, box.Address, subject, "approve without auth")
	res, err := svc.IngestMX(ctx, mxControlInput(t, svc, box.Address, approverAddress, raw, mxwire.AuthResults{}))
	if err != nil {
		t.Fatal(err)
	}
	if r := mxResult(t, res); r.Disposition != mxwire.DispositionControl || r.MachineCode != mxwire.CodeOK {
		t.Fatalf("unauthenticated control result %+v", r)
	}
	sr, err := svc.Store.GetSendRequestByDraft(ctx, asst, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sr.Status != model.SendRequestPending {
		t.Fatalf("unauthenticated evidence authorised the decision: %+v", sr)
	}

	// A DMARC pass authenticates the From domain and the approval applies.
	rawAuth := mxControlRaw(approverAddress, box.Address, subject, "approve with auth")
	auth := mxwire.AuthResults{DMARC: &mxwire.DMARCEvidence{Result: "pass"}}
	resAuth, err := svc.IngestMX(ctx, mxControlInput(t, svc, box.Address, approverAddress, rawAuth, auth))
	if err != nil {
		t.Fatal(err)
	}
	if r := mxResult(t, resAuth); r.Disposition != mxwire.DispositionControl || r.MachineCode != mxwire.CodeOK {
		t.Fatalf("authenticated control result %+v", r)
	}
	srAuth, err := svc.Store.GetSendRequestByDraft(ctx, asst, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if srAuth.Status != model.SendRequestApproved {
		t.Fatalf("authenticated approval did not apply: %+v", srAuth)
	}

	// A byte-identical replay is deduplicated before changing the receive path.
	resDup, err := svc.IngestMX(ctx, mxControlInput(t, svc, box.Address, approverAddress, rawAuth, auth))
	if err != nil {
		t.Fatal(err)
	}
	if r := mxResult(t, resDup); !r.Duplicate || r.MachineCode != mxwire.CodeDuplicate {
		t.Fatalf("control replay was not deduplicated: %+v", r)
	}

	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, box.DomainID, "dialmx", map[string]any{"receiver_urls": "https://receiver.example"}, false); err != nil {
		t.Fatal(err)
	}
	secondDraft, err := svc.Store.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"next@y.test"}, Subject: "proposal two", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RequestSend(ctx, asst, secondDraft.ID, true); err != nil {
		t.Fatal(err)
	}
	handOffWorkflow(t, svc, u.AccountID, box.ID)
	dialRaw := mxControlRaw(approverAddress, box.Address, "[GH-APPROVE:"+approvalToken(t, svc, u.AccountID, box.ID)+"]", "approve with Dial MX")
	resDial, err := svc.IngestDialMX(ctx, mxControlInput(t, svc, box.Address, approverAddress, dialRaw, auth))
	if err != nil {
		t.Fatal(err)
	}
	if got := mxResult(t, resDial); got.MachineCode != mxwire.CodeOK {
		t.Fatalf("Dial MX control auth not accepted: %+v", got)
	}
	if _, err = svc.Store.LookupMXReceipt(ctx, u.AccountID, "mx", box.Address, mxwire.DeliveryFingerprint(approverAddress, box.Address, mxwire.BodyDigest([]byte(dialRaw)))); err != nil {
		t.Fatalf("Dial MX receipt identity changed: %v", err)
	}

}
