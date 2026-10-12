package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestMailboxRouterRoutesByKind proves the router sends a domain inbox to the
// local backend and a standalone inbox to the remote backend, with the declared
// capability surface attached, without any remote I/O.
func TestMailboxRouterRoutesByKind(t *testing.T) {
	svc, u, _, domainInbox := testService(t)
	ctx := context.Background()
	router := app.NewMailboxRouter(svc.Store)

	route, err := router.RouteInternal(ctx, u.AccountID, domainInbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if route.Backend != app.BackendLocal {
		t.Fatalf("domain backend = %q, want local", route.Backend)
	}
	if route.Capabilities != model.DomainCapabilities() {
		t.Fatalf("domain capabilities = %+v", route.Capabilities)
	}
	if backend := router.BackendFor(route); backend.Kind() != app.BackendLocal {
		t.Fatalf("BackendFor(domain) = %q", backend.Kind())
	}
	env, err := router.BackendFor(route).ListFolders(ctx, u.AccountID, domainInbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if env.Completeness != model.CompletenessComplete {
		t.Fatalf("domain folders completeness = %q", env.Completeness)
	}

	st := svc.Store
	standalone, err := st.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{
		Address: "agent@remote.example",
		Remote:  &model.RemoteConnection{Host: "imap.remote.example", Username: "agent@remote.example"},
	})
	if err != nil {
		t.Fatal(err)
	}
	route, err = router.RouteInternal(ctx, u.AccountID, standalone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if route.Backend != app.BackendRemote {
		t.Fatalf("standalone backend = %q, want remote", route.Backend)
	}
	// The remote backend lists the (empty) cached folder set and reports the
	// result as unknown-complete: it reflects the last sync, not a live LIST.
	backend := router.BackendFor(route)
	if backend.Kind() != app.BackendRemote {
		t.Fatalf("BackendFor(standalone) = %q", backend.Kind())
	}
	envRemote, err := backend.ListFolders(ctx, u.AccountID, standalone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if envRemote.Completeness != model.CompletenessUnknown {
		t.Fatalf("remote folders completeness = %q, want unknown until synced", envRemote.Completeness)
	}
}

// TestMailboxApplicationProviderCapabilities exercises the callable app
// interfaces, not just HTTP projections. Routing and backend discovery must
// agree on provider push and outbound state without contacting any provider.
func TestMailboxApplicationProviderCapabilities(t *testing.T) {
	svc, u, _, domain := testService(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	router := app.NewMailboxRouter(svc.Store)
	mailboxes := app.NewStandaloneMailboxService(svc)
	create := func(address string, remote *model.RemoteConnection) model.Inbox {
		t.Helper()
		box, err := svc.Store.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: address, Remote: remote})
		if err != nil {
			t.Fatal(err)
		}
		return box
	}
	check := func(id string, want model.Capabilities) {
		t.Helper()
		internal, err := router.RouteInternal(ctx, u.AccountID, id)
		if err != nil {
			t.Fatal(err)
		}
		route, err := router.Route(ctx, p, id)
		if err != nil {
			t.Fatal(err)
		}
		for name, caps := range map[string]model.Capabilities{
			"internal": internal.Capabilities, "authorized": route.Capabilities,
			"capabilities-of": router.CapabilitiesOf(route.Inbox),
			"backend":         router.BackendFor(route).Capabilities(),
		} {
			if caps != want {
				t.Errorf("%s %s = %+v, want %+v", id, name, caps, want)
			}
		}
		if route.Backend == app.BackendRemote {
			box, err := mailboxes.GetStandalone(ctx, p, id)
			if err != nil {
				t.Fatal(err)
			}
			if box.Capabilities == nil || *box.Capabilities != want {
				t.Errorf("get standalone %s capabilities = %+v", id, box.Capabilities)
			}
			if caps := app.NewRemoteBackend(svc.Store, route.Inbox).Capabilities(); caps != want {
				t.Errorf("direct remote backend %s = %+v, want %+v", id, caps, want)
			}
		}
	}
	check(domain.ID, model.DomainCapabilities())
	imap := create("imap@remote.test", nil)
	want := model.StandaloneCapabilities()
	want.Outbound = false
	check(imap.ID, want)
	smtp := create("smtp@remote.test", &model.RemoteConnection{Host: "imap.remote.test", Username: "smtp@remote.test",
		SMTP: &model.RemoteSMTP{Host: "smtp.remote.test", Username: "smtp@remote.test"}})
	want.Outbound = true
	check(smtp.ID, want)
	google := create("google@remote.test", nil)
	if err := svc.Store.SaveGoogleConnection(ctx, store.GoogleConnection{AccountID: u.AccountID, InboxID: google.ID,
		ClientID: "client", EncryptedSecret: "secret", EncryptedToken: "token", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	want.HierarchicalFolders = false
	want.Realtime = false
	check(google.ID, want)
	binding := store.OneBinding{InboxID: "in_app_one_caps", AccountID: u.AccountID, ConnectionID: "connection",
		GmailEmail: "one@remote.test", EncryptedKey: "key", EncryptedConnectionKey: "connection-key",
		KeyAAD: "aad", Fingerprint: "fingerprint", RateGroup: "group"}
	if _, err := svc.Store.ImportOneInbox(ctx, binding, "10"); err != nil {
		t.Fatal(err)
	}
	check(binding.InboxID, want)
	if err := svc.Store.DeleteOneConnection(ctx, u.AccountID, binding.InboxID); err != nil {
		t.Fatal(err)
	}
	want.Outbound = false
	check(binding.InboxID, want)
	microsoft := create("microsoft@remote.test", nil)
	if err := svc.Store.SaveMicrosoftConnection(ctx, store.MicrosoftConnection{AccountID: u.AccountID, InboxID: microsoft.ID,
		ClientID: "client", Tenant: "common", IdentityID: "identity", Mailbox: microsoft.Address, EncryptedToken: "token"}); err != nil {
		t.Fatal(err)
	}
	want = model.StandaloneCapabilities()
	want.Realtime = false
	check(microsoft.ID, want)
	push := store.MicrosoftPush{AccountID: u.AccountID, InboxID: microsoft.ID,
		SubscriptionID: "subscription", Status: "push active", ExpiresAt: time.Now().Add(time.Hour)}
	if err := svc.Store.SaveMicrosoftPush(ctx, push); err != nil {
		t.Fatal(err)
	}
	want.Realtime = true
	check(microsoft.ID, want)
	push.ExpiresAt = time.Now().Add(-time.Hour)
	if err := svc.Store.SaveMicrosoftPush(ctx, push); err != nil {
		t.Fatal(err)
	}
	want.Realtime = false
	check(microsoft.ID, want)
}

// TestStandaloneCreateRequiresAccountAdmin proves creating a standalone inbox is
// an account-level action: an account Admin may create one, but a mailbox Owner
// who is not an account Admin may not.
func TestStandaloneCreateRequiresAccountAdmin(t *testing.T) {
	svc, u, _, _ := testService(t)
	ctx := context.Background()
	mailboxes := app.NewStandaloneMailboxService(svc)

	admin := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
	if _, err := mailboxes.CreateStandalone(ctx, admin, store.StandaloneCreate{Address: "agent@remote.example"}); err != nil {
		t.Fatalf("admin create: %v", err)
	}

	// A mailbox Owner with Owner on every mailbox it can reach is still not an
	// account Admin, so creation is forbidden.
	owner := model.Principal{AccountID: u.AccountID, UserID: "u2", MailboxRoles: map[string]string{"in_x": "owner"}}
	if !owner.OwnsAccount() {
		t.Fatal("precondition: owner should own its account mailboxes")
	}
	_, err := mailboxes.CreateStandalone(ctx, owner, store.StandaloneCreate{Address: "other@remote.example"})
	var mb *model.MailboxError
	if !errors.As(err, &mb) || mb.Kind != model.ErrKindForbidden {
		t.Fatalf("mailbox-owner create err = %v, want forbidden", err)
	}
}
