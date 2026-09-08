package mailparse

import (
	"bytes"
	"os"
	"strings"
	"testing"
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
