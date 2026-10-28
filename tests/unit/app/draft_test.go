package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dellarb/mammothmail/internal/app"
	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
)

func TestSendDraftConsumesAndAttaches(t *testing.T) {
	t.Parallel()
	svc, u, _, box := testService(t)
	ctx := context.Background()
	admin := model.Principal{AccountID: u.AccountID, Admin: true}
	draft, err := svc.Store.CreateDraft(ctx, admin, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s", Text: "b"})
	if err != nil {
		t.Fatal(err)
	}
	rawRel := "drafts/test-att.bin"
	rawPath := filepath.Join(svc.Config.DataDir, filepath.FromSlash(rawRel))
	if err = os.MkdirAll(filepath.Dir(rawPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(rawPath, []byte("attdata"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Store.AddDraftAttachment(ctx, admin, draft.ID, model.DraftAttachment{Filename: "a.txt", ContentType: "text/plain", Size: int64(len("attdata")), RawPath: rawRel}); err != nil {
		t.Fatal(err)
	}

	res, err := svc.SendDraft(ctx, admin, draft.ID, app.SendInput{InboxID: box.ID, To: draft.To, Subject: draft.Subject, Text: draft.Text}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Store.GetDraft(ctx, admin, draft.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("draft not consumed: %v", err)
	}
	atts, err := svc.Store.ListAttachments(ctx, admin, res.Message.ID)
	if err != nil || len(atts) != 1 || atts[0].Filename != "a.txt" {
		t.Fatalf("attachments %v %#v", err, atts)
	}
	if _, err = os.Stat(rawPath); !os.IsNotExist(err) {
		t.Fatalf("draft attachment file not removed: %v", err)
	}
}

// TestSendDraftReplayRejectsForwardSource covers the one mismatched retry the
// replay fingerprint can catch: a consumed draft retried with the same key but
// naming a forward source must conflict, not report the earlier message sent
// while the new draft it was created from lingers unconsumed.
func TestSendDraftReplayRejectsForwardSource(t *testing.T) {
	t.Parallel()
	svc, u, _, box := testService(t)
	ctx := context.Background()
	admin := model.Principal{AccountID: u.AccountID, Admin: true}
	draft, err := svc.Store.CreateDraft(ctx, admin, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s", Text: "b"})
	if err != nil {
		t.Fatal(err)
	}
	in := app.SendInput{InboxID: box.ID, To: draft.To, Subject: draft.Subject, Text: draft.Text}
	if _, err = svc.SendDraft(ctx, admin, draft.ID, in, "draft-replay-key"); err != nil {
		t.Fatal(err)
	}

	forward := in
	forward.ForwardOfMessageID = "msg_somewhere_else"
	if _, err = svc.SendDraft(ctx, admin, draft.ID, forward, "draft-replay-key"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("mismatched retry err=%v, want conflict", err)
	}
	// The identical retry still replays, so a lost response is not turned into an
	// error.
	if res, rerr := svc.SendDraft(ctx, admin, draft.ID, in, "draft-replay-key"); rerr != nil || res.Message.ID == "" {
		t.Fatalf("identical retry = %+v, %v", res, rerr)
	}
}
