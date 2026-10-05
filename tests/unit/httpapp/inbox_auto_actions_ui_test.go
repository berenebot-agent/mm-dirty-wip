package httpapp_test

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

// TestInboxEditSaveKeepsAutoActions pins the regression that made an unrelated
// inbox edit clear the delivery auto-action policy. The auto-action controls
// live in their own form on the Connectors tab, so they are absent from a save
// posted from any other tab; absence must mean "leave it alone", not "off".
func TestInboxEditSaveKeepsAutoActions(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()

	// Seed a policy as the connector wizard/API would.
	markRead := true
	hours := 4
	trigger := "all"
	if err := svc.Store.SetInboxAutoActions(ctx, u.AccountID, box.ID, &markRead, &hours, &trigger); err != nil {
		t.Fatal(err)
	}

	cookie, csrf := uiSession(t, svc, u.ID)
	// An ordinary inbox save: the Quota/Trash controls plus the display name,
	// and none of the auto-action fields.
	body := url.Values{
		"_csrf":                    {csrf},
		"display":                  {"Renamed"},
		"approver_email":           {""},
		"quota_unlimited":          {"1"},
		"trash_retention_override": {"1"},
		"trash_retention_days":     {"9"},
		"default_sender":           {""},
	}.Encode()
	if rr := uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/edit", body); rr.Code != 303 {
		t.Fatalf("inbox save = %d %s", rr.Code, rr.Body.String())
	}

	got, err := svc.Store.GetInboxInternal(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AutoMarkReadOnDelivery || got.AutoTrashAfterDeliveryHours == nil || *got.AutoTrashAfterDeliveryHours != 4 || got.DeliveryTrigger != "all" {
		t.Fatalf("unrelated save cleared the auto-action policy: %#v", got)
	}
	if got.TrashRetentionDays == nil || *got.TrashRetentionDays != 9 {
		t.Fatalf("trash retention not applied: %#v", got.TrashRetentionDays)
	}
	if got.DisplayName != "Renamed" {
		t.Fatalf("display name not applied: %q", got.DisplayName)
	}
}

// TestInboxAutoActionsFormIsAuthoritative pins the other half: when the
// auto-action controls ARE submitted, the form wins — an unchecked auto-trash
// box clears the window rather than leaving a stale value.
func TestInboxAutoActionsFormIsAuthoritative(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	markRead := true
	hours := 12
	if err := svc.Store.SetInboxAutoActions(ctx, u.AccountID, box.ID, &markRead, &hours, nil); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, u.ID)

	// Only the mark-read box is ticked; the trigger is left at any.
	body := url.Values{"_csrf": {csrf}, "auto_mark_read_on_delivery": {"1"}, "delivery_trigger": {"any"}}.Encode()
	if rr := uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/auto-actions", body); rr.Code != 303 {
		t.Fatalf("auto-actions save = %d %s", rr.Code, rr.Body.String())
	}
	got, err := svc.Store.GetInboxInternal(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AutoMarkReadOnDelivery || got.AutoTrashAfterDeliveryHours != nil || got.DeliveryTrigger != "any" {
		t.Fatalf("auto-actions form not authoritative: %#v", got)
	}

	// Clearing the mark-read box turns it back off.
	body = url.Values{"_csrf": {csrf}, "delivery_trigger": {"all"}}.Encode()
	if rr := uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/auto-actions", body); rr.Code != 303 {
		t.Fatalf("auto-actions clear = %d %s", rr.Code, rr.Body.String())
	}
	got, _ = svc.Store.GetInboxInternal(ctx, u.AccountID, box.ID)
	if got.AutoMarkReadOnDelivery || got.DeliveryTrigger != "all" {
		t.Fatalf("auto-actions not cleared: %#v", got)
	}
}

