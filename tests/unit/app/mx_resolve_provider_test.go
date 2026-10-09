package app_test

import (
	"context"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/mxwire"
)

// seedRemoteMXReceiver registers this account's Remote MX receiver, which the
// store requires before a domain may select the "remotemx" provider. It mirrors
// an administrator saving the receiver in the UI.
func seedRemoteMXReceiver(t *testing.T, svc *app.Service, u model.User) {
	t.Helper()
	if _, err := svc.SaveAccountMXReceiver(context.Background(), adminP(u), app.AccountMXReceiverInput{
		URL: "https://mx.example.test", BearerKey: "test-bearer-key",
	}); err != nil {
		t.Fatalf("save account Remote MX receiver: %v", err)
	}
}

// TestResolveMXRecipientsAcceptsEitherSelfHostedProviderSpelling is the
// regression test for the provider-spelling mismatch that made every recipient
// unresolvable on a domain configured for the per-account Remote MX receiver.
//
// The self-hosted family is named by two spellings: the embedded/private MX edge
// ("mx") and the per-account Remote MX receiver ("remotemx"). A session backend
// resolves against its own spelling, while the domain's receiving config records
// the operator's choice from the receiving dialog. Resolving against only one
// spelling rejected the other as an unknown recipient, which the sender sees as
// "550 5.1.1 Unknown recipient" — indistinguishable from a genuinely absent
// mailbox. Both directions are asserted so a future change cannot fix one
// spelling and break the other.
func TestResolveMXRecipientsAcceptsEitherSelfHostedProviderSpelling(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured string
		prepare    func(*testing.T, *app.Service, model.User)
	}{
		{"embedded mx config", "mx", nil},
		{"remote mx config", app.RemoteMXProvider, seedRemoteMXReceiver},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, u, d, box := mxService(t)
			if tc.prepare != nil {
				tc.prepare(t, svc, u)
			}

			// mxService seeds "mx"; overwrite so each case exercises its own
			// spelling.
			if _, _, err := svc.SaveDomainReceivingConfig(
				context.Background(), u.AccountID, d.ID, tc.configured, map[string]any{}, false,
			); err != nil {
				t.Fatalf("save receiving config %q: %v", tc.configured, err)
			}

			// Resolve through the self-hosted entry point, the shape a
			// self-hosted receiver serves.
			results := svc.ResolveMXRecipients(context.Background(), []string{box.Address})
			if len(results) != 1 {
				t.Fatalf("expected one result, got %+v", results)
			}
			r := results[0]
			if !r.Accept {
				t.Fatalf("domain configured %q must resolve its own inbox; got code=%s accept=%v",
					tc.configured, r.Code, r.Accept)
			}
			if r.Code != mxwire.CodeOK {
				t.Fatalf("expected CodeOK, got %s", r.Code)
			}
		})
	}
}

// TestResolveMXRecipientsStillRejectsUnknownRecipient guards the other half of
// the fix: accepting either self-hosted spelling must not turn the resolve into
// a blanket accept. An address with no inbox, alias or catch-all on a configured
// domain stays unknown, so provider tolerance does not leak mailbox existence.
func TestResolveMXRecipientsStillRejectsUnknownRecipient(t *testing.T) {
	svc, u, d, _ := mxService(t)
	seedRemoteMXReceiver(t, svc, u)
	if _, _, err := svc.SaveDomainReceivingConfig(
		context.Background(), u.AccountID, d.ID, app.RemoteMXProvider, map[string]any{}, false,
	); err != nil {
		t.Fatal(err)
	}

	results := svc.ResolveMXRecipients(context.Background(), []string{"nobody@example.com"})
	if len(results) != 1 {
		t.Fatalf("expected one result, got %+v", results)
	}
	if results[0].Accept {
		t.Fatalf("a missing mailbox must not be accepted: %+v", results[0])
	}
	if results[0].Code != mxwire.CodeUnknownRecipient {
		t.Fatalf("expected CodeUnknownRecipient, got %s", results[0].Code)
	}
}
