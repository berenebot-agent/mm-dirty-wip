package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/dkimsign"
)

// TestDomainSendingDKIMResponse is the security-relevant test for DKIM on the
// per-domain sending API: the private key must never be echoed, while the
// public key, record name and TXT value the operator needs must always be.
func TestDomainSendingDKIMResponse(t *testing.T) {
	svc, h, u, dom, _ := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/v1/admin/domains/"+dom.ID+"/sending", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	// Enable DKIM with a generated key. The private key is not submitted.
	rr := do("PUT", `{"provider":"mx","config":{"helo":"mail.example.com","dkim_enabled":"on"}}`)
	if rr.Code != 200 {
		t.Fatalf("put mx sending %d %s", rr.Code, rr.Body.String())
	}
	resp := decodeDomainConfig(t, rr)

	// The public key and the records derived from it must be present.
	if resp.PublicKey == "" {
		t.Error("public_key is empty")
	}
	if resp.TXTRecord == "" || !strings.HasPrefix(resp.TXTRecord, "v=DKIM1; k=rsa; p=") {
		t.Errorf("txt_record is not a DKIM record: %q", resp.TXTRecord)
	}
	if resp.DKIMRecordName == "" {
		t.Error("dkim_record_name is empty")
	}
	if !strings.HasSuffix(resp.DKIMRecordName, dom.Name) {
		t.Errorf("dkim_record_name %q does not end with the domain %q", resp.DKIMRecordName, dom.Name)
	}
	if resp.KeyID != dkimsign.DefaultSelector {
		t.Errorf("selector = %q, want the default %q", resp.KeyID, dkimsign.DefaultSelector)
	}
	// The p= value must be the public half of the stored private key.
	wantB64, err := dkimsign.PublicKeyBase64(resp.PublicKey)
	if err != nil {
		t.Fatalf("returned public key does not parse: %v", err)
	}
	if want := dkimsign.TXTRecordValue(wantB64); resp.TXTRecord != want {
		t.Errorf("txt_record %q does not match the returned public key", resp.TXTRecord)
	}

	// The private key must never appear anywhere in the response.
	body := rr.Body.String()
	for _, marker := range []string{"PRIVATE KEY", "dkim_private_key"} {
		if strings.Contains(body, marker) {
			t.Errorf("response leaks %q: %s", marker, body)
		}
	}

	// A follow-up GET has no stored private key to leak either, and still
	// reports the public key so an operator can return to the page for DNS.
	rr = do("GET", "")
	if rr.Code != 200 {
		t.Fatalf("get %d %s", rr.Code, rr.Body.String())
	}
	got := decodeDomainConfig(t, rr)
	if got.PublicKey != resp.PublicKey {
		t.Error("public key changed between the PUT response and a GET")
	}
	if strings.Contains(rr.Body.String(), "PRIVATE KEY") {
		t.Error("GET response leaks the private key")
	}
}

// TestDomainSendingDKIMPublicKeyIsDerived proves the config read path reports a
// public key derived from the stored private key, so a stored pair can never
// disagree and send an operator to a DNS record that does not match what
// MailMoose signs with.
func TestDomainSendingDKIMPublicKeyIsDerived(t *testing.T) {
	svc, h, u, dom, _ := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	privPEM, pubPEM, err := dkimsign.GenerateRSA2048()
	if err != nil {
		t.Fatal(err)
	}
	// Submit an explicit pair; the public half is not accepted from the caller,
	// so the response must report the one derived from this private key.
	body := `{"provider":"mx","config":{"helo":"mail.example.com","dkim_enabled":"on","dkim_selector":"sel1","dkim_private_key":` +
		jsonString(privPEM) + `}}`
	req := httptest.NewRequest("PUT", "/v1/admin/domains/"+dom.ID+"/sending", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("put %d %s", rr.Code, rr.Body.String())
	}
	resp := decodeDomainConfig(t, rr)
	if strings.TrimSpace(resp.PublicKey) != strings.TrimSpace(pubPEM) {
		t.Error("reported public key is not the public half of the stored private key")
	}
	if resp.KeyID != "sel1" {
		t.Errorf("selector = %q, want sel1", resp.KeyID)
	}
	if want := dkimsign.RecordName("sel1", dom.Name); resp.DKIMRecordName != want {
		t.Errorf("record name = %q, want %q", resp.DKIMRecordName, want)
	}
}

// decodeDomainConfig decodes a sending/receiving config response.
func decodeDomainConfig(t *testing.T, rr *httptest.ResponseRecorder) domainConfigJSON {
	t.Helper()
	var out domainConfigJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rr.Body.String())
	}
	return out
}

// domainConfigJSON mirrors only the fields these tests assert on.
type domainConfigJSON struct {
	Provider       string         `json:"provider"`
	Config         map[string]any `json:"config"`
	PublicKey      string         `json:"public_key"`
	TXTRecord      string         `json:"txt_record"`
	KeyID          string         `json:"key_id"`
	DKIMRecordName string         `json:"dkim_record_name"`
}
