package app_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport"
	"github.com/dellarb/mailmoose/internal/transport/imap"
)

func TestRemotePasswordsCannotMoveToAnotherEndpoint(t *testing.T) {
	t.Parallel()
	svc, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, MailboxRoles: map[string]string{box.ID: "owner"}}
	configureSecrets(t, rm, u, box, "imap-secret", "")
	if _, err := rm.ConfigureStandaloneRemote(ctx, p, box.ID, store.StandaloneRemoteUpdate{
		SMTPHost: "smtp.remote.example", SMTPPort: 587, SMTPUsername: "agent", SMTPSecurity: "starttls", SMTPPassword: "smtp-secret",
	}); err != nil {
		t.Fatal(err)
	}
	before, creds, err := svc.Store.GetStandaloneRemoteState(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	dials := 0
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) {
		dials++
		return nil, errors.New("unexpected dial")
	})
	for name, update := range map[string]store.StandaloneRemoteUpdate{
		"imap host":     {Host: "attacker.example"},
		"imap port":     {Port: 9143},
		"imap user":     {Username: "attacker"},
		"imap security": {Security: "tls"},
		"smtp host":     {SMTPHost: "attacker.example"},
		"smtp port":     {SMTPPort: 2525},
		"smtp user":     {SMTPUsername: "attacker"},
		"smtp security": {SMTPSecurity: "plain"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := rm.TestStandaloneRemote(ctx, p, box.ID, update); err == nil {
				t.Fatal("test accepted a changed endpoint with a blank password")
			}
			if _, err := rm.ConfigureStandaloneRemote(ctx, p, box.ID, update); err == nil {
				t.Fatal("save accepted a changed endpoint with a blank password")
			}
		})
	}
	if dials != 0 {
		t.Fatalf("rejected candidates dialed %d times", dials)
	}
	after, afterCreds, err := svc.Store.GetStandaloneRemoteState(ctx, u.AccountID, box.ID)
	if err != nil || !store.SameIMAPBinding(before.Remote, after.Remote) || !store.SameSMTPBinding(before.Remote, after.Remote) || creds != afterCreds {
		t.Fatalf("rejected update changed stored state: %v", err)
	}
	// Case/whitespace normalization and a namespace change retain the original secret.
	rm.SetRemoteDialer(func(_ context.Context, cfg imap.Config) (app.RemoteSession, error) {
		if cfg.Host != box.Remote.Host || cfg.Password != "imap-secret" {
			t.Errorf("wrong unchanged binding: host=%q", cfg.Host)
		}
		dials++
		return nil, errors.New("dial reached")
	})
	_, _ = rm.TestStandaloneRemote(ctx, p, box.ID, store.StandaloneRemoteUpdate{Host: " IMAP.REMOTE.EXAMPLE ", Namespace: "Archive"})
	if dials != 1 {
		t.Fatal("unchanged normalized endpoint did not use its stored password")
	}
	if _, err := rm.ConfigureStandaloneRemote(ctx, p, box.ID, store.StandaloneRemoteUpdate{Host: "new.example", IMAPPassword: "new-secret"}); err != nil {
		t.Fatal(err)
	}
	resolved, err := rm.ResolveRemote(ctx, u.AccountID, box.ID)
	if err != nil || resolved.Inbox.Remote.Host != "new.example" || resolved.Secrets.IMAPPassword != "new-secret" {
		t.Fatalf("explicit replacement failed: %v", err)
	}
	// A stale snapshot cannot overwrite either the new endpoint or its password.
	if err := svc.Store.SaveStandaloneRemote(ctx, before, creds.EncryptedIMAP, creds.EncryptedSMTP, store.ConfigVersion{Revision: creds.Revision}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale save: %v", err)
	}
}

