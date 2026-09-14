package mailparse_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"gatehouse-mail/internal/mailparse"
)

// Mirror internal/mailparse unexported limits (internal/mailparse/parse.go).
const (
	testMaxMIMEDepth = 8
	testMaxMIMEParts = 256
)

func TestParseAndExtractAttachment(t *testing.T) {
	raw := strings.Join([]string{
		"From: Alice <alice@example.net>", "To: box@example.com", "Subject: Test", "Message-ID: <one@test>", "MIME-Version: 1.0", "Content-Type: multipart/mixed; boundary=x", "", "--x", "Content-Type: text/plain; charset=utf-8", "", "hello body", "--x", "Content-Type: text/html; name=attack.html", "Content-Disposition: attachment; filename=attack.html", "Content-Transfer-Encoding: base64", "", "PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==", "--x--", ""}, "\r\n")
	path := t.TempDir() + "/m.eml"
	os.WriteFile(path, []byte(raw), 0600)
	p, err := mailparse.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Text != "hello body" || len(p.Attachments) != 1 || p.Attachments[0].Filename != "attack.html" {
		t.Fatalf("%+v", p)
	}
	var b bytes.Buffer
	if err = mailparse.ExtractAttachment(path, p.Attachments[0].PartIndex, &b); err != nil {
		t.Fatal(err)
	}
	if b.String() != "<script>alert(1)</script>" {
		t.Fatalf("%q", b.String())
	}
}
func TestBuildMessageAttachmentRoundTrip(t *testing.T) {
	raw, err := mailparse.BuildMessage(mailparse.Address{Name: "Hermes", Address: "hermes@example.com"}, []string{"friend@example.net"}, []string{"cc@example.net"}, []string{"bcc@example.net"}, "Report", "See attached", "", "<m1@example.com>", "", nil, time.Now(), []mailparse.Attachment{{Filename: "report.txt", ContentType: "text/plain", Content: []byte("hello attachment")}})
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/m.eml"
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	p, err := mailparse.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Text != "See attached" || len(p.Attachments) != 1 || p.Attachments[0].Filename != "report.txt" {
		t.Fatalf("%+v", p)
	}
	var b bytes.Buffer
	if err = mailparse.ExtractAttachment(path, p.Attachments[0].PartIndex, &b); err != nil {
		t.Fatal(err)
	}
	if b.String() != "hello attachment" {
		t.Fatalf("attachment %q", b.String())
	}
}

func TestBuildMessageHTMLAttachmentRoundTrip(t *testing.T) {
	raw, err := mailparse.BuildMessage(mailparse.Address{Address: "hermes@example.com"}, []string{"friend@example.net"}, nil, nil, "Report", "text body", "<p>html body</p>", "<m2@example.com>", "", nil, time.Now(), []mailparse.Attachment{{Filename: "report.txt", ContentType: "text/plain", Content: []byte("hello attachment")}})
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/m.eml"
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	p, err := mailparse.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Text != "text body" || !strings.Contains(p.HTML, "html body") || len(p.Attachments) != 1 {
		t.Fatalf("%+v", p)
	}
}

func TestHTMLIsPreserved(t *testing.T) {
	raw := "From: a@b.test\r\nTo: c@d.test\r\nContent-Type: text/html\r\n\r\n<p>Hello</p>"
	path := t.TempDir() + "/m"
	os.WriteFile(path, []byte(raw), 0600)
	p, err := mailparse.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.HTML, "<p>Hello</p>") {
		t.Fatalf("raw html not preserved: %q", p.HTML)
	}
}

// BCC recipients must never appear in the generated MIME; they are carried only
// in the provider/SMTP envelope.
func TestBuildMessageOmitsBccHeader(t *testing.T) {
	raw, err := mailparse.BuildMessage(mailparse.Address{Address: "hermes@example.com"}, []string{"friend@example.net"}, nil, []string{"secret@example.net"}, "Subject", "body", "", "<m@example.com>", "", nil, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(raw)), "bcc:") {
		t.Fatalf("generated MIME must not contain a Bcc header:\n%s", raw)
	}
	if strings.Contains(string(raw), "secret@example.net") {
		t.Fatalf("BCC recipient leaked into MIME:\n%s", raw)
	}
}

