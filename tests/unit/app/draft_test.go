package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

func TestSendDraftConsumesAndAttaches(t *testing.T) {
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