func TestNewSMTPBindingCannotInheritIMAPPassword(t *testing.T) {
	t.Parallel()
	_, u, box, rm := remoteTestEnv(t)
	configureSecrets(t, rm, u, box, "imap-secret", "")
	dials := 0
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) {
		dials++
		return nil, errors.New("unexpected dial")
	})
	p := model.Principal{AccountID: u.AccountID, MailboxRoles: map[string]string{box.ID: "owner"}}
	_, err := rm.TestStandaloneRemote(context.Background(), p, box.ID, store.StandaloneRemoteUpdate{SMTPHost: "attacker.example"})
	if err == nil || dials != 0 {
		t.Fatalf("SMTP candidate inherited an IMAP secret: err=%v dials=%d", err, dials)
	}
}

func TestApprovalMarkersRequireIssuedTokens(t *testing.T) {
	t.Parallel()
	svc, u, _, box := testService(t)
	ctx := context.Background()
	seedInbound(t, svc, u.AccountID, box.DomainID, "mailgun", map[string]any{"signing_key": testMailgunKey})
	approver, err := svc.Store.CreateInbox(ctx, u.AccountID, box.DomainID, "approver", "Approver")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetInboxApprover(ctx, u.AccountID, box.ID, approver.Address); err != nil {
		t.Fatal(err)
	}
	deliver := func(id, recipient, subject, body string) model.Message {
		t.Helper()
		m, _, err := svc.IngestInbound(ctx, "mailgun", mgControlRequest(t, testMailgunKey, id, recipient, "colleague@outside.test", subject, body))
		if err != nil || m.ID == "" {
			t.Fatalf("ordinary marker mail was consumed: id=%s err=%v", id, err)
		}
		return m
	}
	fake := "[GH-REQUEST:NotAnIssuedToken123456]"
	deliver("no-pending", box.Address, "Operations note", "The format is "+fake)
	asst := assistantPrincipal(u.AccountID, box.ID)
	d, err := svc.Store.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"recipient@outside.test"}, Subject: "Review", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RequestSend(ctx, asst, d.ID, false); err != nil {
		t.Fatal(err)
	}
	token := approvalToken(t, svc, u.AccountID, box.ID)
	deliver("pending-fake", box.Address, "Re: discussion", "Thanks\n> "+fake)
	deliver("unknown-subject", box.Address, "[GH-APPROVE:NotAnIssuedToken123456]", "An example")
	text, _ := approvalEmailParts(t, svc, u.AccountID, box.ID)
	notification := deliver("approver-notification", approver.Address, "Approval needed", text)
	if !strings.Contains(notification.Text, token) {
		t.Fatal("approver could not read its approval notification")
	}
	// The same genuine token may not be exposed in the requesting inbox.
	_, _, err = svc.IngestInbound(ctx, "mailgun", mgControlRequest(t, testMailgunKey, "real-control", box.Address, "colleague@outside.test", "Re: Review", "> [GH-REQUEST:"+token+"]"))
	if !errors.Is(err, transport.ErrInboundIgnored) {
		t.Fatalf("genuine token was delivered: %v", err)
	}
	sr, err := svc.Store.GetSendRequestByDraft(ctx, asst, d.ID)
	if err != nil || sr.Status != model.SendRequestPending {
		t.Fatalf("outsider changed pending approval: %+v %v", sr, err)
	}
	_, _, err = svc.IngestInbound(ctx, "mailgun", mgControlRequest(t, testMailgunKey, "approver-decision", box.Address, approver.Address, "[GH-APPROVE:"+token+"]", "approve"))
	if !errors.Is(err, transport.ErrInboundIgnored) {
		t.Fatalf("managed approver reply: %v", err)
	}
	sr, err = svc.Store.GetSendRequestByDraft(ctx, asst, d.ID)
	if err != nil || sr.Status != model.SendRequestApproved {
		t.Fatalf("managed approver could not approve: %+v %v", sr, err)
	}
	_, _, err = svc.IngestInbound(ctx, "mailgun", mgControlRequest(t, testMailgunKey, "decided-token", box.Address, approver.Address, "Re: Review", "> [GH-REQUEST:"+token+"]"))
	if !errors.Is(err, transport.ErrInboundIgnored) {
		t.Fatalf("decided token became mailbox content: %v", err)
	}
}

