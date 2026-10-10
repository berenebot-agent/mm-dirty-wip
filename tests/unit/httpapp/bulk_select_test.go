package httpapp_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
)

// TestUIDraftsBulkDelete proves the Drafts folder offers the same checkbox
// selector and bulk bar as the other folders, and that bulk delete removes only
// the selected drafts that belong to that inbox.
func TestUIDraftsBulkDelete(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	admin := model.Principal{AccountID: u.AccountID, Admin: true}
	d1, err := svc.Store.CreateDraft(ctx, admin, model.Draft{InboxID: box.ID, To: []string{"a@example.net"}, Subject: "One", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	d2, err := svc.Store.CreateDraft(ctx, admin, model.Draft{InboxID: box.ID, To: []string{"b@example.net"}, Subject: "Two", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	keep, err := svc.Store.CreateDraft(ctx, admin, model.Draft{InboxID: box.ID, To: []string{"c@example.net"}, Subject: "Keep", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, u.ID)

	// The drafts page carries the selector, the bulk form and the banner.
	page := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts").Body.String()
	for _, want := range []string{`id="drafts-bulk-form"`, `id="select-all"`, `form="drafts-bulk-form"`, `data-select-banner`} {
		if !strings.Contains(page, want) {
			t.Fatalf("drafts page missing %q", want)
		}
	}

	// Bulk delete the two selected drafts (explicit ids, page scope).
	rr := uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts/bulk",
		"_csrf="+csrf+"&scope=page&ids="+d1.ID+"&ids="+d2.ID)
	if rr.Code != 303 {
		t.Fatalf("draft bulk delete = %d body=%s", rr.Code, rr.Body.String())
	}
	if _, err = svc.Store.GetDraft(ctx, admin, d1.ID); err == nil {
		t.Fatal("draft 1 should have been deleted")
	}
	if _, err = svc.Store.GetDraft(ctx, admin, d2.ID); err == nil {
		t.Fatal("draft 2 should have been deleted")
	}
	// The unselected draft survives.
	if _, err = svc.Store.GetDraft(ctx, admin, keep.ID); err != nil {
		t.Fatalf("kept draft should remain: %v", err)
	}
}

// TestUIDraftsBulkDeleteScopeAll proves scope=all deletes every draft in the
// inbox, not just a page.
func TestUIDraftsBulkDeleteScopeAll(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	admin := model.Principal{AccountID: u.AccountID, Admin: true}
	const n = 12
	for i := 0; i < n; i++ {
		if _, err := svc.Store.CreateDraft(ctx, admin, model.Draft{InboxID: box.ID, To: []string{fmt.Sprintf("x%d@example.net", i)}, Subject: fmt.Sprintf("D%d", i), Text: "body"}); err != nil {
			t.Fatal(err)
		}
	}
	cookie, csrf := uiSession(t, svc, u.ID)
	rr := uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts/bulk", "_csrf="+csrf+"&scope=all")
	if rr.Code != 303 {
		t.Fatalf("draft bulk delete scope=all = %d body=%s", rr.Code, rr.Body.String())
	}
	got, err := svc.Store.CountDrafts(ctx, admin, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("after scope=all delete, drafts = %d, want 0", got)
	}
}

// TestUIBulkSelectAllBypassesPageCap seeds more messages than the list page cap
// and proves scope=all acts on the true folder total, not just the first page.
func TestUIBulkSelectAllBypassesPageCap(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	const n = 60 // > inboxPageSize (50)
	for i := 0; i < n; i++ {
		seedInbound(t, svc, box, fmt.Sprintf("cap-%d", i), fmt.Sprintf("<cap-%d@test>", i), fmt.Sprintf("Subject %d", i), "body")
	}
	cookie, csrf := uiSession(t, svc, u.ID)

	// The inbox page advertises the true total on the select banner.
	page := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID).Body.String()
	if !strings.Contains(page, `data-select-banner data-total="`+fmt.Sprint(n)+`"`) {
		t.Fatalf("banner missing true total %d", n)
	}

	rr := uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/bulk",
		"_csrf="+csrf+"&folder=inbox&scope=all&action=delete")
	if rr.Code != 303 {
		t.Fatalf("bulk scope=all delete = %d body=%s", rr.Code, rr.Body.String())
	}
	trashed, err := svc.Store.CountTrash(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if trashed != n {
		t.Fatalf("trashed = %d, want %d (scope=all must bypass the %d page cap)", trashed, n, inboxPageSizeForTest)
	}
}

// inboxPageSizeForTest mirrors the UI page size for the message asserted above.
const inboxPageSizeForTest = 50

// TestUIDraftsPagination proves the drafts list pages with a keyset cursor and
// that the "Load older" link renders for a folder larger than a page.
func TestUIDraftsPagination(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	admin := model.Principal{AccountID: u.AccountID, Admin: true}
	const n = 55 // > inboxPageSize (50)
	for i := 0; i < n; i++ {
		if _, err := svc.Store.CreateDraft(ctx, admin, model.Draft{InboxID: box.ID, To: []string{fmt.Sprintf("p%d@example.net", i)}, Subject: fmt.Sprintf("D%d", i), Text: "body"}); err != nil {
			t.Fatal(err)
		}
	}
	cookie, _ := uiSession(t, svc, u.ID)
	page := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts").Body.String()
	if !strings.Contains(page, "Load older") || !strings.Contains(page, "before=") {
		t.Fatalf("drafts page missing pager for %d drafts", n)
	}
	if !strings.Contains(page, `data-total="`+fmt.Sprint(n)+`"`) {
		t.Fatalf("drafts banner missing true total %d", n)
	}
}
