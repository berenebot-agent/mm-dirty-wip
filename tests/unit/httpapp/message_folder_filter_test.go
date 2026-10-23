package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestAPIMessagesFolderFilter proves the account-wide message route honours the
// folder filter exactly as the inbox-scoped route does: a canonical folder id
// narrows the result to that folder, a legacy custom folder is not a canonical
// location and matches nothing, an unknown folder id yields an empty list
// rather than the whole inbox, and no folder filter still lists everything
// else.
func TestAPIMessagesFolderFilter(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	// A legacy custom folder. A domain mailbox organises with labels now, so
	// such a folder is not a canonical location any more.
	legacy, err := svc.Store.CreateFolder(ctx, p, box.ID, store.FolderCreate{Path: "Projects"})
	if err != nil {
		t.Fatal(err)
	}
	filed := seedInbound(t, svc, box, "folder-filter-filed", "<folder-filter-filed@test>", "Filed", "body")
	if _, _, err := svc.Store.MoveMessageToFolder(ctx, p, filed.ID, legacy.ID); err != nil {
		t.Fatal(err)
	}
	// A canonical destination: the system Spam folder.
	spam, err := svc.Store.GetSystemFolder(ctx, u.AccountID, box.ID, model.FolderRoleSpam)
	if err != nil {
		t.Fatal(err)
	}
	spammed := seedInbound(t, svc, box, "folder-filter-spam", "<folder-filter-spam@test>", "Spammed", "body")
	if _, _, err := svc.Store.SetMessageSpam(ctx, p, spammed.ID, true); err != nil {
		t.Fatal(err)
	}
	seedInbound(t, svc, box, "folder-filter-loose", "<folder-filter-loose@test>", "Loose", "body")
	key := adminKey(t, svc, u)

	ids := func(path string) []string {
		t.Helper()
		rr := apiGet(t, h, path, key)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
		var page struct {
			Items []struct{ ID string } `json:"items"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(page.Items))
		for _, item := range page.Items {
			out = append(out, item.ID)
		}
		return out
	}

	// The account-wide route narrows to the folder, exactly like the
	// inbox-scoped route.
	scoped := ids("/v1/inboxes/" + box.ID + "/messages?folder=" + url.QueryEscape(spam.ID))
	account := ids("/v1/messages?inbox=" + box.ID + "&folder=" + url.QueryEscape(spam.ID))
	if len(scoped) != 1 || scoped[0] != spammed.ID {
		t.Fatalf("inbox-scoped folder filter = %v, want only %s", scoped, spammed.ID)
	}
	if len(account) != 1 || account[0] != spammed.ID {
		t.Fatalf("account-wide folder filter = %v, want only %s", account, spammed.ID)
	}
	// A legacy custom folder is not a canonical location: it has no canonical
	// matches, on either route form.
	if got := ids("/v1/inboxes/" + box.ID + "/messages?folder=" + url.QueryEscape(legacy.ID)); len(got) != 0 {
		t.Fatalf("legacy custom folder filter = %v, want no rows", got)
	}
	if got := ids("/v1/messages?inbox=" + box.ID + "&folder=" + url.QueryEscape(legacy.ID)); len(got) != 0 {
		t.Fatalf("account-wide legacy custom folder filter = %v, want no rows", got)
	}
	// An unknown folder id narrows to nothing; it never returns the whole inbox.
	if got := ids("/v1/messages?inbox=" + box.ID + "&folder=fld_missing"); len(got) != 0 {
		t.Fatalf("unknown folder filter = %v, want no rows", got)
	}
	// Without a folder filter the listing is unchanged: spam stays out of an
	// ordinary read.
	if got := ids("/v1/messages?inbox=" + box.ID); len(got) != 2 {
		t.Fatalf("unfiltered listing = %v, want both ordinary messages", got)
	}
}

// TestImplicitInboxFolderFilterAcrossAPIAndUI checks that the system Inbox
// folder id includes new, implicitly filed mail and explicitly filed mail, but
// never mail moved to a custom folder or mail in another inbox.
func TestImplicitInboxFolderFilterAcrossAPIAndUI(t *testing.T) {
	t.Parallel()
	svc, h, u, domain, box := httpFixture(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	inboxFolder, err := svc.Store.GetSystemFolder(ctx, u.AccountID, box.ID, model.FolderRoleInbox)
	if err != nil {
		t.Fatal(err)
	}
	custom, err := svc.Store.CreateFolder(ctx, p, box.ID, store.FolderCreate{Path: "Projects"})
	if err != nil {
		t.Fatal(err)
	}
	otherBox, err := svc.Store.CreateInbox(ctx, u.AccountID, domain.ID, "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	implicit := seedInbound(t, svc, box, "implicit-inbox-uat", "<implicit-inbox-uat@test>", "Implicit Inbox UAT", "needle")
	explicit := seedInbound(t, svc, box, "explicit-inbox-uat", "<explicit-inbox-uat@test>", "Explicit Inbox UAT", "needle")
	moved := seedInbound(t, svc, box, "custom-inbox-uat", "<custom-inbox-uat@test>", "Custom Folder UAT", "needle")
	other := seedInbound(t, svc, otherBox, "other-inbox-uat", "<other-inbox-uat@test>", "Other Inbox UAT", "needle")
	spammed := seedInbound(t, svc, box, "spam-inbox-uat", "<spam-inbox-uat@test>", "Spam Inbox UAT", "needle")
	trashed := seedInbound(t, svc, box, "trash-inbox-uat", "<trash-inbox-uat@test>", "Trash Inbox UAT", "needle")
	if _, _, err := svc.Store.SetMessageSpam(ctx, p, spammed.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Store.TrashMessage(ctx, p, trashed.ID); err != nil {
		t.Fatal(err)
	}
	outbound, _, err := svc.Store.CommitOutbound(ctx, store.OutboundRecord{
		Inbox: box, Provider: "smtp", RFCMessageID: "<outbound-inbox-uat@test>",
		From: model.Address{Address: box.Address}, To: []string{"recipient@example.net"},
		Subject: "Outbound Inbox UAT", Text: "needle", RawPath: "messages/outbound-inbox-uat.eml", SizeBytes: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Store.MoveMessageToFolder(ctx, p, explicit.ID, inboxFolder.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Store.MoveMessageToFolder(ctx, p, moved.ID, custom.ID); err != nil {
		t.Fatal(err)
	}
	key := adminKey(t, svc, u)
	param := "folder=" + url.QueryEscape(inboxFolder.ID)
	for _, path := range []string{
		"/v1/inboxes/" + box.ID + "/messages?" + param,
		"/v1/messages?inbox=" + box.ID + "&" + param,
		"/v1/messages?" + param,
		"/v1/inboxes/" + box.ID + "/search?q=needle&" + param,
		"/v1/search?inbox=" + box.ID + "&q=needle&" + param,
		"/v1/search?q=needle&" + param,
	} {
		rr := apiGet(t, h, path, key)
		var page struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &page) != nil {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
		seen := map[string]bool{}
		for _, item := range page.Items {
			seen[item.ID] = true
		}
		if len(page.Items) != 2 || !seen[implicit.ID] || !seen[explicit.ID] {
			t.Fatalf("%s: got %v, want implicit and explicit Inbox only (exclude %s and %s)", path, seen, moved.ID, other.ID)
		}
	}
	for _, path := range []string{
		"/v1/inboxes/" + box.ID + "/threads?" + param,
		"/v1/threads?inbox=" + box.ID + "&" + param,
		"/v1/threads?" + param,
	} {
		rr := apiGet(t, h, path, key)
		var page struct {
			Items []model.Thread `json:"items"`
		}
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &page) != nil {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
		seen := map[string]bool{}
		for _, th := range page.Items {
			if th.MessageCount != 1 {
				t.Fatalf("%s: thread %s has %d messages, want 1", path, th.ID, th.MessageCount)
			}
			seen[th.ID] = true
		}
		if len(page.Items) != 2 || !seen[implicit.ThreadID] || !seen[explicit.ThreadID] {
			t.Fatalf("%s: got threads %v, want implicit and explicit Inbox only", path, seen)
		}
	}
	// Thread pagination must use the same Inbox predicate to resolve before.
	first := apiGet(t, h, "/v1/inboxes/"+box.ID+"/threads?"+param+"&limit=1", key)
	var page1 struct {
		Items      []model.Thread `json:"items"`
		NextCursor string         `json:"next_cursor"`
	}
	if first.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &page1) != nil || len(page1.Items) != 1 || page1.NextCursor == "" {
		t.Fatalf("first Inbox thread page: %d %s", first.Code, first.Body.String())
	}
	second := apiGet(t, h, "/v1/inboxes/"+box.ID+"/threads?"+param+"&limit=1&before="+url.QueryEscape(page1.NextCursor), key)
	var page2 struct {
		Items []model.Thread `json:"items"`
	}
	if second.Code != http.StatusOK || json.Unmarshal(second.Body.Bytes(), &page2) != nil || len(page2.Items) != 1 || page2.Items[0].ID == page1.Items[0].ID {
		t.Fatalf("second Inbox thread page: %d %s", second.Code, second.Body.String())
	}

	cookie, csrf := uiSession(t, svc, u.ID)
	ui := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID)
	body := ui.Body.String()
	if ui.Code != http.StatusOK || !strings.Contains(body, "Implicit Inbox UAT") || !strings.Contains(body, "Explicit Inbox UAT") ||
		strings.Contains(body, "Custom Folder UAT") || strings.Contains(body, "Other Inbox UAT") ||
		strings.Contains(body, "Spam Inbox UAT") || strings.Contains(body, "Trash Inbox UAT") || strings.Contains(body, "Outbound Inbox UAT") ||
		!strings.Contains(body, `data-select-banner data-total="2"`) {
		t.Fatalf("local Inbox UI must list/count only Inbox mail: %d %s", ui.Code, body)
	}
	// Select-all in the Inbox must not mark archived/custom-folder mail read.
	bulk := uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/bulk",
		url.Values{"_csrf": {csrf}, "folder": {"inbox"}, "scope": {"all"}, "action": {"read"}}.Encode())
	if bulk.Code != http.StatusSeeOther {
		t.Fatalf("Inbox bulk mark read: %d %s", bulk.Code, bulk.Body.String())
	}
	for _, tc := range []struct {
		msg  model.Message
		read bool
	}{
		{implicit, true}, {explicit, true}, {moved, false}, {other, false},
		{spammed, false}, {trashed, false}, {outbound, true},
	} {
		got, err := svc.Store.GetMessage(ctx, p, tc.msg.ID)
		if err != nil || got.Read != tc.read {
			t.Fatalf("Inbox bulk filter updated %s: read=%v, want %v, err=%v", tc.msg.ID, got.Read, tc.read, err)
		}
	}
}

// TestSpamFolderFilterUsesSpamClassification proves the Spam folder selector
// answers the same set as the spam role shorthand. Marking a message Spam does
// not file it in the Spam folder, so a message that carries only is_spam (no
// placement row) must still list under folder=<Spam id>; before the fix the
// exact-placement match returned an empty folder while spam=true found it.
func TestSpamFolderFilterUsesSpamClassification(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	spam, err := svc.Store.GetSystemFolder(ctx, u.AccountID, box.ID, model.FolderRoleSpam)
	if err != nil {
		t.Fatal(err)
	}
	flagged := seedInbound(t, svc, box, "spam-folder-classified", "<spam-folder-classified@test>", "Marked spam needle", "needle")
	// The classification alone: no move, so mailbox_id is not the Spam folder.
	if _, _, err := svc.Store.SetMessageSpam(ctx, p, flagged.ID, true); err != nil {
		t.Fatal(err)
	}
	filed := seedInbound(t, svc, box, "spam-folder-moved", "<spam-folder-moved@test>", "Moved spam needle", "needle")
	// A classify move files the message AND marks it Spam; a plain move only
	// files it, so it is not in Spam.
	if _, err := svc.Store.ClassifyMessage(ctx, p, filed.ID, spam.ID); err != nil {
		t.Fatal(err)
	}
	seedInbound(t, svc, box, "spam-folder-clean", "<spam-folder-clean@test>", "Clean needle", "needle")
	key := adminKey(t, svc, u)
	param := "folder=" + url.QueryEscape(spam.ID)
	for _, path := range []string{
		"/v1/inboxes/" + box.ID + "/messages?" + param,
		"/v1/messages?inbox=" + box.ID + "&" + param,
		"/v1/messages?" + param,
		"/v1/inboxes/" + box.ID + "/search?q=needle&" + param,
		"/v1/search?inbox=" + box.ID + "&q=needle&" + param,
		"/v1/search?q=needle&" + param,
	} {
		rr := apiGet(t, h, path, key)
		var page struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &page) != nil {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
		seen := map[string]bool{}
		for _, item := range page.Items {
			seen[item.ID] = true
		}
		if len(page.Items) != 2 || !seen[flagged.ID] || !seen[filed.ID] {
			t.Fatalf("%s: got %v, want both Spam messages (%s and %s)", path, seen, flagged.ID, filed.ID)
		}
	}
	// The same folder id with the role shorthand agrees.
	rr := apiGet(t, h, "/v1/inboxes/"+box.ID+"/messages?spam=true", key)
	var page struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &page) != nil || len(page.Items) != 2 {
		t.Fatalf("spam=true: %d %s", rr.Code, rr.Body.String())
	}
	// The reader's Folder tab lists the same set as the API.
	session, _ := uiSession(t, svc, u.ID)
	rr = uiGet(t, h, session, "/ui/inboxes/"+box.ID+"/folder?folder="+url.QueryEscape(spam.ID))
	if rr.Code != http.StatusOK {
		t.Fatalf("folder tab: %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, m := range []model.Message{flagged, filed} {
		if !strings.Contains(body, m.Subject) {
			t.Fatalf("folder tab missing %q", m.Subject)
		}
	}
	if strings.Contains(body, "Clean needle") {
		t.Fatal("folder tab lists a message that is not Spam")
	}
}

// TestLegacySpamFolderPathMatchesIDForm proves the legacy folder path form
// reaches the same folder and the same membership rule as the stable id form:
// a caller selecting the Spam folder by its path ("Spam") lists the same two
// Spam messages the id form lists, on every route the id form answers, and the
// reader's Folder tab resolves the path too. Before the path lookup existed
// the listing left an exact-placement filter the classified rows never
// satisfy, so the folder read as empty while the id form found the mail.
func TestLegacySpamFolderPathMatchesIDForm(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	spam, err := svc.Store.GetSystemFolder(ctx, u.AccountID, box.ID, model.FolderRoleSpam)
	if err != nil {
		t.Fatal(err)
	}
	flagged := seedInbound(t, svc, box, "spam-path-classified", "<spam-path-classified@test>", "Marked spam path", "needle")
	if _, _, err := svc.Store.SetMessageSpam(ctx, p, flagged.ID, true); err != nil {
		t.Fatal(err)
	}
	filed := seedInbound(t, svc, box, "spam-path-moved", "<spam-path-moved@test>", "Moved spam path", "needle")
	if _, err := svc.Store.ClassifyMessage(ctx, p, filed.ID, spam.ID); err != nil {
		t.Fatal(err)
	}
	seedInbound(t, svc, box, "spam-path-clean", "<spam-path-clean@test>", "Clean path", "needle")
	key := adminKey(t, svc, u)
	// The fixture has exactly one inbox, so the account-wide routes can resolve
	// the shared system path without ambiguity.
	inboxRoute := "/v1/inboxes/" + box.ID + "/messages?folder=Spam"
	for _, path := range []string{
		inboxRoute,
		"/v1/messages?inbox=" + box.ID + "&folder=Spam",
		"/v1/messages?folder=Spam",
		"/v1/inboxes/" + box.ID + "/search?q=needle&folder=Spam",
		"/v1/search?inbox=" + box.ID + "&q=needle&folder=Spam",
		"/v1/search?q=needle&folder=Spam",
		"/v1/inboxes/" + box.ID + "/threads?folder=Spam",
		"/v1/threads?inbox=" + box.ID + "&folder=Spam",
	} {
		rr := apiGet(t, h, path, key)
		var page struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &page) != nil {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
		seen := map[string]bool{}
		for _, item := range page.Items {
			seen[item.ID] = true
		}
		want := []string{flagged.ID, filed.ID}
		if strings.Contains(path, "/threads") {
			// Thread listings return thread keys, not message ids.
			want = []string{flagged.ThreadID, filed.ThreadID}
		}
		if len(page.Items) != 2 || !seen[want[0]] || !seen[want[1]] {
			t.Fatalf("%s: got %v, want both Spam messages (%s and %s)", path, seen, want[0], want[1])
		}
	}
	// The reader's Folder tab resolves the legacy path and lists the same set.
	session, _ := uiSession(t, svc, u.ID)
	rr := uiGet(t, h, session, "/ui/inboxes/"+box.ID+"/folder?folder=Spam")
	if rr.Code != http.StatusOK {
		t.Fatalf("folder tab by path: %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, flagged.Subject) || !strings.Contains(body, filed.Subject) {
		t.Fatalf("folder tab by path missing the Spam mail: %s", body)
	}
	if strings.Contains(body, "Clean path") {
		t.Fatal("folder tab by path lists a message that is not Spam")
	}
}

// TestLegacyFolderPathMatchesIDFormOnEveryRoute proves a domain inbox's legacy
// folder= path form selects exactly what the id form selects. The path used to
// be compared against the message's folder id, so the path form silently
// selected nothing on every route that reads the local store.
func TestLegacyFolderPathMatchesIDFormOnEveryRoute(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	filed := seedInbound(t, svc, box, "legacy-path-inbox", "<legacy-path-inbox@test>", "Filed inbox", "needle")
	inboxFolder, err := svc.Store.GetSystemFolder(ctx, u.AccountID, box.ID, model.FolderRoleInbox)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(inboxFolder.Path, "Inbox") {
		t.Fatalf("seeded Inbox folder path = %q, the legacy form below assumes Inbox", inboxFolder.Path)
	}
	key := adminKey(t, svc, u)
	// The fixture has one inbox, so the account-wide routes resolve the path
	// without ambiguity.
	for _, route := range []struct {
		prefix, suffix string
	}{
		{"/v1/inboxes/" + box.ID + "/messages?", ""},
		{"/v1/messages?inbox=" + box.ID + "&", ""},
		{"/v1/messages?", ""},
		{"/v1/inboxes/" + box.ID + "/search?", "q=needle&"},
		{"/v1/search?inbox=" + box.ID + "&", "q=needle&"},
		{"/v1/search?", "q=needle&"},
		{"/v1/inboxes/" + box.ID + "/threads?", ""},
		{"/v1/threads?inbox=" + box.ID + "&", ""},
	} {
		byPath := apiGet(t, h, route.prefix+route.suffix+"folder=Inbox", key)
		byID := apiGet(t, h, route.prefix+route.suffix+"folder="+url.QueryEscape(inboxFolder.ID), key)
		if byPath.Code != http.StatusOK || byID.Code != http.StatusOK {
			t.Fatalf("%s: path %d %s / id %d %s", route.prefix, byPath.Code, byPath.Body.String(), byID.Code, byID.Body.String())
		}
		var pathPage, idPage struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		if err := json.Unmarshal(byPath.Body.Bytes(), &pathPage); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(byID.Body.Bytes(), &idPage); err != nil {
			t.Fatal(err)
		}
		if len(idPage.Items) == 0 {
			t.Fatalf("%s: the id form is empty, the fixture is wrong", route.prefix)
		}
		if len(pathPage.Items) != len(idPage.Items) {
			t.Fatalf("%s: path form returned %d rows, id form %d", route.prefix, len(pathPage.Items), len(idPage.Items))
		}
		found := false
		for _, item := range pathPage.Items {
			if item.ID == filed.ID || item.ID == filed.ThreadID {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: the path form did not select the Inbox message %s", route.prefix, filed.ID)
		}
	}
}
