package httpapp_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/events"
	"github.com/dellarb/mailmoose/internal/httpapp"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/tests/support/testdb"
)

// A bulk submission must work entirely from cached identity, even while the
// provider is unavailable or metadata sync is incomplete. No worker is started:
// pending jobs prove the POST did not execute the remote operation itself.
func TestRemoteBulkQueuesWithoutProvider(t *testing.T) {
	for _, action := range []string{"delete", "read", "unread", "restore", "move", "purge"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			st, dir := testdb.OpenDir(t)
			svc, err := app.New(testConfig(dir), st, events.NewHub())
			if err != nil {
				t.Fatal(err)
			}
			u, err := st.CreateAccountAndAdmin(ctx, "Bulk", "bulk@example.test", "correct horse battery staple", 50<<20)
			if err != nil {
				t.Fatal(err)
			}
			p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
			box, err := st.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{
				Address: "remote@example.test",
				Remote:  &model.RemoteConnection{Host: "unavailable.example.test", Port: 993, Security: model.RemoteSecurityTLS, Username: "remote"},
			})
			if err != nil {
				t.Fatal(err)
			}
			rm := app.NewRemoteMailboxService(svc)
			t.Cleanup(rm.Stop)
			if _, err := rm.ConfigureStandaloneRemote(ctx, p, box.ID, store.StandaloneRemoteUpdate{IMAPPassword: "password"}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.ReconcileRemoteFolders(ctx, u.AccountID, box.ID, []store.RemoteFolderSync{
				{Path: "INBOX", Name: "Inbox", Role: model.FolderRoleInbox, Selectable: true, UIDValidity: 1},
				{Path: "Trash", Name: "Trash", Role: model.FolderRoleTrash, Selectable: true, UIDValidity: 1},
			}); err != nil {
				t.Fatal(err)
			}
			folder := "INBOX"
			if action == "purge" {
				folder = "Trash"
			}
			var ids []string
			for uid := uint32(1); uid <= 3; uid++ {
				m, err := st.UpsertRemoteMessage(ctx, u.AccountID, box.ID, store.RemoteMessageInput{FolderPath: folder, UIDValidity: 1, UID: uid, Subject: "Cached"})
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, m.ID)
			}
			cookie, csrf := uiSession(t, svc, u.ID)
			srv := httpapp.New(svc, nil)
			srv.SetRemoteMailbox(rm)
			form := url.Values{"_csrf": {csrf}, "folder": {"inbox"}, "scope": {"page"}, "action": {action}, "ids": {ids[0], ids[1], ids[0], "missing"}, "dest": {"Trash"}}
			rr := uiPost(t, srv.Handler(), cookie, "/ui/inboxes/"+box.ID+"/bulk", form.Encode())
			if rr.Code != http.StatusSeeOther {
				t.Fatalf("submit = %d: %s", rr.Code, rr.Body.String())
			}
			loc, err := url.Parse(rr.Header().Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			if notice := loc.Query().Get("notice"); notice != "2 message actions queued; 1 could not be queued" {
				t.Fatalf("notice = %q", notice)
			}
			jobs, err := st.ListMessageActions(ctx, p)
			if err != nil || len(jobs) != 2 {
				t.Fatalf("jobs = %#v, %v", jobs, err)
			}
			for _, job := range jobs {
				if job.State != "pending" || job.MessageID == ids[2] {
					t.Fatalf("unexpected job: %#v", job)
				}
			}
			// Submission must not claim settled provider success.
			if strings.Contains(loc.Query().Get("notice"), "moved to trash") {
				t.Fatal("queued action presented as completed")
			}
		})
	}
}

func TestRemoteBulkDeleteSettles(t *testing.T) {
	svc, h, u, _, box, fake := standaloneFixture(t)
	fake.folders["Trash"] = 100
	for _, subject := range []string{"First", "Second", "Keep"} {
		fake.add("INBOX", "From: a@b.test\r\nSubject: "+subject+"\r\n\r\nbody", "<"+subject+"@test>", subject)
	}
	refreshRemote(t, h, box.ID, adminKey(t, svc, u))
	ctx := context.Background()
	rows, err := svc.Store.ListRemoteMessages(ctx, u.AccountID, box.ID, "INBOX")
	if err != nil || len(rows) != 3 {
		t.Fatalf("cached messages = %#v, %v", rows, err)
	}
	cookie, csrf := uiSession(t, svc, u.ID)
	form := url.Values{"_csrf": {csrf}, "folder": {"inbox"}, "scope": {"page"}, "action": {"delete"}, "ids": {rows[0].ID, rows[1].ID}}
	rr := uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/bulk", form.Encode())
	if rr.Code != http.StatusSeeOther || !strings.Contains(rr.Header().Get("Location"), "2+message+actions+queued") {
		t.Fatalf("submit = %d: %s", rr.Code, rr.Header().Get("Location"))
	}
	waitUIActions(t, svc, u.AccountID)
	jobs, err := svc.Store.ListMessageActions(ctx, model.Principal{AccountID: u.AccountID, Admin: true})
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs = %#v, %v", jobs, err)
	}
	for _, job := range jobs {
		if job.State != "done" {
			t.Fatalf("delete did not complete: %#v", job)
		}
	}
	for i, row := range rows {
		cached, err := svc.Store.GetRemoteMessage(ctx, u.AccountID, box.ID, row.ID)
		want := "Trash"
		if i == 2 {
			want = "INBOX"
		}
		if err != nil || cached.FolderPath != want {
			t.Fatalf("message %d = %#v, %v; want %s", i, cached, err, want)
		}
	}
}
