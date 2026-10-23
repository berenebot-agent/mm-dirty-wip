package httpapp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/httpapp"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/microsoft"
)

func TestMicrosoftMailboxMovesAndPermissions(t *testing.T) {
	t.Parallel()
	svc, _, u, _, _, _ := standaloneFixture(t)
	ctx := context.Background()
	box, e := svc.Store.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "shared@example.net"})
	if e != nil {
		t.Fatal(e)
	}
	folder := "inbox-id"
	provider := "StableID"
	expired := false
	read := false
	var categories []string
	masterCategories := []string{"Projects", "Clients"}
	var fixture *httptest.Server
	fixture = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access" || r.Header.Get("Prefer") != `IdType="ImmutableId"` {
			t.Errorf("missing Graph headers")
		}
		if !strings.HasPrefix(r.URL.Path, "/users/shared@example.net/") {
			t.Errorf("escaped mailbox boundary: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		message := map[string]any{"id": provider, "conversationId": "thread", "parentFolderId": folder, "subject": "Hello", "isRead": read, "categories": categories, "receivedDateTime": "2026-10-10T10:00:00Z"}
		switch {
		case strings.HasSuffix(r.URL.Path, "/outlook/masterCategories"):
			if r.Method == http.MethodPost {
				var body struct {
					DisplayName string `json:"displayName"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				masterCategories = append(masterCategories, body.DisplayName)
				json.NewEncoder(w).Encode(map[string]any{"id": "category-" + body.DisplayName, "displayName": body.DisplayName, "color": "preset0"})
				return
			}
			value := []any{}
			for _, name := range masterCategories {
				value = append(value, map[string]any{"id": "category-" + name, "displayName": name, "color": "preset0"})
			}
			json.NewEncoder(w).Encode(map[string]any{"value": value})
		case strings.HasPrefix(r.URL.Path, "/users/shared@example.net/outlook/masterCategories/"):
			// A single master category is addressed by its own id; Graph renames
			// with PATCH and deletes with DELETE.
			id := strings.TrimPrefix(r.URL.Path, "/users/shared@example.net/outlook/masterCategories/")
			name := strings.TrimPrefix(id, "category-")
			switch r.Method {
			case http.MethodPatch:
				var body struct {
					DisplayName string `json:"displayName"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				for i, existing := range masterCategories {
					if existing == name {
						masterCategories[i] = body.DisplayName
					}
				}
				for i, existing := range categories {
					if existing == name {
						categories[i] = body.DisplayName
					}
				}
				message["categories"] = categories
				json.NewEncoder(w).Encode(map[string]any{"id": "category-" + body.DisplayName, "displayName": body.DisplayName, "color": "preset0"})
			case http.MethodDelete:
				keptCategories := masterCategories[:0]
				for _, existing := range masterCategories {
					if existing != name {
						keptCategories = append(keptCategories, existing)
					}
				}
				masterCategories = keptCategories
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(405)
			}
		case strings.HasSuffix(r.URL.Path, "/messages/StableID/move"):
			var body struct{ DestinationID string }
			json.NewDecoder(r.Body).Decode(&body)
			folder = body.DestinationID
			message["parentFolderId"] = folder
			json.NewEncoder(w).Encode(message)
		case strings.HasSuffix(r.URL.Path, "/messages/StableID"):
			if r.Method == "PATCH" {
				var body map[string]json.RawMessage
				json.NewDecoder(r.Body).Decode(&body)
				if b := body["isRead"]; b != nil {
					json.Unmarshal(b, &read)
				}
				if b := body["categories"]; b != nil {
					json.Unmarshal(b, &categories)
				}
				message["isRead"] = read
				message["categories"] = categories
			}
			json.NewEncoder(w).Encode(message)
		case strings.HasSuffix(r.URL.Path, "/messages/delta"):
			if expired && strings.Contains(r.URL.Path, "/inbox-id/") {
				expired = false
				w.WriteHeader(410)
				return
			}
			value := []any{}
			if strings.Contains(r.URL.Path, "/"+folder+"/") {
				value = append(value, message)
			}
			json.NewEncoder(w).Encode(map[string]any{"value": value, "@odata.deltaLink": fixture.URL + r.URL.Path + "?cursor=next"})
		case strings.HasSuffix(r.URL.Path, "/mailFolders"):
			w.Write([]byte(`{"value":[{"id":"inbox-id","displayName":"Posteingang"},{"id":"archive-id","displayName":"Archiv"}]}`))
		case strings.HasSuffix(r.URL.Path, "/mailFolders/inbox"):
			w.Write([]byte(`{"id":"inbox-id","displayName":"Posteingang"}`))
		case strings.HasSuffix(r.URL.Path, "/mailFolders/archive"):
			w.Write([]byte(`{"id":"archive-id","displayName":"Archiv"}`))
		case strings.HasSuffix(r.URL.Path, "/messages"):
			value := []any{}
			// Graph lists a folder's messages when the request is folder
			// scoped, and the whole mailbox when it is not.
			if strings.Contains(r.URL.Path, "/"+folder+"/") || !strings.Contains(r.URL.Path, "/mailFolders/") {
				value = append(value, message)
			}
			json.NewEncoder(w).Encode(map[string]any{"value": value})
		default:
			w.WriteHeader(404)
		}
	}))
	defer fixture.Close()
	rm := app.NewRemoteMailboxService(svc)
	defer rm.Stop()
	rm.Microsoft = microsoft.NewClient(fixture.Client())
	rm.Microsoft.BaseURL = fixture.URL
	c := store.MicrosoftConnection{AccountID: u.AccountID, InboxID: box.ID, ClientID: "client", Tenant: "common", IdentityID: "identity", Mailbox: "shared@example.net", Shared: true}
	if e = rm.SaveMicrosoftGrant(ctx, c, microsoft.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600, Scope: microsoft.Scope(true)}); e != nil {
		t.Fatal(e)
	}
	boxes, e := svc.Store.ListInboxes(ctx, model.Principal{AccountID: u.AccountID, Admin: true})
	if e != nil {
		t.Fatal(e)
	}
	ready := false
	for _, b := range boxes {
		if b.ID == box.ID {
			ready = b.Capabilities != nil && b.Capabilities.Outbound && !b.Capabilities.Realtime
		}
	}
	if !ready {
		t.Fatal("Microsoft inbox missing sending capability in dashboard list")
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	page, e := rm.ListRemoteMessages(ctx, p, box.ID, "inbox-id", 50, "")
	if e != nil || len(page.Items) != 1 {
		t.Fatalf("list: %#v %v", page, e)
	}
	id := page.Items[0].ID
	arrivals, e := svc.Store.ListRemoteArrivalsForInbox(ctx, u.AccountID, box.ID, 100)
	if e != nil || len(arrivals) != 0 {
		t.Fatalf("backfill emitted arrivals: %#v %v", arrivals, e)
	}
	if _, e = rm.GetRemoteMessage(ctx, model.Principal{AccountID: u.AccountID}, box.ID, id); e == nil {
		t.Fatal("unscoped read accepted")
	}
	reader := model.Principal{AccountID: u.AccountID, MailboxRoles: map[string]string{box.ID: "read"}}
	if _, e = rm.SetRemoteRead(ctx, reader, box.ID, id, true); e == nil {
		t.Fatal("reader mutation accepted")
	}
	v, e := rm.SetRemoteRead(ctx, p, box.ID, id, true)
	if e != nil || !v.Read {
		t.Fatalf("read %#v %v", v, e)
	}
	// Two native categories at once: they are independent additive labels.
	v, e = rm.AddRemoteLabels(ctx, p, box.ID, id, []string{"Projects", "Clients"})
	if e != nil || len(v.Labels) != 2 {
		t.Fatalf("categories %#v %v", v, e)
	}
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	key := adminKey(t, svc, u)
	catalog := apiGet(t, srv.Handler(), "/v1/inboxes/"+box.ID+"/label-catalog", key)
	if catalog.Code != http.StatusOK {
		t.Fatalf("Graph categories should be additive labels: %d %s", catalog.Code, catalog.Body.String())
	}
	// The catalogue holds the additive categories only: an Outlook folder is a
	// location, not a label.
	var labels struct {
		Items []struct {
			ID, Name, Kind, Membership, Role string
		} `json:"items"`
	}
	if err := json.Unmarshal(catalog.Body.Bytes(), &labels); err != nil {
		t.Fatal(err)
	}
	if len(labels.Items) != 2 {
		t.Fatalf("Outlook catalogue: %+v", labels.Items)
	}
	seenCategories := map[string]bool{}
	for _, item := range labels.Items {
		if item.Kind != "tag" || item.Membership != "multiple" || item.Role != "" {
			t.Fatalf("Outlook catalogue entry: %+v", item)
		}
		seenCategories[item.ID] = true
	}
	for _, want := range []string{"tag:Projects", "tag:Clients"} {
		if !seenCategories[want] {
			t.Fatalf("Outlook catalogue misses %s: %+v", want, labels.Items)
		}
	}
	// A category can be created, renamed and deleted.
	var capabilities struct {
		Capabilities struct {
			CanCreate        bool   `json:"can_create"`
			CreateKind       string `json:"create_kind"`
			CreateMembership string `json:"create_membership"`
			CanRename        bool   `json:"can_rename"`
			CanDelete        bool   `json:"can_delete"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(catalog.Body.Bytes(), &capabilities); err != nil {
		t.Fatal(err)
	}
	if !capabilities.Capabilities.CanCreate || capabilities.Capabilities.CreateKind != "tag" ||
		capabilities.Capabilities.CreateMembership != "multiple" {
		t.Fatalf("Outlook capabilities: %+v", capabilities.Capabilities)
	}
	if !capabilities.Capabilities.CanRename || !capabilities.Capabilities.CanDelete {
		t.Fatalf("Outlook categories are renamable and deletable on Graph: %+v", capabilities.Capabilities)
	}
	// The system folders are discovered through the folder endpoint.
	folders := apiGet(t, srv.Handler(), "/v1/inboxes/"+box.ID+"/folders", key)
	var folderPage struct {
		Items []struct {
			ID   string `json:"id"`
			Path string `json:"path"`
		} `json:"items"`
	}
	if folders.Code != http.StatusOK || json.Unmarshal(folders.Body.Bytes(), &folderPage) != nil {
		t.Fatalf("Outlook folders: %d %s", folders.Code, folders.Body.String())
	}
	inboxFolder := ""
	for _, f := range folderPage.Items {
		if f.Path == "inbox-id" {
			inboxFolder = f.ID
		}
	}
	if inboxFolder == "" {
		t.Fatalf("missing Outlook Inbox folder: %s", folders.Body.String())
	}
	assertThreadUnreadRoutes(t, srv.Handler(), key, box.ID, page.Items[0].ThreadKey, 1, 0)
	for _, path := range []string{
		"/v1/inboxes/" + box.ID + "/threads?label_id=tag:Projects",
		"/v1/threads?inbox=" + box.ID + "&label_id=tag:Projects",
		"/v1/threads?label_id=tag:Projects",
	} {
		res := apiGet(t, srv.Handler(), path, key)
		var page struct {
			Items []threadUnreadResponse `json:"items"`
		}
		if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &page) != nil || len(page.Items) != 1 {
			t.Fatalf("Outlook filtered thread: %d %s", res.Code, res.Body.String())
		}
		assertThreadUnread(t, page.Items[0], 1, 0)
	}
	// The message carries its category as an additive label and its Outlook
	// folder as its location.
	for _, path := range []string{
		"/v1/inboxes/" + box.ID + "/messages?", "/v1/messages?inbox=" + box.ID + "&",
		"/v1/inboxes/" + box.ID + "/search?", "/v1/search?inbox=" + box.ID + "&",
	} {
		w := apiGet(t, srv.Handler(), path+"label_id="+url.QueryEscape("tag:Projects"), key)
		var page struct {
			Items []struct {
				ID       string
				LabelIDs []string `json:"label_ids"`
				FolderID *string  `json:"folder_id"`
			} `json:"items"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].ID != id {
			t.Fatalf("Outlook label filter %s: %d %s", path, w.Code, w.Body.String())
		}
		if len(page.Items[0].LabelIDs) != 2 || !stringSliceContains(page.Items[0].LabelIDs, "tag:Projects") || !stringSliceContains(page.Items[0].LabelIDs, "tag:Clients") {
			t.Fatalf("Outlook label_ids: %v", page.Items[0].LabelIDs)
		}
		if page.Items[0].FolderID == nil || *page.Items[0].FolderID != inboxFolder {
			t.Fatalf("Outlook folder_id = %v, want %s", page.Items[0].FolderID, inboxFolder)
		}
	}
	// A folder is not a label: it is refused, and it never files the message.
	applyLabel := func(labelID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/inboxes/"+box.ID+"/messages/"+id+"/labels?wait=true", strings.NewReader(`{"label_id":"`+labelID+`","operation":"add"}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}
	if got := applyLabel("folder:" + inboxFolder); got.Code == http.StatusOK {
		t.Fatalf("an Outlook folder must not be applicable as a label: %s", got.Body.String())
	}
	// A native category is created through the catalogue.
	created := httptest.NewRequest(http.MethodPost, "/v1/inboxes/"+box.ID+"/label-catalog", strings.NewReader(`{"name":"NewCategory"}`))
	created.Header.Set("Authorization", "Bearer "+key)
	created.Header.Set("Content-Type", "application/json")
	createdRR := httptest.NewRecorder()
	srv.Handler().ServeHTTP(createdRR, created)
	if createdRR.Code != http.StatusCreated {
		t.Fatalf("create Outlook category: %d %s", createdRR.Code, createdRR.Body.String())
	}
	var createdLabel struct {
		ID         string `json:"id"`
		Kind       string `json:"kind"`
		Membership string `json:"membership"`
	}
	if err := json.Unmarshal(createdRR.Body.Bytes(), &createdLabel); err != nil {
		t.Fatal(err)
	}
	if createdLabel.ID != "tag:NewCategory" || createdLabel.Kind != "tag" || createdLabel.Membership != "multiple" {
		t.Fatalf("created Outlook category: %+v", createdLabel)
	}
	after := apiGet(t, srv.Handler(), "/v1/inboxes/"+box.ID+"/label-catalog", key)
	if after.Code != http.StatusOK || !strings.Contains(after.Body.String(), `"id":"tag:NewCategory"`) {
		t.Fatalf("created Outlook category missing from the catalogue: %d %s", after.Code, after.Body.String())
	}
	// It is a native Outlook master category, created on the provider.
	graphRequest, gerr := http.NewRequest(http.MethodGet, fixture.URL+"/users/shared@example.net/outlook/masterCategories", nil)
	if gerr != nil {
		t.Fatal(gerr)
	}
	graphRequest.Header.Set("Authorization", "Bearer access")
	graphRequest.Header.Set("Prefer", `IdType="ImmutableId"`)
	graphResponse, gerr := fixture.Client().Do(graphRequest)
	if gerr != nil {
		t.Fatal(gerr)
	}
	defer graphResponse.Body.Close()
	var graphCategories struct {
		Value []struct {
			DisplayName string `json:"displayName"`
		} `json:"value"`
	}
	if gerr := json.NewDecoder(graphResponse.Body).Decode(&graphCategories); gerr != nil {
		t.Fatal(gerr)
	}
	createdNatively := false
	for _, category := range graphCategories.Value {
		createdNatively = createdNatively || category.DisplayName == "NewCategory"
	}
	if !createdNatively {
		t.Fatalf("the category was not created natively in Outlook: %+v", graphCategories.Value)
	}
	// A category is renamed on the provider and the catalogue follows it.
	renamed := apiDo(srv.Handler(), "PATCH", "/v1/inboxes/"+box.ID+"/label-catalog/"+url.PathEscape("tag:NewCategory"), key, `{"name":"RenamedCategory"}`)
	if renamed.Code != http.StatusNoContent {
		t.Fatalf("rename Outlook category = %d %s", renamed.Code, renamed.Body.String())
	}
	catalogAfterRename := apiGet(t, srv.Handler(), "/v1/inboxes/"+box.ID+"/label-catalog", key)
	if !strings.Contains(catalogAfterRename.Body.String(), `"id":"tag:RenamedCategory"`) ||
		strings.Contains(catalogAfterRename.Body.String(), `"id":"tag:NewCategory"`) {
		t.Fatalf("catalogue did not follow the rename: %s", catalogAfterRename.Body.String())
	}
	// An unknown category cannot be renamed: Graph is the provider's truth.
	if missing := apiDo(srv.Handler(), "PATCH", "/v1/inboxes/"+box.ID+"/label-catalog/"+url.PathEscape("tag:NoSuchCategory"), key, `{"name":"Whatever"}`); missing.Code == http.StatusNoContent {
		t.Fatalf("renamed a category Outlook does not have: %s", missing.Body.String())
	}
	// It is deleted on the provider, which also clears it from every message.
	deleted := apiDo(srv.Handler(), "DELETE", "/v1/inboxes/"+box.ID+"/label-catalog/"+url.PathEscape("tag:RenamedCategory"), key, "")
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete Outlook category = %d %s", deleted.Code, deleted.Body.String())
	}
	catalogAfterDelete := apiGet(t, srv.Handler(), "/v1/inboxes/"+box.ID+"/label-catalog", key)
	if strings.Contains(catalogAfterDelete.Body.String(), "RenamedCategory") {
		t.Fatalf("deleted category is still in the catalogue: %s", catalogAfterDelete.Body.String())
	}
	// An invalid category name is refused.
	invalid := httptest.NewRequest(http.MethodPost, "/v1/inboxes/"+box.ID+"/label-catalog", strings.NewReader(`{"name":""}`))
	invalid.Header.Set("Authorization", "Bearer "+key)
	invalid.Header.Set("Content-Type", "application/json")
	invalidRR := httptest.NewRecorder()
	srv.Handler().ServeHTTP(invalidRR, invalid)
	if invalidRR.Code != http.StatusBadRequest {
		t.Fatalf("invalid Outlook category = %d, want 400: %s", invalidRR.Code, invalidRR.Body.String())
	}
	v, e = rm.MoveRemoteMessage(ctx, p, box.ID, id, "archive-id")
	if e != nil || v.ID != id || v.FolderPath != "archive-id" {
		t.Fatalf("move %#v %v", v, e)
	}
	// A folder move keeps every category: filing a message never drops a label.
	if len(v.Labels) != 2 || !stringSliceContains(v.Labels, "Projects") || !stringSliceContains(v.Labels, "Clients") {
		t.Fatalf("a move must keep the categories: %#v", v.Labels)
	}
	page, e = rm.ListRemoteMessages(ctx, p, box.ID, "inbox-id", 50, "")
	if e != nil || len(page.Items) != 0 {
		t.Fatalf("stale inbox %#v %v", page, e)
	}
	c, e = svc.Store.MicrosoftConnection(ctx, u.AccountID, box.ID)
	if e != nil || strings.Contains(c.EncryptedToken, "refresh") {
		t.Fatalf("credentials %v", e)
	}
	c.Mailbox = "another@example.net"
	if e = rm.SaveMicrosoftGrant(ctx, c, microsoft.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600, Scope: microsoft.Scope(true)}); e == nil {
		t.Fatal("rebind accepted")
	}
	provider = "NewArrival"
	folder = "inbox-id"
	for n := 0; n < 2; n++ {
		if _, e = rm.ReconcileRemote(ctx, u.AccountID, box.ID); e != nil {
			t.Fatal(e)
		}
	}
	arrivals, e = svc.Store.ListRemoteArrivalsForInbox(ctx, u.AccountID, box.ID, 100)
	if e != nil || len(arrivals) != 1 {
		t.Fatalf("deduplication %#v %v", arrivals, e)
	}
	r, e := svc.Store.RemoteMessageForArrival(ctx, u.AccountID, arrivals[0].ID)
	if e != nil || r.Subject != "Hello" {
		t.Fatalf("arrival resolution %#v %v", r, e)
	}
	expired = true
	provider = "AfterRecovery"
	for n := 0; n < 3; n++ {
		if _, e = rm.ReconcileRemote(ctx, u.AccountID, box.ID); e != nil {
			t.Fatal(e)
		}
	}
	page, e = rm.ListRemoteMessages(ctx, p, box.ID, "inbox-id", 50, "")
	if e != nil || len(page.Items) != 1 {
		t.Fatalf("resync left stale metadata %#v %v", page, e)
	}
}

