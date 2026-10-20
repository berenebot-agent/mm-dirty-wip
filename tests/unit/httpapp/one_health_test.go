package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/httpapp"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/one"
	"github.com/dellarb/mailmoose/tests/support/onefixture"
)

// oneHealthEnv is an imported Gmail-via-One inbox backed by the deterministic
// One fixture. The import runs through a dedicated service whose cached clients
// and scheduled background pass are discarded, so each test controls exactly
// which One client and which provider response it exercises.
type oneHealthEnv struct {
	svc     *app.Service
	user    model.User
	cookie  *http.Cookie
	box     model.Inbox
	fixture *onefixture.Fixture
}

func newOneHealthEnv(t *testing.T) *oneHealthEnv {
	t.Helper()
	svc, _, u, _, _, _ := standaloneFixture(t)
	f := onefixture.New(t)
	imp := app.NewRemoteMailboxService(svc)
	imp.OneHTTP, imp.OneInterval = f.Client(t), time.Nanosecond
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
	setup, err := imp.PrepareOneImport(context.Background(), p, onefixture.Key)
	if err != nil {
		t.Fatal(err)
	}
	results, err := imp.ImportOne(context.Background(), p, setup, []string{onefixture.ConnectionID}, nil)
	if err != nil || len(results) != 1 || results[0].Error != "" {
		t.Fatalf("import: %+v %v", results, err)
	}
	imp.Stop()
	box, err := svc.Store.GetInboxInternal(context.Background(), u.AccountID, results[0].InboxID)
	if err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)
	return &oneHealthEnv{svc: svc, user: u, cookie: cookie, box: box, fixture: f}
}

// remote returns a service wired to the fixture with an empty client cache, so
// the test's first operation is the one that builds (and validates) the client.
func (e *oneHealthEnv) remote(t *testing.T) *app.RemoteMailboxService {
	t.Helper()
	rm := app.NewRemoteMailboxService(e.svc)
	t.Cleanup(rm.Stop)
	rm.OneHTTP, rm.OneInterval = e.fixture.Client(t), time.Nanosecond
	return rm
}

// TestOneHealthRequiresRepeatedRejection pins the threshold: one rejected
// authorization must not mark the inbox unavailable, because a transient
// rejection used to strand it on a state only a manual re-save could clear.
// Consecutive failures are counted, and only the threshold renders the
// reconnect prompt.
func TestOneHealthRequiresRepeatedRejection(t *testing.T) {
	t.Parallel()
	env := newOneHealthEnv(t)
	ctx := context.Background()
	env.fixture.Change(func(f *onefixture.Fixture) {
		f.Status["GET /v1/passthrough/gmail/v1/users/me/profile"] = 401
	})
	for attempt := 1; attempt <= store.OneFailureThreshold; attempt++ {
		rm := env.remote(t)
		client, token, err := rm.GoogleMailboxClient(ctx, env.user.AccountID, env.box.ID)
		if err == nil {
			_, err = client.Profile(ctx, token)
		}
		if err == nil {
			t.Fatalf("attempt %d: a rejected authorization must fail the operation", attempt)
		}
		b, berr := env.svc.Store.OneBinding(ctx, env.user.AccountID, env.box.ID)
		if berr != nil {
			t.Fatal(berr)
		}
		wantStatus := "active"
		if attempt >= store.OneFailureThreshold {
			wantStatus = "attention"
		}
		if b.Status != wantStatus || b.FailureCount != attempt {
			t.Fatalf("attempt %d: status=%q failures=%d, want status=%q failures=%d", attempt, b.Status, b.FailureCount, wantStatus, attempt)
		}
	}
}

