package imap_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	imapadapter "github.com/dellarb/mailmoose/internal/transport/imap"
	"github.com/emersion/go-imap/v2"
)

func TestFolderCreateRenameDelete(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	fs.AddMailbox("INBOX")
	adapter := dialFake(t, fs, "user@example.com", "secret")
	ctx := testContext(t)

	if err := adapter.CreateFolder(ctx, "Projects"); err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if _, err := adapter.EnsureFolderExists(ctx, "Projects"); err != nil {
		t.Fatalf("EnsureFolderExists after create: %v", err)
	}
	if err := adapter.RenameFolder(ctx, "Projects", "Work"); err != nil {
		t.Fatalf("RenameFolder: %v", err)
	}
	if _, err := adapter.EnsureFolderExists(ctx, "Work"); err != nil {
		t.Fatalf("EnsureFolderExists after rename: %v", err)
	}
	if err := adapter.DeleteFolder(ctx, "Work"); err != nil {
		t.Fatalf("DeleteFolder: %v", err)
	}
	if _, err := adapter.EnsureFolderExists(ctx, "Work"); err == nil {
		t.Fatal("expected deleted folder to be gone")
	}
}

func TestDeleteFolderRefusedWhenNonEmpty(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	fs.AddMailbox("INBOX")
	adapter := dialFake(t, fs, "user@example.com", "secret")
	fs.setDeleteNonEmpty(true)

	err := adapter.DeleteFolder(testContext(t), "INBOX")
	if err == nil {
		t.Fatal("expected delete refusal")
	}
	if !errors.Is(err, imapadapter.ErrFolderNotEmpty) {
		t.Fatalf("error = %v, want ErrFolderNotEmpty", err)
	}
}

func TestSetFlagsSeenAndUnseen(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	fs.AddMailbox("INBOX")
	adapter := dialFake(t, fs, "user@example.com", "secret")
	loc := seedMessage(t, adapter, "INBOX", sampleMessage, nil)
	ctx := testContext(t)

	if err := adapter.MarkSeen(ctx, loc); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	h, err := adapter.FetchHeader(ctx, loc)
	if err != nil {
		t.Fatalf("FetchHeader: %v", err)
	}
	if !h.Read {
		t.Error("expected message to be seen")
	}

	if err := adapter.MarkUnseen(ctx, loc); err != nil {
		t.Fatalf("MarkUnseen: %v", err)
	}
	h, _ = adapter.FetchHeader(ctx, loc)
	if h.Read {
		t.Error("expected message to be unseen")
	}
}

func TestSetFlagsCustomFlag(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	fs.AddMailbox("INBOX")
	adapter := dialFake(t, fs, "user@example.com", "secret")
	loc := seedMessage(t, adapter, "INBOX", sampleMessage, nil)

	flags, err := adapter.SetFlags(testContext(t), loc, []string{"$Handoff"}, nil)
	if err != nil {
		t.Fatalf("SetFlags: %v", err)
	}
	found := false
	for _, f := range flags {
		if strings.EqualFold(f, "$Handoff") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected $Handoff in flags, got %v", flags)
	}
}

func TestMoveMessagePreservesDestinationUID(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	fs.AddMailbox("INBOX")
	fs.AddMailbox("Archive")
	adapter := dialFake(t, fs, "user@example.com", "secret")
	loc := seedMessage(t, adapter, "INBOX", sampleMessage, nil)
	ctx := testContext(t)

	res, err := adapter.MoveMessage(ctx, loc, "Archive")
	if err != nil {
		t.Fatalf("MoveMessage: %v", err)
	}
	if res.DestinationUID == 0 {
		t.Fatal("expected a destination UID from COPYUID")
	}
	// The message must now be findable in Archive at the reported UID.
	found, err := adapter.FetchHeader(ctx, imapadapter.Locator{
		FolderPath: "Archive", UIDValidity: res.UIDValidity, UID: res.DestinationUID,
	})
	if err != nil {
		t.Fatalf("FetchHeader in destination: %v", err)
	}
	if found.MessageID != "handoff-123@example.com" {
		t.Errorf("moved message-id = %q", found.MessageID)
	}
}

func TestAppendConfirmedReturnsUID(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	fs.AddMailbox("INBOX")
	fs.AddMailbox("Drafts")
	adapter := dialFake(t, fs, "user@example.com", "secret")

	res, err := adapter.AppendBytes(testContext(t), "Drafts", []byte(sampleMessage), []string{imapadapter.FlagDraft}, time.Time{})
	if err != nil {
		t.Fatalf("AppendBytes: %v", err)
	}
	if !res.Confirmed || res.DestinationUID == 0 {
		t.Fatalf("expected a confirmed append with a UID, got %+v", res)
	}
}