func TestMicrosoftOAuthPasteStateAndReplay(t *testing.T) {
	t.Parallel()
	svc, _, u, _, _, _ := standaloneFixture(t)
	var logs bytes.Buffer
	svc.Store.SetInboxLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	svc.Config.MicrosoftClientID = "client"
	svc.Config.MicrosoftTenantID = "common"
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/token") {
			r.ParseForm()
			if r.Form.Get("client_secret") != "" || r.Form.Get("code_verifier") == "" || r.Form.Get("redirect_uri") != microsoft.RedirectURI {
				t.Error("bad public-client exchange")
			}
			json.NewEncoder(w).Encode(microsoft.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600, Scope: microsoft.Scope(false)})
			return
		}
		if r.URL.Path == "/me" {
			w.Write([]byte(`{"id":"identity","mail":"personal@example.net","displayName":"Personal Owner"}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/mailFolders/inbox") {
			w.Write([]byte(`{"id":"inbox-id"}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer fixture.Close()
	rm := app.NewRemoteMailboxService(svc)
	defer rm.Stop()
	rm.Microsoft = microsoft.NewClient(fixture.Client())
	rm.Microsoft.BaseURL = fixture.URL
	rm.Microsoft.AuthBaseURL = fixture.URL
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	h := srv.Handler()
	session, csrf, e := svc.Store.CreateSession(context.Background(), u.ID, time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	post := func(path string, v url.Values) *httptest.ResponseRecorder {
		v.Set("_csrf", csrf)
		r := httptest.NewRequest("POST", path, strings.NewReader(v.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: "mmm_session", Value: session})
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr
	}
	rr := post("/ui/oauth/microsoft/begin", url.Values{"mailbox_type": {"personal"}})
	if rr.Code != 200 {
		t.Fatalf("begin %d %s", rr.Code, rr.Body)
	}
	var result map[string]string
	json.Unmarshal(rr.Body.Bytes(), &result)
	auth, _ := url.Parse(result["authorization_url"])
	if auth.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("missing PKCE")
	}
	state := auth.Query().Get("state")
	for _, callback := range []string{"http://evil.example/?state=" + state + "&code=secret", "http://localhost/?state=wrong&code=secret"} {
		rr = post("/ui/oauth/microsoft/finish", url.Values{"callback_url": {callback}})
		if rr.Code != 400 {
			t.Fatalf("invalid callback accepted %d", rr.Code)
		}
	}
	callback := "http://localhost/?state=" + url.QueryEscape(state) + "&code=secret"
	rr = post("/ui/oauth/microsoft/finish", url.Values{"callback_url": {callback}})
	if rr.Code != 200 {
		t.Fatalf("finish %d %s", rr.Code, rr.Body)
	}
	rr = post("/ui/oauth/microsoft/finish", url.Values{"callback_url": {callback}})
	if rr.Code != 400 {
		t.Fatal("callback replay accepted")
	}
	boxes, e := svc.Store.ListStandaloneInboxes(context.Background(), u.AccountID)
	if e != nil {
		t.Fatal(e)
	}
	if len(boxes) != 2 {
		t.Fatalf("personal inbox missing: %#v", boxes)
	}
	if boxes[1].DisplayName != "Personal Owner" || boxes[1].Address != "personal@example.net" {
		t.Fatalf("personal label not defaulted from the identity: %#v", boxes[1])
	}
	records := lifecycleRecords(t, &logs)
	if len(records) != 1 || records[0]["msg"] != "inbox.created" || records[0]["operation"] != "microsoft_connect" || records[0]["source"] != "ui" || records[0]["user_id"] != u.ID {
		t.Fatalf("Microsoft lifecycle attribution: %s", logs.String())
	}
	for _, private := range []string{"refresh", "secret", session, csrf, state} {
		if strings.Contains(logs.String(), private) {
			t.Fatalf("Microsoft credential leaked: %q", private)
		}
	}
}

// TestMicrosoftSharedMailboxSelection covers the two-phase shared flow: the
// sign-in returns before mailbox choice, accessible shared mailboxes are
// discovered from the directory, and the chosen address completes the inbox.
func TestMicrosoftSharedMailboxSelection(t *testing.T) {
	t.Parallel()
	svc, _, u, _, _, _ := standaloneFixture(t)
	var logs bytes.Buffer
	svc.Store.SetInboxLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	svc.Config.MicrosoftClientID = "client"
	svc.Config.MicrosoftTenantID = "common"
	var probeMu sync.Mutex
	probes := map[string]bool{}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			json.NewEncoder(w).Encode(microsoft.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600, Scope: microsoft.Scope(true)})
		case r.URL.Path == "/me":
			w.Write([]byte(`{"id":"identity","mail":"owner@example.net","displayName":"Owner"}`))
		case r.URL.Path == "/users":
			if got := r.URL.Query().Get("$select"); !strings.Contains(got, "displayName") {
				t.Errorf("directory select missing displayName: %s", got)
			}
			w.Write([]byte(`{"value":[{"id":"u2","displayName":"Team Box","mail":"shared@example.net"},{"id":"u3","displayName":"Other","mail":"other@example.net"}]}`))
		case strings.HasSuffix(r.URL.Path, "/mailFolders/inbox"):
			probeMu.Lock()
			probes[r.URL.Path] = true
			probeMu.Unlock()
			if r.URL.Path != "/users/shared@example.net/mailFolders/inbox" {
				w.WriteHeader(403)
				return
			}
			w.Write([]byte(`{"id":"inbox-id"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer fixture.Close()
	rm := app.NewRemoteMailboxService(svc)
	defer rm.Stop()
	rm.Microsoft = microsoft.NewClient(fixture.Client())
	rm.Microsoft.BaseURL = fixture.URL
	rm.Microsoft.AuthBaseURL = fixture.URL
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	h := srv.Handler()
	session, csrf, e := svc.Store.CreateSession(context.Background(), u.ID, time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	post := func(path string, v url.Values) *httptest.ResponseRecorder {
		v.Set("_csrf", csrf)
		r := httptest.NewRequest("POST", path, strings.NewReader(v.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: "mmm_session", Value: session})
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr
	}
	begin := post("/ui/oauth/microsoft/begin", url.Values{"mailbox_type": {"shared"}, "display": {"Shared Team"}})
	if begin.Code != 200 {
		t.Fatalf("begin %d %s", begin.Code, begin.Body)
	}
	var started map[string]string
	json.Unmarshal(begin.Body.Bytes(), &started)
	auth, _ := url.Parse(started["authorization_url"])
	if !strings.Contains(auth.Query().Get("scope"), "Mail.ReadWrite.Shared") || !strings.Contains(auth.Query().Get("scope"), "User.ReadBasic.All") {
		t.Fatalf("shared scopes missing: %s", auth.Query().Get("scope"))
	}
	callback := "http://localhost/?state=" + url.QueryEscape(auth.Query().Get("state")) + "&code=secret"
	phase1 := post("/ui/oauth/microsoft/finish", url.Values{"callback_url": {callback}})
	if phase1.Code != 200 {
		t.Fatalf("shared finish %d %s", phase1.Code, phase1.Body)
	}
	var choice struct {
		State     string `json:"selection_state"`
		Mailboxes []struct{ Address, Name string }
	}
	if json.Unmarshal(phase1.Body.Bytes(), &choice); choice.State == "" || len(choice.Mailboxes) != 1 || choice.Mailboxes[0].Address != "shared@example.net" || choice.Mailboxes[0].Name != "Team Box" {
		t.Fatalf("shared discovery %s", phase1.Body)
	}
	probeMu.Lock()
	otherProbed := probes["/users/other@example.net/mailFolders/inbox"]
	probeMu.Unlock()
	if !otherProbed {
		t.Fatal("inaccessible candidate was not probed")
	}
	if rr := post("/ui/oauth/microsoft/finish", url.Values{"state": {choice.State}, "mailbox": {"not-an-address"}}); rr.Code != 400 {
		t.Fatal("invalid shared address accepted")
	}
	if rr := post("/ui/oauth/microsoft/finish", url.Values{"state": {choice.State}, "mailbox": {"other@example.net"}}); rr.Code != 400 {
		t.Fatal("inaccessible shared mailbox accepted")
	}
	done := post("/ui/oauth/microsoft/finish", url.Values{"state": {choice.State}, "mailbox": {"shared@example.net"}})
	if done.Code != 200 {
		t.Fatalf("selection %d %s", done.Code, done.Body)
	}
	var finished map[string]string
	json.Unmarshal(done.Body.Bytes(), &finished)
	if finished["inbox_id"] == "" {
		t.Fatal("no inbox returned")
	}
	records := lifecycleRecords(t, &logs)
	if len(records) != 1 || records[0]["operation"] != "microsoft_connect" || records[0]["source"] != "ui" || records[0]["user_id"] != u.ID || records[0]["inbox_id"] != finished["inbox_id"] {
		t.Fatalf("shared Microsoft attribution: %s", logs.String())
	}
	if rr := post("/ui/oauth/microsoft/finish", url.Values{"state": {choice.State}, "mailbox": {"shared@example.net"}}); rr.Code != 400 {
		t.Fatal("selection replay accepted")
	}
	c, e := svc.Store.MicrosoftConnection(context.Background(), u.AccountID, finished["inbox_id"])
	if e != nil || !c.Shared || c.Mailbox != "shared@example.net" {
		t.Fatalf("shared connection %#v %v", c, e)
	}
	box, e := svc.Store.GetInbox(context.Background(), model.Principal{AccountID: u.AccountID, Admin: true}, finished["inbox_id"])
	if e != nil || box.DisplayName != "Shared Team" {
		t.Fatalf("typed label not kept: %#v %v", box, e)
	}
}
