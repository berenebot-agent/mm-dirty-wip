package httpapp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
)

func TestAliasWritesRequireAdminBeforeAnyPatchMutation(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	foreignDomain, err := svc.Store.CreateDomain(ctx, u.AccountID, "sibling.test")
	if err != nil {
		t.Fatal(err)
	}
	admin := model.Principal{AccountID: u.AccountID, Admin: true}
	aliases := []store.AliasInput{{DomainID: foreignDomain.ID, LocalPart: "sales", DisplayName: "Sales"}}
	if err := svc.Store.SetInboxAliases(ctx, admin, box.ID, aliases); err != nil {
		t.Fatal(err)
	}
	owner := model.Principal{AccountID: u.AccountID, MailboxRoles: map[string]string{box.ID: "owner"}}
	if err := svc.Store.SetInboxAliases(ctx, owner, box.ID, nil); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("store accepted Owner alias write: %v", err)
	}
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "owner", false, owner.MailboxRoles)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"display_name":"Changed","approver_email":"attacker@example.test","aliases":["new@sibling.test"]}`,
		`{"display_name":"Changed","aliases":[]}`,
		`{"display_name":"Changed","alias_names":{"sales@sibling.test":"Forged"}}`,
	} {
		req := httptest.NewRequest(http.MethodPatch, "/v1/inboxes/"+box.ID, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("Owner PATCH: %d %s", rr.Code, rr.Body.String())
		}
		got, err := svc.Store.GetInboxInternal(ctx, u.AccountID, box.ID)
		if err != nil || got.DisplayName != box.DisplayName || got.ApproverEmail != "" || len(got.Aliases) != 1 || got.AliasNames["sales@sibling.test"] != "Sales" {
			t.Fatalf("rejected PATCH changed inbox: %+v %v", got, err)
		}
	}
}

func TestInboxApproverRedactedAcrossPublicReads(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	const approver = "private-approver@example.test"
	if err := svc.Store.SetInboxApprover(ctx, u.AccountID, box.ID, approver); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"read", "assistant", "owner", "admin"} {
		_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, role, role == "admin", map[string]string{box.ID: role})
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/v1/inboxes", "/v1/inboxes/" + box.ID, "/v1/bootstrap"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer "+key)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != 200 || strings.Contains(rr.Body.String(), approver) != (role != "read") {
				t.Fatalf("%s %s: %d %s", role, path, rr.Code, rr.Body.String())
			}
		}
	}
	internal, err := svc.Store.GetInboxInternal(ctx, u.AccountID, box.ID)
	if err != nil || internal.ApproverEmail != approver {
		t.Fatal("redaction mutated the internal approval setting")
	}
}

func TestTransientEventsRespectInboxVisibilityAndFilter(t *testing.T) {
	t.Parallel()
	svc, h, admin, domain, box := httpFixture(t)
	ctx := context.Background()
	sibling, err := svc.Store.CreateInbox(ctx, admin.AccountID, domain.ID, "sibling", "Sibling")
	if err != nil {
		t.Fatal(err)
	}
	member := accessMember(t, svc, admin.AccountID, admin.ID, "operator@member.test", box.ID)
	ts := httptest.NewServer(h)
	defer ts.Close()
	for _, tc := range []struct {
		name  string
		user  model.User
		query string
	}{
		{"restricted member", member, ""},
		{"filtered admin", admin, "?inbox=" + box.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cookie, _ := uiSession(t, svc, tc.user.ID)
			streamCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(streamCtx, http.MethodGet, ts.URL+"/ui/events/stream"+tc.query, nil)
			req.AddCookie(cookie)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("stream status: %d", resp.StatusCode)
			}
			svc.Hub.Publish(model.Event{AccountID: admin.AccountID, InboxID: sibling.ID, Type: "test.hidden", Transient: true})
			svc.Hub.Publish(model.Event{AccountID: admin.AccountID, Type: "test.account", Transient: true})
			svc.Hub.Publish(model.Event{AccountID: "another-account", InboxID: box.ID, Type: "test.foreign", Transient: true})
			svc.Hub.Publish(model.Event{AccountID: admin.AccountID, InboxID: box.ID, Type: "test.visible", Transient: true})
			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				line := scanner.Text()
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var event model.Event
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
					t.Fatal(err)
				}
				if event.Type != "test.visible" || event.InboxID != box.ID {
					t.Fatalf("unauthorized transient event: %+v", event)
				}
				return
			}
			t.Fatalf("visible sentinel missing: %v", scanner.Err())
		})
	}
}
