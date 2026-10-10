package app_test

import (
	"context"
	"errors"
	"testing"

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
