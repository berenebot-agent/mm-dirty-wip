package app_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/mailparse"
	"github.com/dellarb/mailmoose/internal/model"
)

func TestForwardCarriesBodyAndAttachments(t *testing.T) {
	svc, u, dom, box := testService(t)
	ctx := context.Background()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"messageId":"<brevo-fwd>"}`)
	}))
	defer api.Close()
	seedSending(t, svc, u.AccountID, dom.ID, "brevo", map[string]any{"api_key": "xkeysib-test", "api_base": api.URL})
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}
	original, err := svc.Send(ctx, p, app.SendInput{
		InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Report", Text: "body text",
		Attachments: []app.SendAttachment{{Filename: "report.txt", ContentType: "text/plain", Content: []byte("hello attachment")}},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	forwarded, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"elsewhere@example.net"}, ForwardOfMessageID: original.Message.ID}, "")
	if err != nil {
		t.Fatal(err)
	}
	if forwarded.Message.Subject != "Fwd: Report" {
		t.Fatalf("subject %q", forwarded.Message.Subject)
	}
	if !strings.Contains(forwarded.Message.Text, "---------- Forwarded message ----------") || !strings.Contains(forwarded.Message.Text, "body text") {
		t.Fatalf("forward body %q", forwarded.Message.Text)
	}
	if forwarded.Message.ThreadID == original.Message.ThreadID {
		t.Fatal("forward should start a new conversation")
	}
	atts, err := svc.Store.ListAttachments(ctx, p, forwarded.Message.ID)
	if err != nil || len(atts) != 1 || atts[0].Filename != "report.txt" {
		t.Fatalf("forward attachments %v %#v", err, atts)
	}
	path := filepath.Join(svc.Config.DataDir, filepath.FromSlash(forwarded.Message.RawPath))
	var buf bytes.Buffer
	if err = mailparse.ExtractAttachment(path, atts[0].PartIndex, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "hello attachment" {
		t.Fatalf("forwarded attachment bytes %q", buf.String())
	}
}
