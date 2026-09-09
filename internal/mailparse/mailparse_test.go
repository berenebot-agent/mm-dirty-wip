package mailparse

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseAndExtractAttachment(t *testing.T) {
	raw := strings.Join([]string{
		"From: Alice <alice@example.net>", "To: box@example.com", "Subject: Test", "Message-ID: <one@test>", "MIME-Version: 1.0", "Content-Type: multipart/mixed; boundary=x", "", "--x", "Content-Type: text/plain; charset=utf-8", "", "hello body", "--x", "Content-Type: text/html; name=attack.html", "Content-Disposition: attachment; filename=attack.html", "Content-Transfer-Encoding: base64", "", "PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==", "--x--", ""}, "\r\n")
	path := t.TempDir() + "/m.eml"
	os.WriteFile(path, []byte(raw), 0600)
	p, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Text != "hello body" || len(p.Attachments) != 1 || p.Attachments[0].Filename != "attack.html" {
		t.Fatalf("%+v", p)
	}
	var b bytes.Buffer
	if err = ExtractAttachment(path, p.Attachments[0].PartIndex, &b); err != nil {
		t.Fatal(err)
	}
	if b.String() != "<script>alert(1)</script>" {
		t.Fatalf("%q", b.String())
	}
}
func TestBuildMessageAttachmentRoundTrip(t *testing.T) {
	raw, err := BuildMessage(Address{Name: "Hermes", Address: "hermes@example.com"}, []string{"friend@example.net"}, []string{"cc@example.net"}, []string{"bcc@example.net"}, "Report", "See attached", "", "<m1@example.com>", "", nil, time.Now(), []Attachment{{Filename: "report.txt", ContentType: "text/plain", Content: []byte("hello attachment")}})
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/m.eml"
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	p, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Text != "See attached" || len(p.Attachments) != 1 || p.Attachments[0].Filename != "report.txt" {
		t.Fatalf("%+v", p)
	}
	var b bytes.Buffer
	if err = ExtractAttachment(path, p.Attachments[0].PartIndex, &b); err != nil {
		t.Fatal(err)
	}
	if b.String() != "hello attachment" {
		t.Fatalf("attachment %q", b.String())
	}
}

func TestBuildMessageHTMLAttachmentRoundTrip(t *testing.T) {
	raw, err := BuildMessage(Address{Address: "hermes@example.com"}, []string{"friend@example.net"}, nil, nil, "Report", "text body", "<p>html body</p>", "<m2@example.com>", "", nil, time.Now(), []Attachment{{Filename: "report.txt", ContentType: "text/plain", Content: []byte("hello attachment")}})
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/m.eml"
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	p, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Text != "text body" || !strings.Contains(p.HTML, "html body") || len(p.Attachments) != 1 {
		t.Fatalf("%+v", p)
	}
}

func TestHTMLIsEscaped(t *testing.T) {
	raw := "From: a@b.test\r\nTo: c@d.test\r\nContent-Type: text/html\r\n\r\n<script>alert(1)</script>"
	path := t.TempDir() + "/m"
	os.WriteFile(path, []byte(raw), 0600)
	p, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.HTML, "<script>") {
		t.Fatal("unsafe html retained")
	}
}

// BCC recipients must never appear in the generated MIME; they are carried only
// in the provider/SMTP envelope.
func TestBuildMessageOmitsBccHeader(t *testing.T) {
	raw, err := BuildMessage(Address{Address: "hermes@example.com"}, []string{"friend@example.net"}, nil, []string{"secret@example.net"}, "Subject", "body", "", "<m@example.com>", "", nil, time.Now(), nil)
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
	depth := maxMIMEDepth + 2
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
	if _, err := ParseFile(path); err == nil || !strings.Contains(err.Error(), "nesting too deep") {
		t.Fatalf("expected nesting error, got %v", err)
	}
}

func TestParseRejectsTooManyParts(t *testing.T) {
	var b strings.Builder
	b.WriteString("Content-Type: multipart/mixed; boundary=x\r\n\r\n")
	for i := 0; i < maxMIMEParts+1; i++ {
		fmt.Fprintf(&b, "--x\r\nContent-Type: text/plain\r\n\r\npart %d\r\n", i)
	}
	b.WriteString("--x--\r\n")
	raw := "From: a@b.test\r\nTo: c@d.test\r\n" + b.String()
	path := t.TempDir() + "/many.eml"
	os.WriteFile(path, []byte(raw), 0600)
	if _, err := ParseFile(path); err == nil || !strings.Contains(err.Error(), "too many mime parts") {
		t.Fatalf("expected part-count error, got %v", err)
	}
}