// TestOneHealthSelfHealsWhenOneConfirmsHealthy proves the flagged binding is not
// a dead end: the next attempt revalidates against One, and a connection One
// still accepts returns the inbox to service without an operator.
func TestOneHealthSelfHealsWhenOneConfirmsHealthy(t *testing.T) {
	t.Parallel()
	env := newOneHealthEnv(t)
	ctx := context.Background()
	b, err := env.svc.Store.OneBinding(ctx, env.user.AccountID, env.box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.svc.Store.SetOneHealth(ctx, env.user.AccountID, env.box.ID, "attention", "One connection revoked or expired", b.Revision); err != nil {
		t.Fatal(err)
	}
	rm := env.remote(t)
	client, token, err := rm.GoogleMailboxClient(ctx, env.user.AccountID, env.box.ID)
	if err != nil {
		t.Fatalf("a connection One still accepts must not stay unavailable: %v", err)
	}
	if _, err := client.Profile(ctx, token); err != nil {
		t.Fatal(err)
	}
	after, err := env.svc.Store.OneBinding(ctx, env.user.AccountID, env.box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "active" || after.FailureCount != 0 || after.LastError != "" {
		t.Fatalf("binding did not recover: status=%q failures=%d error=%q", after.Status, after.FailureCount, after.LastError)
	}
}

// TestOneHealthKeepsUnavailableWhenConnectionUnusable is the counterpoint: when
// One no longer exposes the connection, revalidation must fail and the verdict
// must stand, so the reconnect prompt is not cleared by wishful retrying.
func TestOneHealthKeepsUnavailableWhenConnectionUnusable(t *testing.T) {
	t.Parallel()
	env := newOneHealthEnv(t)
	ctx := context.Background()
	b, err := env.svc.Store.OneBinding(ctx, env.user.AccountID, env.box.ID)
	if err != nil {
		t.Fatal(err)
	}
	env.fixture.Change(func(f *onefixture.Fixture) {
		for i := range f.Connections {
			if f.Connections[i]["id"] == onefixture.ConnectionID {
				f.Connections[i]["state"] = "reconnect_required"
			}
		}
	})
	if err := env.svc.Store.SetOneHealth(ctx, env.user.AccountID, env.box.ID, "attention", "One connection revoked or expired", b.Revision); err != nil {
		t.Fatal(err)
	}
	rm := env.remote(t)
	if _, _, err := rm.GoogleMailboxClient(ctx, env.user.AccountID, env.box.ID); err == nil {
		t.Fatal("an unusable connection must not resume")
	}
	after, err := env.svc.Store.OneBinding(ctx, env.user.AccountID, env.box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "attention" {
		t.Fatalf("status = %q, want attention", after.Status)
	}
}

// TestOneHealthIgnoresConnectorActionFailure proves a connector-level problem
// (One no longer exposing a mandatory Gmail action) is not treated as a
// credential rejection. Flagging it left the inbox pinned in "attention", which
// only a manual reconnect could clear, so a transient mapping hiccup read as a
// permanent disconnect.
func TestOneHealthIgnoresConnectorActionFailure(t *testing.T) {
	t.Parallel()
	env := newOneHealthEnv(t)
	ctx := context.Background()
	env.fixture.Change(func(f *onefixture.Fixture) {
		kept := make([]one.Action, 0, len(f.Actions))
		for _, a := range f.Actions {
			if a.Method == "GET" && a.Path == "/gmail/v1/users/{{userId}}/profile" {
				continue
			}
			kept = append(kept, a)
		}
		f.Actions = kept
	})
	rm := env.remote(t)
	client, token, err := rm.GoogleMailboxClient(ctx, env.user.AccountID, env.box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Profile(ctx, token); err == nil {
		t.Fatal("an inbox with no validated action mapping must fail the operation")
	}
	b, err := env.svc.Store.OneBinding(ctx, env.user.AccountID, env.box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "active" {
		t.Fatalf("a connector action problem must not read as a credential rejection: status=%q error=%q", b.Status, b.LastError)
	}
}

// TestOneHealthKeepsVerdictAcrossTransientFailure pins the invariant that a
// transient outcome cannot lift a recorded authorization failure, so a
// rate-limited or timed-out attempt never reads as a healthy connection. Only a
// revalidation One accepts may clear the verdict.
func TestOneHealthKeepsVerdictAcrossTransientFailure(t *testing.T) {
	t.Parallel()
	env := newOneHealthEnv(t)
	ctx := context.Background()
	b, err := env.svc.Store.OneBinding(ctx, env.user.AccountID, env.box.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < store.OneFailureThreshold; i++ {
		if err := env.svc.Store.RecordOneFailure(ctx, env.user.AccountID, env.box.ID, "One connection revoked or expired", b.Revision); err != nil {
			t.Fatal(err)
		}
	}
	env.fixture.Change(func(f *onefixture.Fixture) {
		f.Status["GET /v1/passthrough/gmail/v1/users/me/profile"] = 429
	})
	rm := env.remote(t)
	if _, _, err := rm.GoogleMailboxClient(ctx, env.user.AccountID, env.box.ID); err == nil {
		t.Fatal("a rate-limited attempt must surface an error, not a live client")
	}
	current, err := env.svc.Store.OneBinding(ctx, env.user.AccountID, env.box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "attention" || current.FailureCount != store.OneFailureThreshold || !strings.Contains(current.LastError, "revoked or expired") {
		t.Fatalf("a transient failure replaced the recorded auth verdict: status=%q failures=%d error=%q", current.Status, current.FailureCount, current.LastError)
	}
}

// TestRemoteMessageBodyFailureCarriesReconnectVerdict proves the reader is told
// when the body fetch failed because the connection was revoked, so it can offer
// the reconnect prompt instead of a bare "Could not load the message body". The
// authorization is revoked after the shell has painted, which is the window the
// server-rendered banner cannot cover.
func TestRemoteMessageBodyFailureCarriesReconnectVerdict(t *testing.T) {
	t.Parallel()
	env := newOneHealthEnv(t)
	ctx := context.Background()
	rm := env.remote(t)
	srv := httpapp.New(env.svc, nil)
	srv.SetRemoteMailbox(rm)
	h := srv.Handler()
	if _, err := rm.ReconcileRemote(ctx, env.user.AccountID, env.box.ID); err != nil {
		t.Fatal(err)
	}
	id, err := env.svc.Store.GoogleLocalID(ctx, env.user.AccountID, env.box.ID, "message-one")
	if err != nil {
		t.Fatal(err)
	}
	// One stops exposing the connection, and repeated rejections mark the
	// binding unavailable, so the live body fetch is the attempt that fails.
	env.fixture.Change(func(f *onefixture.Fixture) {
		for i := range f.Connections {
			if f.Connections[i]["id"] == onefixture.ConnectionID {
				f.Connections[i]["state"] = "reconnect_required"
			}
		}
	})
	b, err := env.svc.Store.OneBinding(ctx, env.user.AccountID, env.box.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < store.OneFailureThreshold; i++ {
		if err := env.svc.Store.RecordOneFailure(ctx, env.user.AccountID, env.box.ID, "One connection revoked or expired", b.Revision); err != nil {
			t.Fatal(err)
		}
	}
	rr := uiGet(t, h, env.cookie, "/ui/messages/"+id+"/body")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("body status = %d: %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Connection *struct {
			State     string `json:"state"`
			Reconnect string `json:"reconnect"`
		} `json:"connection"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Connection == nil || out.Connection.State != "disconnected" || out.Connection.Reconnect != "one" {
		t.Fatalf("a body failure must carry the reconnect verdict: %s", rr.Body.String())
	}
}
