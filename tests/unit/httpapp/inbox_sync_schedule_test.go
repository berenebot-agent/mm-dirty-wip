package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
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
			rr := uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/edit",
				"_csrf="+csrf+"&display=Shared&remote_poll_seconds=120&remote_full_sync_minutes=30")
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

// intPtr returns a pointer to v, for the optional per-inbox cadence overrides.
func intPtr(v int) *int { return &v }