func TestRemoteMXBackendCannotCrossAccounts(t *testing.T) {
	t.Parallel()
	svc, u, dom, box := mxService(t)
	ctx := context.Background()
	if _, err := svc.SaveAccountMXReceiver(ctx, adminP(u), app.AccountMXReceiverInput{URL: "https://own-receiver.example", BearerKey: "own-key"}); err != nil {
		t.Fatal(err)
	}
	seedInbound(t, svc, u.AccountID, dom.ID, app.RemoteMXProvider, map[string]any{})
	other, err := svc.Store.CreateAccountAndAdmin(ctx, "Other", "admin@other.test", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	d, err := svc.Store.CreateDomain(ctx, other.AccountID, "other.test")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := svc.Store.CreateInbox(ctx, other.AccountID, d.ID, "victim", "Victim")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveAccountMXReceiver(ctx, adminP(other), app.AccountMXReceiverInput{URL: "https://other-receiver.example", BearerKey: "other-key"}); err != nil {
		t.Fatal(err)
	}
	seedInbound(t, svc, other.AccountID, d.ID, app.RemoteMXProvider, map[string]any{})
	backend := svc.RemoteMXBackend(u.AccountID)
	result, err := backend.Resolve(ctx, d.Name, []string{foreign.Address})
	if err != nil || result.MachineCode != mxwire.CodeUnauthorized {
		t.Fatalf("foreign resolve: %+v %v", result, err)
	}
	allowed, err := backend.Resolve(ctx, dom.Name, []string{box.Address})
	if err != nil || len(allowed.Results) != 1 || !allowed.Results[0].Accept {
		t.Fatalf("own resolve: %+v %v", allowed, err)
	}
	for i, recipients := range [][]string{{foreign.Address}, {box.Address, foreign.Address}} {
		raw := mxControlRaw("sender@outside.test", foreign.Address, "Injected", fmt.Sprint(i))
		response, err := backend.Ingest(ctx, []string{dom.Name, d.Name}, mxwire.IngestMetadata{
			Recipients: recipients, EnvelopeFrom: "sender@outside.test", Size: int64(len(raw)), ContentDigest: mxwire.BodyDigest([]byte(raw)),
			AuthResults: mxwire.AuthResults{DMARC: &mxwire.DMARCEvidence{Result: "pass"}},
		}, stageMX(t, svc, raw), "https://receiver.example")
		if err != nil || response.MachineCode != mxwire.CodeUnauthorized {
			t.Fatalf("foreign ingest: %+v %v", response, err)
		}
	}
	for _, account := range []model.User{u, other} {
		events, err := svc.Store.ListEvents(ctx, adminP(account), 0, "", 100)
		if err != nil || len(events) != 0 {
			t.Fatalf("rejected ingest produced events: %+v %v", events, err)
		}
	}
	raw := mxControlRaw("sender@outside.test", box.Address, "Own account", "body")
	meta := mxwire.IngestMetadata{Recipients: []string{box.Address}, EnvelopeFrom: "sender@outside.test", Size: int64(len(raw)), ContentDigest: mxwire.BodyDigest([]byte(raw))}
	for _, duplicate := range []bool{false, true} {
		response, err := backend.Ingest(ctx, []string{dom.Name}, meta, stageMX(t, svc, raw), "https://receiver.example")
		want := mxwire.CodeOK
		if duplicate {
			want = mxwire.CodeDuplicate
		}
		if err != nil || len(response.PerRecipient) != 1 || response.PerRecipient[0].MachineCode != want {
			t.Fatalf("own ingest (duplicate=%v): %+v %v", duplicate, response, err)
		}
	}
}
