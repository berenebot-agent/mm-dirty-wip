package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// remoteCadenceView decodes the sync-cadence fields of a standalone inbox's
// secret-free remote configuration view (GET /v1/inboxes/{id}/remote).
func remoteCadenceView(t *testing.T, h http.Handler, inboxID, key string) (string, int, int) {
	t.Helper()
	rr := apiGet(t, h, "/v1/inboxes/"+inboxID+"/remote", key)
	if rr.Code != http.StatusOK {
		t.Fatalf("remote config = %d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Provider        string `json:"provider"`
		PollSeconds     int    `json:"poll_seconds"`
		FullSyncMinutes int    `json:"full_sync_minutes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode remote config: %v", err)
	}
	return out.Provider, out.PollSeconds, out.FullSyncMinutes
}

// TestRemoteConfigReportsCadenceForEveryStandaloneProvider proves the sync
// cadence is reported for every standalone provider, not only a hand-configured
// IMAP binding. A Microsoft delegated shared mailbox is the case that matters
// most: Graph has no change subscriptions for a delegated shared mailbox, so its
// poll cadence is the mailbox's only delivery knob, and an unreported cadence
// leaves the Identity tab with nothing to show for it. An unset override resolves
// to the model defaults, so the form always has an effective value to display.
func TestRemoteConfigReportsCadenceForEveryStandaloneProvider(t *testing.T) {
	svc, h, u, _, standalone, _ := standaloneFixture(t)
	ctx := context.Background()
	key := adminKey(t, svc, u)

	// The fixture's IMAP binding carries a password but no cadence override, so
	// it reports the inherited defaults.
	if provider, poll, full := remoteCadenceView(t, h, standalone.ID, key); provider != "imap" ||
		poll != model.RemotePollSecondsDefault || full != model.RemoteFullSyncMinutesDefault {
		t.Fatalf("imap cadence = %q %d/%d, want the inherited defaults", provider, poll, full)
	}
	// An explicit override is reported as stored.
	if err := svc.Store.SetInboxRemoteSync(ctx, u.AccountID, standalone.ID, intPtr(120), intPtr(30)); err != nil {
		t.Fatal(err)
	}
	if _, poll, full := remoteCadenceView(t, h, standalone.ID, key); poll != 120 || full != 30 {
		t.Fatalf("imap override = %d/%d, want 120/30", poll, full)
	}

	microsoft, err := svc.Store.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "shared@remote.test"})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.SaveMicrosoftConnection(ctx, store.MicrosoftConnection{
		AccountID: u.AccountID, InboxID: microsoft.ID, ClientID: "client", Tenant: "common",
		IdentityID: "identity", Mailbox: microsoft.Address, Shared: true, EncryptedToken: "encrypted",
	}); err != nil {
		t.Fatal(err)
	}
	if provider, poll, full := remoteCadenceView(t, h, microsoft.ID, key); provider != "microsoft" ||
		poll != model.RemotePollSecondsDefault || full != model.RemoteFullSyncMinutesDefault {
		t.Fatalf("microsoft cadence = %q %d/%d, want the inherited defaults", provider, poll, full)
	}

	google, err := svc.Store.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "gmail@remote.test"})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.SaveGoogleConnection(ctx, store.GoogleConnection{
		InboxID: google.ID, AccountID: u.AccountID, ClientID: "client",
		EncryptedSecret: "secret", EncryptedToken: "encrypted", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.SetInboxRemoteSync(ctx, u.AccountID, google.ID, intPtr(300), intPtr(60)); err != nil {
		t.Fatal(err)
	}
	if provider, poll, full := remoteCadenceView(t, h, google.ID, key); provider != "google" || poll != 300 || full != 60 {
		t.Fatalf("google cadence = %q %d/%d, want 300/60", provider, poll, full)
	}
}

// TestInboxSettingsSaveCadenceKeepsProviderGrant proves an Identity-tab save
// writes the cadence of a provider-connected inbox and leaves its OAuth grant
// intact. The shared settings form also carries the IMAP/SMTP connector fields
// (hidden and disabled for a provider inbox, so they arrive blank), which is
// exactly the path where a blank form was once read as "clear the connector":
// the save answered 400 from ConfigureStandaloneRemote, which refuses a provider
// inbox outright, so no Identity-tab setting could be saved at all.
func TestInboxSettingsSaveCadenceKeepsProviderGrant(t *testing.T) {
	for _, provider := range []string{"microsoft", "google"} {
		t.Run(provider, func(t *testing.T) {
			svc, h, u, _, _, _ := standaloneFixture(t)
			ctx := context.Background()
			box, err := svc.Store.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: provider + "@remote.test"})
			if err != nil {
				t.Fatal(err)
			}
			if provider == "microsoft" {
				err = svc.Store.SaveMicrosoftConnection(ctx, store.MicrosoftConnection{
					AccountID: u.AccountID, InboxID: box.ID, ClientID: "client", Tenant: "common",
					IdentityID: "identity", Mailbox: box.Address, Shared: true, EncryptedToken: "encrypted",
				})
			} else {
				err = svc.Store.SaveGoogleConnection(ctx, store.GoogleConnection{
					InboxID: box.ID, AccountID: u.AccountID, ClientID: "client",
					EncryptedSecret: "secret", EncryptedToken: "encrypted", ExpiresAt: time.Now().Add(time.Hour),
				})
			}
			if err != nil {
				t.Fatal(err)
			}

			cookie, csrf := uiSession(t, svc, u.ID)
			// The Identity tab states the poll in whole minutes.
			rr := uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/edit",
				"_csrf="+csrf+"&display=Shared&remote_poll_minutes=2&remote_full_sync_minutes=30")
			if rr.Code != http.StatusSeeOther {
				t.Fatalf("settings save = %d location=%q body=%s", rr.Code, rr.Header().Get("Location"), rr.Body.String())
			}
			// A provider inbox has no IMAP binding to verify, so the save must not
			// bounce through the connector test: that answered a redirect carrying a
			// bogus "IMAP connection failed" error and persisted nothing at all.
			if loc := rr.Header().Get("Location"); strings.Contains(loc, "error=") {
				t.Fatalf("settings save redirected with an error: %q", loc)
			}
			// The provider grant and its configured marker survive the save.
			if provider == "microsoft" {
				c, cerr := svc.Store.MicrosoftConnection(ctx, u.AccountID, box.ID)
				if cerr != nil || c.Mailbox != box.Address || !c.Shared {
					t.Fatalf("microsoft grant after save = %#v %v", c, cerr)
				}
			} else {
				if _, cerr := svc.Store.GoogleConnection(ctx, u.AccountID, box.ID); cerr != nil {
					t.Fatalf("google grant after save = %v", cerr)
				}
			}
			stored, err := svc.Store.GetInboxInternal(ctx, u.AccountID, box.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !stored.RemoteConfigured {
				t.Fatal("save cleared the configured marker of a provider inbox")
			}
			if stored.RemotePollSeconds == nil || *stored.RemotePollSeconds != 120 ||
				stored.RemoteFullSyncMinutes == nil || *stored.RemoteFullSyncMinutes != 30 {
				t.Fatalf("cadence after save = %v/%v, want 120/30", stored.RemotePollSeconds, stored.RemoteFullSyncMinutes)
			}
		})
	}
}

// TestInboxSettingsPollMinutesConvertedAndBounded proves the Identity tab's
// new-mail poll is entered in whole minutes and converted to the column's
// seconds, that a blank field keeps inheriting the process default (no override
// row), and that the one-minute floor rejects a zero instead of storing a poll
// of nothing. The connector fields are submitted as the dialog sends them, so
// the save is a real Identity-tab save rather than a partial form.
func TestInboxSettingsPollMinutesConvertedAndBounded(t *testing.T) {
	svc, h, u, _, standalone, _ := standaloneFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)
	// The fixture's connector, resubmitted unchanged so the save is not read as a
	// connector update and does not run the live IMAP test.
	connector := "&remote_host=imap.remote.test&remote_port=143&remote_username=agent@remote.test&remote_security=plain&namespace="
	post := func(poll string) *httptest.ResponseRecorder {
		return uiPost(t, h, cookie, "/ui/inboxes/"+standalone.ID+"/edit",
			"_csrf="+csrf+"&display=Agent"+connector+"&remote_poll_minutes="+poll)
	}

	// Five minutes is stored as 300 seconds.
	if rr := post("5"); rr.Code != http.StatusSeeOther {
		t.Fatalf("minutes save = %d body=%s", rr.Code, rr.Body.String())
	}
	box, err := svc.Store.GetInboxInternal(ctx, u.AccountID, standalone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if box.RemotePollSeconds == nil || *box.RemotePollSeconds != 300 {
		t.Fatalf("poll seconds = %v, want 300", box.RemotePollSeconds)
	}

	// The floor is one minute, so a zero is refused rather than stored.
	if rr := post("0"); rr.Code != http.StatusBadRequest {
		t.Fatalf("zero minutes = %d, want 400", rr.Code)
	}

	// A blank field inherits the default again, clearing the override.
	if rr := post(""); rr.Code != http.StatusSeeOther {
		t.Fatalf("blank poll = %d body=%s", rr.Code, rr.Body.String())
	}
	box, err = svc.Store.GetInboxInternal(ctx, u.AccountID, standalone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if box.RemotePollSeconds != nil {
		t.Fatalf("blank poll kept an override: %v", *box.RemotePollSeconds)
	}
}

// remoteRealtime reads capabilities.realtime from a standalone inbox's
// secret-free remote view (GET /v1/inboxes/{id}/remote).
func remoteRealtime(t *testing.T, h http.Handler, inboxID, key string) bool {
	t.Helper()
	rr := apiGet(t, h, "/v1/inboxes/"+inboxID+"/remote", key)
	if rr.Code != http.StatusOK {
		t.Fatalf("remote config = %d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Capabilities struct {
			Realtime bool `json:"realtime"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode remote config: %v", err)
	}
	return out.Capabilities.Realtime
}

// TestRealtimeCapabilityReflectsPush proves capabilities.realtime reports the
// mailbox's actual push state rather than the standalone default. Gmail has no
// push subscription — native or via One — and is poll-only, so it must never
// claim instant push. A Microsoft inbox without an active Graph subscription and
// a plain IMAP inbox are included for contrast: IMAP keeps the flag because IDLE
// can deliver immediately, the others do not.
func TestRealtimeCapabilityReflectsPush(t *testing.T) {
	svc, h, u, _, standalone, _ := standaloneFixture(t)
	ctx := context.Background()
	key := adminKey(t, svc, u)

	// A plain IMAP standalone inbox keeps the default: IDLE can push, so the
	// capability stays advertised.
	if !remoteRealtime(t, h, standalone.ID, key) {
		t.Fatal("imap inbox lost its realtime capability")
	}

	google, err := svc.Store.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "gmail@remote.test"})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.SaveGoogleConnection(ctx, store.GoogleConnection{
		InboxID: google.ID, AccountID: u.AccountID, ClientID: "client",
		EncryptedSecret: "secret", EncryptedToken: "encrypted", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if remoteRealtime(t, h, google.ID, key) {
		t.Fatal("gmail inbox claimed instant push")
	}

	microsoft, err := svc.Store.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "shared@remote.test"})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.SaveMicrosoftConnection(ctx, store.MicrosoftConnection{
		AccountID: u.AccountID, InboxID: microsoft.ID, ClientID: "client", Tenant: "common",
		IdentityID: "identity", Mailbox: microsoft.Address, Shared: true, EncryptedToken: "encrypted",
	}); err != nil {
		t.Fatal(err)
	}
	if remoteRealtime(t, h, microsoft.ID, key) {
		t.Fatal("microsoft inbox without an active subscription claimed instant push")
	}

	// Gmail via One has no push setup either.
	oneBinding := store.OneBinding{
		InboxID: "in_one_realtime", AccountID: u.AccountID, ConnectionID: "conn-one",
		GmailEmail: "one@remote.test", EncryptedKey: "key", EncryptedConnectionKey: "conn-key",
		KeyAAD: "aad", Fingerprint: "fp", RateGroup: "group",
	}
	if _, err = svc.Store.ImportOneInbox(ctx, oneBinding, "10"); err != nil {
		t.Fatal(err)
	}
	if remoteRealtime(t, h, oneBinding.InboxID, key) {
		t.Fatal("gmail-via-one inbox claimed instant push")
	}

	// The account-wide listing must include capabilities for IMAP as well as
	// provider inboxes, without claiming push for a poll-only provider.
	rr := apiGet(t, h, "/v1/inboxes", key)
	if rr.Code != http.StatusOK {
		t.Fatalf("list inboxes = %d body=%s", rr.Code, rr.Body.String())
	}
	var listed []struct {
		ID           string `json:"id"`
		Capabilities *struct {
			Realtime bool `json:"realtime"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode inbox list: %v", err)
	}
	present := map[string]bool{}
	realtime := map[string]bool{}
	for _, b := range listed {
		if b.Capabilities != nil {
			present[b.ID] = true
			realtime[b.ID] = b.Capabilities.Realtime
		}
	}
	for _, id := range []string{google.ID, microsoft.ID, oneBinding.InboxID} {
		if !present[id] {
			t.Fatalf("list omitted capabilities for %s", id)
		}
		if realtime[id] {
			t.Fatalf("list reported instant push for %s", id)
		}
	}
	if !present[standalone.ID] || !realtime[standalone.ID] {
		t.Fatal("list omitted the declared imap realtime capability")
	}
}

// TestInboxListingOutboundCapabilities checks the IMAP connector and One's
// configured marker in the public listing, including the retained One tombstone.
func TestInboxListingOutboundCapabilities(t *testing.T) {
	svc, h, u, _, imap, _ := standaloneFixture(t)
	ctx := context.Background()
	key := adminKey(t, svc, u)
	smtp, err := svc.Store.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{
		Address: "smtp@remote.test",
		Remote: &model.RemoteConnection{Host: "imap.remote.test", Username: "smtp@remote.test",
			SMTP: &model.RemoteSMTP{Host: "smtp.remote.test", Username: "smtp@remote.test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := store.OneBinding{InboxID: "in_one_outbound", AccountID: u.AccountID,
		ConnectionID: "outbound-connection", GmailEmail: "one-outbound@remote.test",
		EncryptedKey: "key", EncryptedConnectionKey: "connection-key", KeyAAD: "aad",
		Fingerprint: "fingerprint", RateGroup: "group"}
	if _, err = svc.Store.ImportOneInbox(ctx, binding, "10"); err != nil {
		t.Fatal(err)
	}
	check := func(oneOutbound bool) {
		t.Helper()
		rr := apiGet(t, h, "/v1/inboxes", key)
		if rr.Code != http.StatusOK {
			t.Fatalf("list = %d: %s", rr.Code, rr.Body.String())
		}
		var boxes []model.Inbox
		if err := json.Unmarshal(rr.Body.Bytes(), &boxes); err != nil {
			t.Fatal(err)
		}
		want := map[string]bool{imap.ID: false, smtp.ID: true, binding.InboxID: oneOutbound}
		for _, box := range boxes {
			outbound, ok := want[box.ID]
			if !ok {
				continue
			}
			if box.Capabilities == nil || box.Capabilities.Outbound != outbound {
				t.Fatalf("%s capabilities = %+v, want outbound=%v", box.ID, box.Capabilities, outbound)
			}
			delete(want, box.ID)
		}
		if len(want) != 0 {
			t.Fatalf("missing inboxes: %v", want)
		}
	}
	check(true)
	if err = svc.Store.DeleteOneConnection(ctx, u.AccountID, binding.InboxID); err != nil {
		t.Fatal(err)
	}
	check(false)
}

// intPtr returns a pointer to v, for the optional per-inbox cadence overrides.
func intPtr(v int) *int { return &v }
