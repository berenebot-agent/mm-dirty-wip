package imap_test

import (
	"bytes"
	"strings"
	"testing"
)

func TestFetchPreviewIsBoundedAndUnseen(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	fs.AddMailbox("INBOX")
	a := dialFake(t, fs, "user@example.com", "secret")
	raw := sampleMessage + strings.Repeat("large body", 20000)
	loc := seedMessage(t, a, "INBOX", raw, nil)
	for _, limit := range []int64{32, 64 << 10, 100 << 10} {
		var buf bytes.Buffer
		if err := a.FetchPreview(testContext(t), loc, limit, &buf); err != nil {
			t.Fatal(err)
		}
		want := limit
		if want > 64<<10 {
			want = 64 << 10
		}
		if int64(buf.Len()) != want || buf.String() != raw[:want] {
			t.Fatalf("prefix length=%d want=%d", buf.Len(), want)
		}
	}
	h, err := a.FetchHeader(testContext(t), loc)
	if err != nil || h.Read {
		t.Fatalf("preview marked Seen or damaged session: %+v %v", h, err)
	}
	loc.UIDValidity++
	var buf bytes.Buffer
	if err := a.FetchPreview(testContext(t), loc, 32, &buf); err == nil || buf.Len() != 0 {
		t.Fatal("stale UIDVALIDITY was not rejected")
	}
}