// TestInboxConnectorsTabHasItsOwnForm pins the markup contract the Connectors
// tab's save control depends on: the auto-action controls are inside a form of
// their own, outside the main inbox edit form, so the two saves cannot post each
// other's fields. Without this the tab's controls are unreachable, because the
// dialog hides its own save button while that tab is active.
func TestInboxConnectorsTabHasItsOwnForm(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)
	page := uiGet(t, h, cookie, "/?inbox="+box.ID+"&inbox_tab=connectors")
	if page.Code != 200 {
		t.Fatalf("dashboard = %d", page.Code)
	}
	body := page.Body.String()

	form := dialogHTML(t, body, "inbox-edit-dialog")
	if !strings.Contains(form, `id="inbox-connectors-form"`) {
		t.Fatalf("connectors tab has no form of its own:\n%s", form)
	}
	if !strings.Contains(form, `id="inbox-connectors-save"`) {
		t.Fatalf("connectors tab has no save control:\n%s", form)
	}
	// The auto-action controls must sit inside the connectors form: check the
	// ordering, since both strings appear somewhere in the dialog.
	connForm := form[strings.Index(form, `id="inbox-connectors-form"`):]
	closeMain := strings.Index(form, `</form>`)
	markRead := strings.Index(form, `id="inbox-auto-mark-read"`)
	if closeMain < 0 || markRead < 0 {
		t.Fatalf("markers missing from dialog:\n%s", form)
	}
	if markRead < closeMain {
		t.Fatalf("auto-action controls are inside the main inbox edit form")
	}
	if !strings.Contains(connForm, `id="inbox-auto-mark-read"`) {
		t.Fatalf("auto-action controls are not inside the connectors form")
	}
}

// TestInboxAutoActionsBlankTriggerDefaultsToAll pins the default a submitted
// form falls back to when it omits the trigger: "all", not "any". A form that
// reaches the endpoint without the field would otherwise silently narrow the
// policy to the first delivery.
func TestInboxAutoActionsBlankTriggerDefaultsToAll(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)

	body := url.Values{"_csrf": {csrf}, "auto_mark_read_on_delivery": {"1"}}.Encode()
	if rr := uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/auto-actions", body); rr.Code != 303 {
		t.Fatalf("auto-actions save = %d %s", rr.Code, rr.Body.String())
	}
	got, err := svc.Store.GetInboxInternal(context.Background(), u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DeliveryTrigger != "all" {
		t.Fatalf("blank trigger stored as %q, want all", got.DeliveryTrigger)
	}
}

// assertDefaultDeliveryTrigger checks that every delivery-trigger select in the
// markup opens on "all". The connector-create dialog carries one per connector
// kind and the Connectors tab another; none of them is populated from the
// stored value until its dialog is opened, so the option marked selected is the
// default a user saves.
func assertDefaultDeliveryTrigger(t *testing.T, markup string) {
	t.Helper()
	const open = `<select name="delivery_trigger"`
	found := 0
	for rest := markup; ; {
		i := strings.Index(rest, open)
		if i < 0 {
			break
		}
		rest = rest[i:]
		end := strings.Index(rest, "</select>")
		if end < 0 {
			t.Fatalf("delivery trigger select is not closed:\n%s", rest)
		}
		body := rest[len(open):end]
		found++
		if !strings.Contains(body, `<option value="all" selected>`) {
			t.Fatalf("delivery trigger select does not open on all:\n%s", body)
		}
		if strings.Contains(body, `<option value="any" selected>`) {
			t.Fatalf("delivery trigger select opens on any:\n%s", body)
		}
		rest = rest[end:]
	}
	if found == 0 {
		t.Fatal("no delivery trigger select in the page")
	}
}

// TestDeliveryTriggerControlsDefaultToAll pins the markup the default rides on:
// the trigger select must present "all" as the preselected option, so a user who
// never touches it saves the default rather than the old "any".
func TestDeliveryTriggerControlsDefaultToAll(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)
	page := uiGet(t, h, cookie, "/?inbox="+box.ID+"&inbox_tab=connectors")
	if page.Code != 200 {
		t.Fatalf("dashboard = %d", page.Code)
	}
	assertDefaultDeliveryTrigger(t, page.Body.String())
}
