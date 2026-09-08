package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/open-agent-inbox/open-agent-inbox/internal/model"
)

func testStore(t *testing.T) (*Store, model.User, model.Domain, []model.Inbox) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	u, err := s.CreateAccountAndAdmin(context.Background(), "Test", "admin@example.com", "correct horse battery staple", 100<<20)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDomain(context.Background(), u.AccountID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	var boxes []model.Inbox
	for _, name := range []string{"owner", "assistant", "read", "other"} {
		b, err := s.CreateInbox(context.Background(), u.AccountID, d.ID, name, name)
		if err != nil {
			t.Fatal(err)
		}
		boxes = append(boxes, b)
	}
	return s, u, d, boxes
}

func inbound(box model.Inbox, delivery, rfc, inReply string, refs []string, subject, body string) InboundRecord {
	return InboundRecord{Inbox: box, Provider: "mailgun", ProviderDeliveryID: delivery, RFCMessageID: rfc, InReplyTo: inReply, References: refs,
		From: model.Address{Address: "sender@outside.test"}, To: []string{box.Address}, EnvelopeTo: []string{box.Address}, Subject: subject, Text: body,
		RawPath: "messages/test.eml", SizeBytes: 100, ReceivedAt: time.Now().UTC()}
}

func TestPermissionsThreadIsolationDedupSearchEvents(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	_, key, err := s.CreateAPIKey(ctx, u.AccountID, "mixed", false, map[string]string{b[0].ID: "owner", b[1].ID: "assistant", b[2].ID: "read"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.APIKeyPrincipal(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !p.CanOwn(b[0].ID) || !p.CanAssist(b[1].ID) || !p.CanRead(b[2].ID) || p.CanRead(b[3].ID) {
		t.Fatalf("bad roles: %#v", p.MailboxRoles)
	}
	if _, err = s.CreateDraft(ctx, p, model.Draft{InboxID: b[1].ID, Subject: "draft"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateDraft(ctx, p, model.Draft{InboxID: b[2].ID}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read created draft: %v", err)
	}

	m1, e1, dup, err := s.CommitInbound(ctx, inbound(b[0], "delivery-1", "<one@test>", "", nil, "Generator quote", "revised generator price"))
	if err != nil || dup {
		t.Fatalf("commit: %v dup=%v", err, dup)
	}
	mdup, _, dup, err := s.CommitInbound(ctx, inbound(b[0], "delivery-1", "<different@test>", "", nil, "different", "different"))
	if err != nil || !dup || mdup.ID != m1.ID {
		t.Fatalf("dedup failed err=%v dup=%v ids=%s/%s", err, dup, mdup.ID, m1.ID)
	}
	m2, _, _, err := s.CommitInbound(ctx, inbound(b[0], "delivery-2", "<two@test>", "<one@test>", nil, "Re: Generator quote", "reply"))
	if err != nil {
		t.Fatal(err)
	}
	if m2.ThreadID != m1.ThreadID {
		t.Fatalf("reply did not thread")
	}
	// Same forged header in another inbox must remain isolated.
	forged, _, _, err := s.CommitInbound(ctx, inbound(b[3], "delivery-3", "<evil@test>", "<one@test>", nil, "forged", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if forged.ThreadID == m1.ThreadID {
		t.Fatal("cross-inbox thread injection")
	}

	got, err := s.SearchMessages(ctx, p, "generator price", b[0].ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != m1.ID {
		t.Fatalf("search got %#v", got)
	}
	if _, err = s.SearchMessages(ctx, p, "forged", b[3].ID, 20); !errors.Is(err, ErrForbidden) {
		t.Fatalf("search scope: %v", err)
	}
	evs, err := s.ListEvents(ctx, p, 0, b[0].ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) < 2 || evs[0].ID != e1.ID {
		t.Fatalf("events %#v", evs)
	}
	replay, err := s.ListEvents(ctx, p, e1.ID, b[0].ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay) < 1 || replay[0].ID <= e1.ID {
		t.Fatalf("event replay %#v", replay)
	}
}

func TestSQLiteConcurrentWritesSerialized(t *testing.T) {
	ctx := context.Background()
	s, u, d, _ := testStore(t)
	var wg sync.WaitGroup
	errs := make(chan error, 30)
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.CreateInbox(ctx, u.AccountID, d.ID, fmt.Sprintf("box%d", i), "")
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}