func TestParseRejectsDeepNesting(t *testing.T) {
	// Build a deeply nested multipart message exceeding maxMIMEDepth.
	var b strings.Builder
	depth := testMaxMIMEDepth + 2
	for i := 0; i < depth; i++ {
		fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=b%d\r\n\r\n--b%d\r\n", i, i)
	}
	b.WriteString("Content-Type: text/plain\r\n\r\nbody\r\n")
	for i := depth - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "--b%d--\r\n", i)
	}
	raw := "From: a@b.test\r\nTo: c@d.test\r\n" + b.String()
	path := t.TempDir() + "/deep.eml"
	os.WriteFile(path, []byte(raw), 0600)
	if _, err := mailparse.ParseFile(path); err == nil || !strings.Contains(err.Error(), "nesting too deep") {
		t.Fatalf("expected nesting error, got %v", err)
	}
}

func TestParseRejectsTooManyParts(t *testing.T) {
	var b strings.Builder
	b.WriteString("Content-Type: multipart/mixed; boundary=x\r\n\r\n")
	for i := 0; i < testMaxMIMEParts+1; i++ {
		fmt.Fprintf(&b, "--x\r\nContent-Type: text/plain\r\n\r\npart %d\r\n", i)
	}
	b.WriteString("--x--\r\n")
	raw := "From: a@b.test\r\nTo: c@d.test\r\n" + b.String()
	path := t.TempDir() + "/many.eml"
	os.WriteFile(path, []byte(raw), 0600)
	if _, err := mailparse.ParseFile(path); err == nil || !strings.Contains(err.Error(), "too many mime parts") {
		t.Fatalf("expected part-count error, got %v", err)
	}
}

// TestParseHonoursConfiguredLimits proves the traversal bounds are wired from
// configuration rather than the package defaults.
func TestParseHonoursConfiguredLimits(t *testing.T) {
	// Two levels of nesting is fine by default but rejected when the configured
	// depth is 1.
	var b strings.Builder
	b.WriteString("Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\n")
	b.WriteString("Content-Type: text/plain\r\n\r\npart\r\n--x--\r\n")
	raw := "From: a@b.test\r\nTo: c@d.test\r\n" + b.String()
	path := t.TempDir() + "/configured.eml"
	os.WriteFile(path, []byte(raw), 0600)
	if _, err := mailparse.ParseFile(path); err != nil {
		t.Fatalf("default limits rejected a shallow message: %v", err)
	}
	if _, err := mailparse.ParseFile(path, mailparse.Limits{MaxDepth: 1}); err == nil || !strings.Contains(err.Error(), "nesting too deep") {
		t.Fatalf("configured depth not honoured: %v", err)
	}
	if _, err := mailparse.ParseFile(path, mailparse.Limits{MaxParts: 1}); err == nil || !strings.Contains(err.Error(), "too many mime parts") {
		t.Fatalf("configured part cap not honoured: %v", err)
	}
}

// TestBuildMessageRejectsHeaderInjection guards against CRLF smuggling: no
// address or header-bound value may carry a CR/LF into the generated message.
func TestBuildMessageRejectsHeaderInjection(t *testing.T) {
	from := mailparse.Address{Address: "hermes@example.com"}
	cases := []struct {
		name      string
		from      mailparse.Address
		to, cc    []string
		messageID string
		inReplyTo string
	}{
		{name: "to address", from: from, to: []string{"victim@example.net\r\nBcc: attacker@example.net"}},
		{name: "cc address", from: from, to: []string{"victim@example.net"}, cc: []string{"cc@example.net\r\nBcc: attacker@example.net"}},
		{name: "from name", from: mailparse.Address{Name: "Evil\r\nBcc: attacker@example.net", Address: "hermes@example.com"}, to: []string{"victim@example.net"}},
		{name: "from address", from: mailparse.Address{Address: "hermes@example.com\r\nBcc: attacker@example.net"}, to: []string{"victim@example.net"}},
		{name: "message id", from: from, to: []string{"victim@example.net"}, messageID: "<m@example.com>\r\nBcc: attacker@example.net"},
		{name: "in reply to", from: from, to: []string{"victim@example.net"}, messageID: "<m@example.com>", inReplyTo: "<p@example.com>\r\nBcc: attacker@example.net"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgID := tc.messageID
			if msgID == "" {
				msgID = "<m@example.com>"
			}
			if _, err := mailparse.BuildMessage(tc.from, tc.to, tc.cc, nil, "Subject", "body", "", msgID, tc.inReplyTo, nil, time.Now(), nil); err == nil {
				t.Fatal("expected control-character error")
			}
		})
	}
}