func TestAppendUnknownOutcomeNotExactlyOnce(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	fs.AddMailbox("INBOX")
	fs.AddMailbox("Drafts")
	adapter := dialFake(t, fs, "user@example.com", "secret")
	fs.setAppendNoUID(true)

	res, err := adapter.AppendBytes(testContext(t), "Drafts", []byte(sampleMessage), nil, time.Time{})
	if err != nil {
		t.Fatalf("AppendBytes: %v", err)
	}
	if res.Confirmed {
		t.Fatal("append without APPENDUID must not be reported as confirmed")
	}
	if res.DestinationUID != 0 {
		t.Fatalf("destination UID = %d, want 0", res.DestinationUID)
	}
	// Recovery: the message is still findable by Message-ID.
	loc, err := adapter.FindByMessageID(testContext(t), "Drafts", "handoff-123@example.com")
	if err != nil {
		t.Fatalf("FindByMessageID recovery: %v", err)
	}
	if loc.UID == 0 {
		t.Fatal("expected a recovered UID")
	}
}

func TestExpungeUnsupportedWithoutUIDPlus(t *testing.T) {
	// Advertise only IMAP4rev1 without UIDPLUS/MOVE.
	fs := newFakeServer(t, serverConfig{caps: imap.CapSet{imap.CapIMAP4rev1: {}}})
	fs.AddMailbox("INBOX")
	adapter := dialFake(t, fs, "user@example.com", "secret")
	loc := seedMessage(t, adapter, "INBOX", sampleMessage, nil)

	err := adapter.ExpungeUIDs(testContext(t), "INBOX", loc.UIDValidity, []uint32{loc.UID})
	if err == nil {
		t.Fatal("expected unsupported without UIDPLUS")
	}
	if !errors.Is(err, imapadapter.ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
	var mb *model.MailboxError
	if !errors.As(err, &mb) || mb.Kind != model.ErrKindUnsupported {
		t.Fatalf("error = %v, want kind unsupported", err)
	}
}

func TestMoveRefusedWithoutMoveOrUIDPlus(t *testing.T) {
	// A server with neither MOVE nor UIDPLUS would otherwise make go-imap's
	// COPY fallback issue a blanket EXPUNGE (deleting unrelated \Deleted mail).
	// The adapter must refuse the move rather than risk that.
	fs := newFakeServer(t, serverConfig{caps: imap.CapSet{imap.CapIMAP4rev1: {}}})
	fs.AddMailbox("INBOX")
	fs.AddMailbox("Archive")
	adapter := dialFake(t, fs, "user@example.com", "secret")

	_, err := adapter.MoveMessage(testContext(t), imapadapter.Locator{FolderPath: "INBOX", UID: 1}, "Archive")
	if !errors.Is(err, imapadapter.ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
}

func TestDeleteMessageRefusedWithoutUIDPlus(t *testing.T) {
	// Without UIDPLUS, DeleteMessage must refuse before flagging \Deleted so the
	// message is not left hidden-but-present while the caller is told it failed.
	fs := newFakeServer(t, serverConfig{caps: imap.CapSet{imap.CapIMAP4rev1: {}}})
	fs.AddMailbox("INBOX")
	adapter := dialFake(t, fs, "user@example.com", "secret")

	err := adapter.DeleteMessage(testContext(t), imapadapter.Locator{FolderPath: "INBOX", UID: 1})
	if !errors.Is(err, imapadapter.ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
}

func TestDeleteMessageUIDTargeted(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	fs.AddMailbox("INBOX")
	adapter := dialFake(t, fs, "user@example.com", "secret")
	loc := seedMessage(t, adapter, "INBOX", sampleMessage, nil)
	keep := seedMessage(t, adapter, "INBOX", multipartMessage, nil)
	ctx := testContext(t)

	if err := adapter.DeleteMessage(ctx, loc); err != nil {
		t.Fatalf("DeleteMessage: %v", err)
	}
	// The other message must survive: no blanket EXPUNGE.
	if _, err := adapter.FetchHeader(ctx, keep); err != nil {
		t.Fatalf("unrelated message was expunged: %v", err)
	}
	if _, err := adapter.FetchHeader(ctx, loc); err == nil {
		t.Fatal("expected the delete target to be gone")
	}
}
