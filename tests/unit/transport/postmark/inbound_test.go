package postmark_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/transport"
	"github.com/dellarb/mailmoose/internal/transport/postmark"
)

var _ transport.InboundTransport = postmark.Transport{}

type fakeResolver struct {
	user, pass string
	err        error
}

func (f fakeResolver) ResolveInboundBinding(_ context.Context, provider, recipient string) (transport.InboundBinding, error) {
	if f.err != nil {
		return transport.InboundBinding{}, f.err
	}
	return transport.InboundBinding{
		AccountID: "acc", DomainID: "dom", CredentialID: "cred",
		Provider: provider, Recipient: recipient,
		Config: map[string]any{"username": f.user, "password": f.pass},
	}, nil
}

func basicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// request builds a Postmark webhook with RawEmail as a JSON string.
func request(t *testing.T, user, pass, recipient, rawEmail, messageID string) *http.Request {
	t.Helper()
	payload := map[string]any{
		"From":              "Sender <b@test>",
		"OriginalRecipient": recipient,
		"MessageID":         messageID,
		"RawEmail":          rawEmail,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if user != "" || pass != "" {
		r.Header.Set("Authorization", basicAuth(user, pass))
	}
	return r
}

func TestReceiveValidBasicAuth(t *testing.T) {
	var tr postmark.Transport
	raw := "From: b@test\r\nTo: a@example.com\r\nSubject: hi\r\n\r\nhello"
	path := t.TempDir() + "/m.eml"
	msg, binding, err := tr.Receive(context.Background(), request(t, "u", "p", "a@example.com", raw, "pm-1"), fakeResolver{user: "u", pass: "p"}, path, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Provider != "postmark" || msg.Recipient != "a@example.com" {
		t.Fatalf("msg %+v", msg)
	}
	if msg.DeliveryID != "pm-1" || msg.ProviderMessageID != "pm-1" {
		t.Fatalf("delivery %q provider %q", msg.DeliveryID, msg.ProviderMessageID)
	}
	if binding.AccountID != "acc" {
		t.Fatalf("binding %+v", binding)
	}
	staged, _ := os.ReadFile(msg.RawPath)
	if !strings.Contains(string(staged), "hello") {
		t.Fatal("raw mime missing")
	}
}

func TestReceiveWrongCredentialsRejected(t *testing.T) {
	var tr postmark.Transport
	raw := "From: b@test\r\n\r\nhi"
	if _, _, err := tr.Receive(context.Background(), request(t, "u", "wrong", "a@example.com", raw, "pm-1"), fakeResolver{user: "u", pass: "p"}, t.TempDir()+"/m.eml", 4096); err != transport.ErrInboundUnauthorized {
		t.Fatalf("err=%v", err)
	}
}

func TestReceiveMissingCredentialsRejected(t *testing.T) {
	var tr postmark.Transport
	raw := "From: b@test\r\n\r\nhi"
	if _, _, err := tr.Receive(context.Background(), request(t, "", "", "a@example.com", raw, "pm-1"), fakeResolver{user: "u", pass: "p"}, t.TempDir()+"/m.eml", 4096); err != transport.ErrInboundUnauthorized {
		t.Fatalf("err=%v", err)
	}
}

func TestReceiveMissingRawEmailIsTerminal(t *testing.T) {
	var tr postmark.Transport
	if _, _, err := tr.Receive(context.Background(), request(t, "u", "p", "a@example.com", "", "pm-1"), fakeResolver{user: "u", pass: "p"}, t.TempDir()+"/m.eml", 4096); err == nil || !strings.Contains(err.Error(), "raw email missing") {
		t.Fatalf("err=%v", err)
	}
}

func TestReceiveMissingRecipientRejected(t *testing.T) {
	var tr postmark.Transport
	raw := "From: b@test\r\n\r\nhi"
	if _, _, err := tr.Receive(context.Background(), request(t, "u", "p", "", raw, "pm-1"), fakeResolver{user: "u", pass: "p"}, t.TempDir()+"/m.eml", 4096); err != transport.ErrInboundUnauthorized {
		t.Fatalf("err=%v", err)
	}
}

// TestReceivePreservesNonUTF8RawEmail proves the raw MIME bytes are extracted
// without being coerced through a UTF-8 string, so binary parts survive.
func TestReceivePreservesNonUTF8RawEmail(t *testing.T) {
	var tr postmark.Transport
	// A raw MIME body containing a lone 0xFF byte and a literal tab, embedded in
	// JSON via a \u00ff escape (which Go's json.Marshal produces for invalid
	// UTF-8 only if we hand it bytes; here we build the JSON by hand).
	body := []byte(`{"From":"b@test","OriginalRecipient":"a@example.com","MessageID":"pm-2","RawEmail":"From: b@test\r\n\r\n` + "\\u00ff" + `tab\ttab"}`)
	r := httptest.NewRequest("POST", "/", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", basicAuth("u", "p"))
	path := t.TempDir() + "/m.eml"
	msg, _, err := tr.Receive(context.Background(), r, fakeResolver{user: "u", pass: "p"}, path, 4096)
	if err != nil {
		t.Fatal(err)
	}
	staged, _ := os.ReadFile(msg.RawPath)
	if !bytes.Contains(staged, []byte("tab\ttab")) {
		t.Fatalf("tab escape not decoded: %q", staged)
	}
	if !bytes.Contains(staged, []byte{0xc3, 0xbf}) {
		t.Fatalf("\\u00ff not decoded to UTF-8: %q", staged)
	}
}

func TestReceiveRejectsOversize(t *testing.T) {
	var tr postmark.Transport
	raw := strings.Repeat("a", 8192)
	if _, _, err := tr.Receive(context.Background(), request(t, "u", "p", "a@example.com", raw, "pm-1"), fakeResolver{user: "u", pass: "p"}, t.TempDir()+"/m.eml", 1024); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err=%v", err)
	}
}

func TestReceiveRejectsBadContentType(t *testing.T) {
	var tr postmark.Transport
	r := httptest.NewRequest("POST", "/", strings.NewReader("x"))
	r.Header.Set("Content-Type", "text/plain")
	if _, _, err := tr.Receive(context.Background(), r, fakeResolver{user: "u", pass: "p"}, t.TempDir()+"/m.eml", 4096); err == nil {
		t.Fatal("bad content type accepted")
	}
}
