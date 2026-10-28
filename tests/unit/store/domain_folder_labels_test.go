package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
	"github.com/dellarb/mammothmail/tests/support/testdb"
)

// TestDomainCustomFoldersBecomeAdditiveLabels proves the one-time conversion
// of a domain inbox's custom folders: every folder gains an additive label and
// its filed mail carries it, while the folder record and the mail's folder
// membership are preserved. A name already in use takes the first free suffix
// instead of colliding.
func TestDomainCustomFoldersBecomeAdditiveLabels(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dir := testdb.OpenDir(t)
	u, err := st.CreateAccountAndAdmin(ctx, "A", "admin@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDomain(ctx, u.AccountID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	box, err := st.CreateInbox(ctx, u.AccountID, d.ID, "box", "Box")
	if err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	invoices, err := st.CreateFolder(ctx, p, box.ID, store.FolderCreate{Path: "Invoices"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateFolder(ctx, p, box.ID, store.FolderCreate{Path: "Projects/Alpha"}); err != nil {
		t.Fatal(err)
	}
	// A label already in use must not be taken over by the converted folder.
	existing, _, _, err := st.CommitInbound(ctx, store.InboundRecord{
		Inbox: box, Provider: "mailgun", ProviderDeliveryID: "d1", RFCMessageID: "<one@test>",
		From: model.Address{Address: "sender@outside.test"}, To: []string{box.Address},
		Subject: "Invoices", Text: "body", RawPath: "messages/test.eml", SizeBytes: 4, ReceivedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ModifyMessageLabel(ctx, p, existing.ID, "Invoices", "add", false); err != nil {
		t.Fatal(err)
	}
	filed, _, _, err := st.CommitInbound(ctx, store.InboundRecord{
		Inbox: box, Provider: "mailgun", ProviderDeliveryID: "d2", RFCMessageID: "<two@test>",
		From: model.Address{Address: "sender@outside.test"}, To: []string{box.Address},
		Subject: "Filed", Text: "body", RawPath: "messages/test.eml", SizeBytes: 4, ReceivedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.MoveMessageToFolder(ctx, p, filed.ID, invoices.ID); err != nil {
		t.Fatal(err)
	}

	// Re-opening the store runs the conversion.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	converted, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = converted.Close() })

	// The folder records survive: the organisation stays reversible.
	folders, err := converted.ListFolders(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]model.Folder{}
	for _, f := range folders {
		byPath[f.Path] = f
	}
	if _, ok := byPath["Invoices"]; !ok {
		t.Fatalf("the custom folder record was destroyed: %+v", folders)
	}
	if _, ok := byPath["Projects/Alpha"]; !ok {
		t.Fatalf("the nested folder record was destroyed: %+v", folders)
	}
	// The filed mail keeps its folder and gains the label that names it.
	got, err := converted.GetMessage(ctx, p, filed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MailboxID != invoices.ID {
		t.Fatalf("filed message left its folder: %q", got.MailboxID)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "Invoices-2" {
		t.Fatalf("filed message labels = %v, want [Invoices-2]", got.Labels)
	}
	defined, err := converted.ListDefinedLabels(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, n := range defined {
		names[n] = true
	}
	if !names["Invoices-2"] {
		t.Fatalf("converted folder label missing: %v", defined)
	}
	if !names["Projects-Alpha"] {
		t.Fatalf("nested folder label missing: %v", defined)
	}
	// The pre-existing label is untouched.
	before, err := converted.GetMessage(ctx, p, existing.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Labels) != 1 || before.Labels[0] != "Invoices" {
		t.Fatalf("existing label changed: %v", before.Labels)
	}
}

// TestDomainFolderLabelCollisionsAreDistinct proves two folders that flatten
// to the same label name, and a name that collides case-insensitively with an
// existing label, each get their own label: no folder is dropped and no label
// collides.
func TestDomainFolderLabelCollisionsAreDistinct(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dir := testdb.OpenDir(t)
	u, err := st.CreateAccountAndAdmin(ctx, "A", "admin@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDomain(ctx, u.AccountID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	box, err := st.CreateInbox(ctx, u.AccountID, d.ID, "box", "Box")
	if err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	// "A/B" and "A-B" flatten to the same label name.
	first, err := st.CreateFolder(ctx, p, box.ID, store.FolderCreate{Path: "A/B"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.CreateFolder(ctx, p, box.ID, store.FolderCreate{Path: "A-B"})
	if err != nil {
		t.Fatal(err)
	}
	// A long name that collides with an existing label.
	long := strings.Repeat("x", model.MaxLabelLength)
	if _, err := st.CreateDefinedLabel(ctx, p, box.ID, long); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateFolder(ctx, p, box.ID, store.FolderCreate{Path: long}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []model.Folder{first, second} {
		msg, _, _, err := st.CommitInbound(ctx, store.InboundRecord{
			Inbox: box, Provider: "mailgun", ProviderDeliveryID: "d" + f.ID, RFCMessageID: "<" + f.ID + "@test>",
			From: model.Address{Address: "sender@outside.test"}, To: []string{box.Address},
			Subject: f.Path, Text: "body", RawPath: "messages/test.eml", SizeBytes: 4, ReceivedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.MoveMessageToFolder(ctx, p, msg.ID, f.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	converted, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = converted.Close() })
	labels, err := converted.ListInboxLabels(ctx, p, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	defined, err := converted.ListDefinedLabels(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, list := range [][]string{labels, defined} {
		local := map[string]bool{}
		for _, l := range list {
			if local[strings.ToLower(l)] {
				t.Fatalf("duplicate label %q in %v", l, list)
			}
			local[strings.ToLower(l)] = true
			seen[strings.ToLower(l)] = true
			if len([]rune(l)) > model.MaxLabelLength {
				t.Fatalf("label %q exceeds the label length limit", l)
			}
		}
	}
	if !seen["a-b"] || !seen["a-b-2"] {
		t.Fatalf("colliding folders did not get distinct labels: %v %v", labels, defined)
	}
	// The base is shortened so the suffixed label still fits the limit.
	wantLong := string([]rune(long)[:model.MaxLabelLength-len("-2")]) + "-2"
	if !seen[strings.ToLower(wantLong)] {
		t.Fatalf("long colliding folder did not get a bounded label: %v %v", labels, defined)
	}
}

// TestDomainFolderConversionIsIdempotent proves a second conversion finds
// nothing to do: the same label is reused and the filed mail is unchanged.
func TestDomainFolderConversionIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dir := testdb.OpenDir(t)
	u, err := st.CreateAccountAndAdmin(ctx, "A", "admin@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDomain(ctx, u.AccountID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	box, err := st.CreateInbox(ctx, u.AccountID, d.ID, "box", "Box")
	if err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	folder, err := st.CreateFolder(ctx, p, box.ID, store.FolderCreate{Path: "Receipts"})
	if err != nil {
		t.Fatal(err)
	}
	msg, _, _, err := st.CommitInbound(ctx, store.InboundRecord{
		Inbox: box, Provider: "mailgun", ProviderDeliveryID: "d1", RFCMessageID: "<one@test>",
		From: model.Address{Address: "sender@outside.test"}, To: []string{box.Address},
		Subject: "Receipt", Text: "body", RawPath: "messages/test.eml", SizeBytes: 4, ReceivedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.MoveMessageToFolder(ctx, p, msg.ID, folder.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	first, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	labels, err := first.ListInboxLabels(ctx, p, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 1 || labels[0] != "Receipts" {
		t.Fatalf("converted labels = %v, want [Receipts]", labels)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	again, err := second.ListInboxLabels(ctx, p, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || again[0] != "Receipts" {
		t.Fatalf("second conversion changed the labels: %v", again)
	}
	got, err := second.GetMessage(ctx, p, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MailboxID != folder.ID {
		t.Fatalf("second conversion moved the message out of its folder: %q", got.MailboxID)
	}
}