// TestParseExtractIndexConsistency guards the shared walker: attachment
// indexing during parsing must match extraction even with nested multiparts
// and a malformed Content-Type.
func TestParseExtractIndexConsistency(t *testing.T) {
	raw := strings.Join([]string{
		"From: Alice <alice@example.net>", "To: box@example.com", "Subject: Nested", "Message-ID: <n@test>",
		"MIME-Version: 1.0", "Content-Type: multipart/mixed; boundary=outer", "",
		"--outer", "Content-Type: text/plain", "", "hello", "",
		"--outer", "Content-Type: multipart/alternative; boundary=inner", "",
		"--inner", "Content-Type: text/plain", "", "alt text", "",
		"--inner", "Content-Type: text/html", "", "<p>alt html</p>", "",
		"--inner--", "",
		"--outer", "Content-Type: application/octet-stream; name=\"broken", "Content-Disposition: attachment; filename=broken.bin", "Content-Transfer-Encoding: base64", "", "aGVsbG8gYnJva2Vu", "",
		"--outer", "Content-Type: text/csv; name=data.csv", "Content-Disposition: attachment; filename=data.csv", "Content-Transfer-Encoding: base64", "", "YSxi", "",
		"--outer--", "",
	}, "\r\n")
	path := t.TempDir() + "/nested.eml"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := mailparse.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Attachments) != 2 {
		t.Fatalf("expected 2 attachments, got %#v", p.Attachments)
	}
	if p.Attachments[0].Filename != "broken.bin" || p.Attachments[1].Filename != "data.csv" {
		t.Fatalf("filenames %#v", p.Attachments)
	}
	want := map[int]string{1: "hello broken", 2: "a,b"}
	for _, a := range p.Attachments {
		var b bytes.Buffer
		if err := mailparse.ExtractAttachment(path, a.PartIndex, &b); err != nil {
			t.Fatalf("extract %d: %v", a.PartIndex, err)
		}
		if b.String() != want[a.PartIndex] {
			t.Fatalf("part %d = %q, want %q", a.PartIndex, b.String(), want[a.PartIndex])
		}
	}
	var names []string
	if err := mailparse.ExtractAllAttachments(path, func(a mailparse.Attachment, r io.Reader) error {
		data, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		if string(data) != want[a.PartIndex] {
			t.Fatalf("bulk part %d = %q", a.PartIndex, data)
		}
		names = append(names, a.Filename)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "broken.bin" || names[1] != "data.csv" {
		t.Fatalf("bulk names %#v", names)
	}
}

// TestParseWindows1252Headers guards the header decoder against a regression
// where an unhandled charset (notably the Windows-1252 labels Outlook emits)
// made DecodeHeader fail and reduced the subject to an empty string. This is
// the exact shape that caused an approval-token subject to be lost and the
// control reply to be delivered as ordinary mail.
func TestParseWindows1252Headers(t *testing.T) {
	raw := strings.Join([]string{
		"From: =?Windows-1252?Q?Ben_Dell=E1r?= <ben@example.com>",
		"To: =?Windows-1252?Q?Jos=E9?= <jose@example.net>",
		"Subject: =?Windows-1252?Q?[GH-APPROVE:wVIg8xlkTlqgcv35bo218w]_BSS-REC-0042_=97_BIC?=",
		" =?Windows-1252?Q?KLEY(Nicola)_=97_Paid_invoice_for_your_records?=",
		"Message-ID: <win@test>",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"body",
	}, "\r\n")
	path := t.TempDir() + "/win.eml"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := mailparse.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "[GH-APPROVE:wVIg8xlkTlqgcv35bo218w] BSS-REC-0042 \u2014 BICKLEY(Nicola) \u2014 Paid invoice for your records"
	if p.Subject != want {
		t.Fatalf("subject = %q, want %q", p.Subject, want)
	}
	if p.From.Name != "Ben Dellár" || p.From.Address != "ben@example.com" {
		t.Fatalf("from = %#v", p.From)
	}
	if len(p.To) != 1 || p.To[0] != "jose@example.net" {
		t.Fatalf("to = %#v", p.To)
	}
}

// TestParseUnknownCharsetFallsBackToRawHeader guards the fail-safe: a header
// encoded with a charset the decoder cannot convert must keep the raw value so
// an ASCII control token inside it is still detectable, rather than becoming
// empty and silently disabling approval handling.
func TestParseUnknownCharsetFallsBackToRawHeader(t *testing.T) {
	raw := strings.Join([]string{
		"From: Ben <ben@example.com>",
		"To: box@example.net",
		"Subject: =?ks_c_5601-1987?Q?[GH-APPROVE:abcdefghijklmnop]_hello?=",
		"Message-ID: <unknown@test>",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"body",
	}, "\r\n")
	path := t.TempDir() + "/unknown.eml"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := mailparse.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Subject, "[GH-APPROVE:abcdefghijklmnop]") {
		t.Fatalf("raw subject token lost: %q", p.Subject)
	}
}
