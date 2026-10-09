package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestAPIMessageSanitizesHTML proves the single-message API returns sanitised
// HTML, not the raw email body, so an agent/LLM consumer cannot receive
// script-bearing markup.
func TestAPIMessageSanitizesHTML(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()

	m, _, _, err := svc.Store.CommitInbound(ctx, store.InboundRecord{
		Inbox: box, Provider: "mailgun", ProviderDeliveryID: "sanitize-1", RFCMessageID: "<s@test>",
		From: model.Address{Address: "sender@outside.test"}, To: []string{box.Address}, EnvelopeTo: []string{box.Address},
		Subject: "sanitize", Text: "hello",
		HTML:    `<p>hi</p><script>alert(1)</script><img src=x onerror=alert(2)>`,
		RawPath: "messages/s.eml", SizeBytes: 10, ReceivedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/v1/messages/"+m.ID, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("get message %d %s", rr.Code, rr.Body.String())
	}
	var got struct {
		HTML string `json:"html"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if strings.Contains(strings.ToLower(got.HTML), "<script") || strings.Contains(strings.ToLower(got.HTML), "onerror") {
		t.Fatalf("API returned unsanitised HTML: %q", got.HTML)
	}
	if !strings.Contains(got.HTML, "hi") {
		t.Fatalf("sanitised HTML lost benign content: %q", got.HTML)
	}
}
