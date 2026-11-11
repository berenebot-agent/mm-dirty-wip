package httpapp_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
)

// TestPlacedDraftRoutesRecogniseGmailAndOutlookDrafts proves the editor and the
// delete route accept a draft that sits in a Gmail or Outlook Drafts folder.
// Those rows are filed under "gmail:<id>" / "microsoft:<id>" while the Drafts
// folder's path is the DRAFT label or a Graph folder id, so a plain path
// comparison rejected every one of them with a 404 / 400.
func TestPlacedDraftRoutesRecogniseGmailAndOutlookDrafts(t *testing.T) {
	t.Parallel()
	svc, h, u, _, _, _ := standaloneFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)
	received := time.Now().UTC().Format(time.RFC3339Nano)
	in := func(id string) store.RemoteMessageInput {
		return store.RemoteMessageInput{RFCMessageID: "<" + id + "@test>", Subject: id, FromAddress: "a@remote.test", Draft: true, ReceivedAt: &received, InternalDate: &received}
	}
	drafts := func(path string) []store.RemoteFolderSync {
		return []store.RemoteFolderSync{{Path: path, Name: "Drafts", Role: model.FolderRoleDrafts, Selectable: true, UIDValidity: 1, IndexedAt: time.Now()}}
	}

	for _, provider := range []string{"gmail", "outlook"} {
		box, err := svc.Store.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: provider + "@remote.test"})
		if err != nil {
			t.Fatal(err)
		}
		var rowID string
		if provider == "gmail" {
			if err = svc.Store.SaveGoogleConnection(ctx, store.GoogleConnection{AccountID: u.AccountID, InboxID: box.ID, ClientID: "client", EncryptedSecret: "s", EncryptedToken: "t", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			if _, err = svc.Store.ReconcileRemoteFolders(ctx, u.AccountID, box.ID, drafts("DRAFT")); err != nil {
				t.Fatal(err)
			}
			row, err := svc.Store.UpsertGoogleMessage(ctx, u.AccountID, box.ID, "g-1", "th-1", []string{"DRAFT"}, in("g-1"))
			if err != nil {
				t.Fatal(err)
			}
			rowID = row.ID
		} else {
			if err = svc.Store.SaveMicrosoftConnection(ctx, store.MicrosoftConnection{AccountID: u.AccountID, InboxID: box.ID, ClientID: "client", Tenant: "common", IdentityID: "id", Mailbox: box.Address, EncryptedToken: "t", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			if _, err = svc.Store.ReconcileRemoteFolders(ctx, u.AccountID, box.ID, drafts("graph-drafts-id")); err != nil {
				t.Fatal(err)
			}
			row, err := svc.Store.UpsertMicrosoftMessage(ctx, u.AccountID, box.ID, "m-1", "th-1", "graph-drafts-id", in("m-1"))
			if err != nil {
				t.Fatal(err)
			}
			rowID = row.ID
		}

		// The editor must recognise the draft. With no live provider behind the
		// test it then fails loading the body (502), which is not the 404 an
		// unrecognised draft gets.
		if rr := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts/"+rowID+"/edit"); rr.Code == http.StatusNotFound {
			t.Errorf("%s: editor = 404, the placed draft was not recognised", provider)
		}
		rr := uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts/"+rowID+"/delete", "_csrf="+csrf)
		if rr.Code >= 400 {
			t.Errorf("%s: delete = %d, want a redirect: %s", provider, rr.Code, rr.Body.String())
		}
	}
}
