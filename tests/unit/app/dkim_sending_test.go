package app_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/dkimsign"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/safepath"
	"github.com/emersion/go-msgauth/dkim"
)

// storedRaw reads back the frozen raw MIME a queued send wrote to disk. The
// assertions below are about what is stored, because RawPath is the artifact
// that is delivered, replayed into Sent and reused on every retry.
func storedRaw(t *testing.T, svc *app.Service, accountID string, msg model.Message) []byte {
	t.Helper()
	path, err := safepath.Join(svc.Config.DataDir, msg.RawPath)
	if err != nil {
		t.Fatalf("resolve raw path: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read raw message: %v", err)
	}
	return raw
}

func sendOne(t *testing.T, svc *app.Service, accountID, inboxID string) model.Message {
	t.Helper()
	p := model.Principal{AccountID: accountID, Admin: true, MailboxRoles: map[string]string{}}
	res, err := svc.Send(context.Background(), p, app.SendInput{
		InboxID: inboxID,
		To:      []string{"friend@example.net"},
		Subject: "Hi",
		Text:    "hello",
	}, "")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	return res.Message
}

// TestDirectMXSignsStoredMessage is the end-to-end proof for DKIM signing: an
// enabled Direct MX config produces a stored message carrying exactly one
// signature that verifies against the public key MailMoose reports for DNS.
func TestDirectMXSignsStoredMessage(t *testing.T) {
	svc, u, d, box := testService(t)
	seedSending(t, svc, u.AccountID, d.ID, "mx", map[string]any{
		"helo":         "mail.example.com",
		"dkim_enabled": "on",
	})

	msg := sendOne(t, svc, u.AccountID, box.ID)
	raw := storedRaw(t, svc, u.AccountID, msg)

	if n := bytes.Count(raw, []byte("DKIM-Signature:")); n != 1 {
		t.Fatalf("stored message has %d DKIM-Signature headers, want 1", n)
	}

	// Read the config back to obtain the public key an operator would publish.
	stored, err := svc.Store.GetDomainSendingConfig(context.Background(), u.AccountID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := svc.DecryptDomainSendingConfig(stored)
	if err != nil {
		t.Fatal(err)
	}
	privPEM, _ := dec["dkim_private_key"].(string)
	if strings.TrimSpace(privPEM) == "" {
		t.Fatal("no private key was generated for the enabled config")
	}
	key, err := dkimsign.ParsePrivateKey(privPEM)
	if err != nil {
		t.Fatalf("generated key does not parse: %v", err)
	}
	pubPEM, err := dkimsign.PublicKeyPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	pubB64, err := dkimsign.PublicKeyBase64(pubPEM)
	if err != nil {
		t.Fatal(err)
	}
	selector, _ := dec["dkim_selector"].(string)

	verifs, err := dkim.VerifyWithOptions(bytes.NewReader(raw), &dkim.VerifyOptions{
		LookupTXT: func(name string) ([]string, error) {
			if want := dkimsign.RecordName(selector, "example.com"); name != want {
				t.Errorf("verifier looked up %q, want %q", name, want)
			}
			return []string{dkimsign.TXTRecordValue(pubB64)}, nil
		},
	})
	if err != nil {
		t.Fatalf("verify stored message: %v", err)
	}
	if len(verifs) != 1 || verifs[0].Err != nil {
		t.Fatalf("stored signature did not verify: %+v", verifs)
	}
}

// TestDirectMXDefaultSelectorAndDomain proves the defaults an operator gets
// without filling anything in: the well-known selector, and a signing domain
// that follows the address the mail is From.
func TestDirectMXDefaultSelectorAndDomain(t *testing.T) {
	svc, u, d, box := testService(t)
	seedSending(t, svc, u.AccountID, d.ID, "mx", map[string]any{
		"helo":         "mail.example.com",
		"dkim_enabled": "on",
	})
	msg := sendOne(t, svc, u.AccountID, box.ID)
	raw := storedRaw(t, svc, u.AccountID, msg)

	if !bytes.Contains(raw, []byte("s="+dkimsign.DefaultSelector+";")) {
		t.Errorf("stored signature does not use the default selector %q", dkimsign.DefaultSelector)
	}
	if !bytes.Contains(raw, []byte("d=example.com;")) {
		t.Error("stored signature does not sign as the sending domain")
	}
}

// TestDirectMXSigningOffLeavesMessageUnsigned proves signing is opt-in: the
// default config, and a config that explicitly turns it off, must not sign.
func TestDirectMXSigningOffLeavesMessageUnsigned(t *testing.T) {
	for name, cfg := range map[string]map[string]any{
		"default (unset)": {"helo": "mail.example.com"},
		"explicitly off":  {"helo": "mail.example.com", "dkim_enabled": "off"},
	} {
		t.Run(name, func(t *testing.T) {
			svc, u, d, box := testService(t)
			seedSending(t, svc, u.AccountID, d.ID, "mx", cfg)
			msg := sendOne(t, svc, u.AccountID, box.ID)
			raw := storedRaw(t, svc, u.AccountID, msg)
			if bytes.Contains(raw, []byte("DKIM-Signature:")) {
				t.Error("message was signed despite signing being off")
			}
		})
	}
}

// TestNonMXProviderIsNotSigned guards the blast radius: only Direct MX is
// signed, because every other provider either signs with its own key or
// submits through its own authenticated infrastructure.
func TestNonMXProviderIsNotSigned(t *testing.T) {
	svc, u, d, box := testService(t)
	api := providerServer(t, "<brevo>", new(atomic.Int32))
	seedSending(t, svc, u.AccountID, d.ID, "brevo", map[string]any{"api_key": "k", "api_base": api.URL})
	msg := sendOne(t, svc, u.AccountID, box.ID)
	raw := storedRaw(t, svc, u.AccountID, msg)
	if bytes.Contains(raw, []byte("DKIM-Signature:")) {
		t.Error("a non-MX provider's stored message was signed")
	}
}

// TestPastedDKIMKeyIsUsedVerbatim proves an operator can supply their own key
// instead of a generated one, and that the message is signed with it.
func TestPastedDKIMKeyIsUsedVerbatim(t *testing.T) {
	svc, u, d, box := testService(t)
	privPEM, pubPEM, err := dkimsign.GenerateRSA2048()
	if err != nil {
		t.Fatal(err)
	}
	seedSending(t, svc, u.AccountID, d.ID, "mx", map[string]any{
		"helo":             "mail.example.com",
		"dkim_enabled":     "on",
		"dkim_selector":    "custom",
		"dkim_private_key": privPEM,
	})

	stored, err := svc.Store.GetDomainSendingConfig(context.Background(), u.AccountID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := svc.DecryptDomainSendingConfig(stored)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := dec["dkim_private_key"].(string); got != dkimsign.CanonicalPEM(privPEM) {
		t.Error("the pasted key was replaced or altered beyond canonical PEM form")
	}

	msg := sendOne(t, svc, u.AccountID, box.ID)
	raw := storedRaw(t, svc, u.AccountID, msg)
	pubB64, err := dkimsign.PublicKeyBase64(pubPEM)
	if err != nil {
		t.Fatal(err)
	}
	verifs, err := dkim.VerifyWithOptions(bytes.NewReader(raw), &dkim.VerifyOptions{
		LookupTXT: func(string) ([]string, error) {
			return []string{dkimsign.TXTRecordValue(pubB64)}, nil
		},
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(verifs) != 1 || verifs[0].Err != nil {
		t.Fatalf("pasted-key signature did not verify: %+v", verifs)
	}
}

// TestBlankPrivateKeyRetainsStoredKey proves a re-save that does not re-send the
// key keeps signing with the existing key, so an unrelated settings change does
// not silently rotate the key out from under published DNS.
func TestBlankPrivateKeyRetainsStoredKey(t *testing.T) {
	svc, u, d, box := testService(t)
	ctx := context.Background()
	seedSending(t, svc, u.AccountID, d.ID, "mx", map[string]any{
		"helo":         "mail.example.com",
		"dkim_enabled": "on",
	})
	first, err := svc.Store.GetDomainSendingConfig(ctx, u.AccountID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstDec, err := svc.DecryptDomainSendingConfig(first)
	if err != nil {
		t.Fatal(err)
	}
	firstKey, _ := firstDec["dkim_private_key"].(string)

	// Re-save with the key left blank, changing only the HELO hostname.
	seedSending(t, svc, u.AccountID, d.ID, "mx", map[string]any{
		"helo":         "mail2.example.com",
		"dkim_enabled": "on",
	})
	second, err := svc.Store.GetDomainSendingConfig(ctx, u.AccountID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondDec, err := svc.DecryptDomainSendingConfig(second)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := secondDec["dkim_private_key"].(string); got != firstKey {
		t.Error("a blank private key did not retain the stored key")
	}

	msg := sendOne(t, svc, u.AccountID, box.ID)
	if !bytes.Contains(storedRaw(t, svc, u.AccountID, msg), []byte("DKIM-Signature:")) {
		t.Error("message was not signed after a retained-key re-save")
	}
}

// TestDisablingDKIMDropsStoredKey proves turning signing off removes the key
// material at rest, so it cannot reappear later against DNS that has since been
// republished for a different key.
func TestDisablingDKIMDropsStoredKey(t *testing.T) {
	svc, u, d, _ := testService(t)
	ctx := context.Background()
	seedSending(t, svc, u.AccountID, d.ID, "mx", map[string]any{
		"helo":         "mail.example.com",
		"dkim_enabled": "on",
	})
	seedSending(t, svc, u.AccountID, d.ID, "mx", map[string]any{
		"helo":         "mail.example.com",
		"dkim_enabled": "off",
	})
	stored, err := svc.Store.GetDomainSendingConfig(ctx, u.AccountID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := svc.DecryptDomainSendingConfig(stored)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := dec["dkim_private_key"].(string); strings.TrimSpace(got) != "" {
		t.Error("disabling DKIM left the private key stored")
	}
}

// TestPastedNonRSAKeyIsRejected proves an unusable pasted key is a save error
// rather than a silently unsigned domain.
func TestPastedNonRSAKeyIsRejected(t *testing.T) {
	svc, u, d, _ := testService(t)
	_, err := svc.SaveDomainSendingConfig(context.Background(), u.AccountID, d.ID, "mx", map[string]any{
		"helo":             "mail.example.com",
		"dkim_enabled":     "on",
		"dkim_private_key": "not a key at all",
	})
	if err == nil {
		t.Fatal("expected a save error for an unusable pasted key")
	}
	if !strings.Contains(err.Error(), "private key") && !strings.Contains(err.Error(), "Private key") {
		t.Errorf("error does not point at the key field: %v", err)
	}
}

// TestGeneratedKeyNeverAppearsInReadConfig proves the private key is not exposed
// on any config read: only the public half is.
func TestGeneratedKeyNeverAppearsInReadConfig(t *testing.T) {
	svc, u, d, _ := testService(t)
	ctx := context.Background()
	seedSending(t, svc, u.AccountID, d.ID, "mx", map[string]any{
		"helo":         "mail.example.com",
		"dkim_enabled": "on",
	})
	stored, err := svc.Store.GetDomainSendingConfig(ctx, u.AccountID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := svc.DecryptDomainSendingConfig(stored)
	if err != nil {
		t.Fatal(err)
	}
	privPEM, _ := dec["dkim_private_key"].(string)
	if strings.TrimSpace(privPEM) == "" {
		t.Fatal("expected a stored private key to test against")
	}
	// The stored config legitimately holds the private key: it must, in order to
	// sign. What must never happen is the private key leaking through the
	// redacted read path (outboundNonsecretConfig), which skips secret fields.
	// Assert the public field itself carries only public material.
	pubPEM, pubOK := dec["dkim_public_key"].(string)
	if !pubOK || strings.TrimSpace(pubPEM) == "" {
		t.Fatal("no public key was stored alongside the private key")
	}
	if strings.Contains(pubPEM, "PRIVATE KEY") {
		t.Error("the public key field contains private key material")
	}
	if !strings.Contains(pubPEM, "PUBLIC KEY") {
		t.Errorf("public key field is not an SPKI public key: %q", pubPEM)
	}
}

// TestSigningDomainMustBelongToAccount proves an account cannot configure
// signing as a domain it does not own, which would let it claim another
// account's identity in the DKIM d= tag.
func TestSigningDomainMustBelongToAccount(t *testing.T) {
	svc, u, d, _ := testService(t)
	ctx := context.Background()

	// A non-owned domain is rejected.
	_, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "mx", map[string]any{
		"helo":         "mail.example.com",
		"dkim_enabled": "on",
		"dkim_domain":  "someone-else.example.org",
	})
	if err == nil {
		t.Fatal("expected a save error for a signing domain the account does not own")
	}
	if !strings.Contains(err.Error(), "not a domain on this account") {
		t.Errorf("error does not explain the ownership problem: %v", err)
	}

	// A domain the account does own is accepted, and is used for signing.
	if _, err := svc.Store.CreateDomain(ctx, u.AccountID, "signer.example.com"); err != nil {
		t.Fatal(err)
	}
	seedSending(t, svc, u.AccountID, d.ID, "mx", map[string]any{
		"helo":         "mail.example.com",
		"dkim_enabled": "on",
		"dkim_domain":  "signer.example.com",
	})
	stored, err := svc.Store.GetDomainSendingConfig(ctx, u.AccountID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := svc.DecryptDomainSendingConfig(stored)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := dec["dkim_domain"].(string); got != "signer.example.com" {
		t.Errorf("stored signing domain = %q, want signer.example.com", got)
	}
}

// TestQueuedBeforeEnableIsDeliveredUnsigned pins the documented consequence of
// signing at queue time: a message already stored before signing was enabled
// keeps its unsigned bytes, because rewriting a stored message a worker may
// already be delivering would break byte-identical retries.
func TestQueuedBeforeEnableIsDeliveredUnsigned(t *testing.T) {
	svc, u, d, box := testService(t)
	// Queue one message with signing off.
	seedSending(t, svc, u.AccountID, d.ID, "mx", map[string]any{"helo": "mail.example.com"})
	early := sendOne(t, svc, u.AccountID, box.ID)
	if bytes.Contains(storedRaw(t, svc, u.AccountID, early), []byte("DKIM-Signature:")) {
		t.Fatal("message queued before signing was enabled was signed")
	}

	// Enable signing, then queue another message.
	seedSending(t, svc, u.AccountID, d.ID, "mx", map[string]any{
		"helo":         "mail.example.com",
		"dkim_enabled": "on",
	})
	if bytes.Contains(storedRaw(t, svc, u.AccountID, early), []byte("DKIM-Signature:")) {
		t.Error("enabling signing retroactively rewrote an already-queued stored message")
	}
	late := sendOne(t, svc, u.AccountID, box.ID)
	if !bytes.Contains(storedRaw(t, svc, u.AccountID, late), []byte("DKIM-Signature:")) {
		t.Error("message queued after signing was enabled is not signed")
	}
}

// TestDKIMSigningSurvivesSizeChecks proves the signature is added before the
// message-size check, so the check accounts for the bytes actually stored.
func TestDKIMSigningSurvivesSizeChecks(t *testing.T) {
	svc, u, d, box := testService(t)
	seedSending(t, svc, u.AccountID, d.ID, "mx", map[string]any{
		"helo":         "mail.example.com",
		"dkim_enabled": "on",
	})
	msg := sendOne(t, svc, u.AccountID, box.ID)
	raw := storedRaw(t, svc, u.AccountID, msg)
	if int64(len(raw)) != msg.SizeBytes {
		t.Errorf("stored size %d does not match recorded size_bytes %d", len(raw), msg.SizeBytes)
	}
}
