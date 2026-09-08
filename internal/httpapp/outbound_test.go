package httpapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gatehouse-mail/internal/model"
)

func TestAPISendWithBase64Attachment(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"messageId":"<brevo-http>"}`)
	}))
	defer api.Close()
	cred, err := svc.SaveOutboundCredential(ctx, u.AccountID, "", "Brevo", "brevo", map[string]any{"api_key": "k", "api_base": api.URL})
	if err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}
	cid := cred.ID
	if err = svc.Store.UpdateInbox(ctx, p, box.ID, "", nil, &cid); err != nil {
		t.Fatal(err)
	}
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "owner", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"inbox_id": box.ID,
		"to":       []string{"friend@example.net"},
		"subject":  "Report",
		"text":     "See attached",
		"attachments": []map[string]any{{
			"filename":     "report.txt",
			"content_type": "text/plain",
			"content":      base64.StdEncoding.EncodeToString([]byte("http attachment")),
		}},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", "/v1/send", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || calls.Load() != 1 {
		t.Fatalf("send %d %s calls=%d", rr.Code, rr.Body.String(), calls.Load())
	}
}
