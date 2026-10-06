package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

func TestWebhookClientAPI(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	_, key, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	// Invalid destinations and modes are rejected before persistence.
	for _, bad := range []string{
		`{"inbox_id":"` + box.ID + `","url":"http://insecure.test/h","mode":"notify","auth":"signature"}`,
		`{"inbox_id":"` + box.ID + `","url":"https://ok.test/h","mode":"nope","auth":"signature"}`,
		`{"inbox_id":"` + box.ID + `","url":"https://ok.test/h","mode":"notify","auth":"nope"}`,
	} {
		if rr := call("POST", "/v1/admin/clients/webhooks", bad); rr.Code != http.StatusBadRequest {
			t.Fatalf("bad config accepted: %d %s", rr.Code, rr.Body.String())
		}
	}

	rr := call("POST", "/v1/admin/clients/webhooks", `{"inbox_id":"`+box.ID+`","name":"ep","url":"https://ok.test/h","mode":"notify","auth":"signature"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create %d %s", rr.Code, rr.Body.String())
	}
	var created struct {
		Client struct {
			ID        string `json:"ID"`
			Secret    string `json:"secret"`
			InboxID   string `json:"InboxID"`
			AuthMode  string `json:"AuthMode"`
			Mode      string `json:"Mode"`
			Encrypted string `json:"SecretEncrypted"`
		} `json:"client"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode %v", err)
	}
	if created.Secret == "" || created.Client.ID == "" {
		t.Fatalf("created %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "SecretEncrypted") {
		t.Fatal("encrypted secret leaked in response")
	}

	list := call("GET", "/v1/admin/clients/webhooks", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), created.Client.ID) {
		t.Fatalf("list %d %s", list.Code, list.Body.String())
	}
	if strings.Contains(list.Body.String(), "SecretEncrypted") || strings.Contains(list.Body.String(), created.Secret) {
		t.Fatal("list leaked a secret")
	}

	clients := call("GET", "/v1/admin/clients", "")
	if clients.Code != http.StatusOK || !strings.Contains(clients.Body.String(), `"webhooks"`) {
		t.Fatalf("clients %d %s", clients.Code, clients.Body.String())
	}

	if rr := call("POST", "/v1/admin/clients/webhooks/"+created.Client.ID+"/enabled", `{"enabled":false}`); rr.Code != http.StatusOK {
		t.Fatalf("pause %d %s", rr.Code, rr.Body.String())
	}
	if rr := call("POST", "/v1/admin/clients/webhooks/"+created.Client.ID+"/rotate", ""); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "secret") {
		t.Fatalf("rotate %d %s", rr.Code, rr.Body.String())
	}
	if rr := call("DELETE", "/v1/admin/clients/webhooks/"+created.Client.ID, ""); rr.Code != http.StatusNoContent {
		t.Fatalf("delete %d %s", rr.Code, rr.Body.String())
	}
}

// TestClientDeliveryLogPage proves an inbox connector is surfaced on the
// dashboard and its delivery-log route renders the outcome. Webhook and Hermes
// connectors expose logs while API-key clients do not.
func TestClientDeliveryLogPage(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	cookie, _ := uiSession(t, svc, u.ID)
	enc, _ := svc.EncryptSecret([]byte("s"))
	cl, err := svc.Store.CreateWebhookClient(ctx, u.AccountID, box.ID, "Hooks", "https://hooks.example.test/x", "notify", "signature", enc)
	if err != nil {
		t.Fatal(err)
	}
	_, ev, _, err := svc.Store.CommitInbound(ctx, store.InboundRecord{Inbox: box, Provider: "mailgun", ProviderDeliveryID: "log-ui-1", RFCMessageID: "<logui@test>", From: model.Address{Address: "alice@outside.test"}, To: []string{box.Address}, EnvelopeTo: []string{box.Address}, Subject: "Logged UI", Text: "hi", RawPath: "messages/logui.eml", SizeBytes: 4, ReceivedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.RecordWebhookDelivery(ctx, cl.ID, ev.ID, true, "", time.Now().UTC(), time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	dash := httptest.NewRequest("GET", "/", nil)
	dash.AddCookie(cookie)
	dr := httptest.NewRecorder()
	h.ServeHTTP(dr, dash)
	if dr.Code != http.StatusOK || !strings.Contains(dr.Body.String(), `data-connector="`+cl.ID+`"`) {
		t.Fatalf("dashboard missing connector: %d", dr.Code)
	}

	req := httptest.NewRequest("GET", "/ui/clients/"+cl.ID+"/log", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("log page %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"Hooks · Log", "Delivered", "message.received", "Logged UI"} {
		if !strings.Contains(body, want) {
			t.Fatalf("log page missing %q", want)
		}
	}

	// An API key client has no delivery log.
	if _, _, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "k", true, nil); err != nil {
		t.Fatal(err)
	}
	apiKeys, _ := svc.Store.ListAPIKeys(ctx, u.AccountID)
	req = httptest.NewRequest("GET", "/ui/clients/"+apiKeys[0].ID+"/log", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("api key log page = %d, want 404", rr.Code)
	}
}

func TestWebhookCreateViaDashboard(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	form := url.Values{
		"type":  {"webhook"},
		"name":  {"Hooks"},
		"inbox": {box.ID},
		"url":   {"https://ok.test/hook"},
		"mode":  {"notify"},
		"auth":  {"signature"},
		"_csrf": {csrf},
		// Inbox-wide delivery actions belong to inbox settings and must be
		// ignored even if a stale client includes them during connector setup.
		"auto_mark_read_on_delivery":      {"1"},
		"auto_trash_after_delivery":       {"1"},
		"auto_trash_after_delivery_hours": {"6"},
		"delivery_trigger":                {"any"},
	}
	req := httptest.NewRequest("POST", "/ui/keys", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create webhook = %d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if got["notice"] != "Webhook created" || got["secret"] == "" {
		t.Fatalf("unexpected response %#v", got)
	}

	dash := httptest.NewRequest("GET", "/", nil)
	dash.AddCookie(cookie)
	dr := httptest.NewRecorder()
	h.ServeHTTP(dr, dash)
	if dr.Code != http.StatusOK {
		t.Fatalf("dashboard %d", dr.Code)
	}
	body := dr.Body.String()
	clients, err := svc.Store.ListWebhookClients(context.Background(), u.AccountID)
	if err != nil || len(clients) != 1 {
		t.Fatalf("webhook clients: %+v %v", clients, err)
	}
	createdInbox, err := svc.Store.GetInboxInternal(context.Background(), u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if createdInbox.AutoMarkReadOnDelivery || createdInbox.AutoTrashAfterDeliveryHours != nil || createdInbox.DeliveryTrigger != "all" {
		t.Fatalf("connector creation changed inbox-level delivery actions: %+v", createdInbox)
	}
	if !strings.Contains(body, `data-connector="`+clients[0].ID+`"`) || !strings.Contains(body, "Hooks") {
		t.Fatal("dashboard does not render the webhook connector on its inbox")
	}
	if strings.Contains(body, `data-kind="webhook" data-name="Hooks"`) {
		t.Fatal("webhook connector should not render as a row in Clients")
	}
}

func TestWebhookSuppliedBearerSecret(t *testing.T) {
	for _, viaUI := range []bool{false, true} {
		name := "API"
		if viaUI {
			name = "dashboard"
		}
		t.Run(name, func(t *testing.T) {
			svc, h, u, _, box := httpFixture(t)
			ctx := context.Background()
			_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
			if err != nil {
				t.Fatal(err)
			}
			cookie, csrf := uiSession(t, svc, u.ID)
			call := func(id, token, authMode string) *httptest.ResponseRecorder {
				method, path := "POST", "/v1/admin/clients/webhooks"
				if id != "" {
					method, path = "PUT", path+"/"+id
				}
				body, err := json.Marshal(map[string]string{"inbox_id": box.ID, "name": "supplied", "url": "https://ok.test/hook", "mode": "notify", "auth": authMode, "bearer_secret": token})
				if err != nil {
					t.Fatal(err)
				}
				contentType := "application/json"
				if viaUI {
					method, path = "POST", "/ui/keys"
					if id != "" {
						path = "/ui/webhooks/" + id + "/edit"
					}
					form := url.Values{"type": {"webhook"}, "inbox": {box.ID}, "name": {"supplied"}, "url": {"https://ok.test/hook"}, "mode": {"notify"}, "auth": {authMode}, "bearer_secret": {token}, "_csrf": {csrf}}
					body = []byte(form.Encode())
					contentType = "application/x-www-form-urlencoded"
				}
				req := httptest.NewRequest(method, path, strings.NewReader(string(body)))
				req.Header.Set("Content-Type", contentType)
				req.Header.Set("Accept", "application/json")
				if viaUI {
					req.AddCookie(cookie)
				} else {
					req.Header.Set("Authorization", "Bearer "+key)
				}
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, req)
				return rr
			}
			const supplied = "other.app-token_123+/=="
			for _, bad := range []string{"Bearer token", "token\r\nInjected: x", " ", "=", "token=tail"} {
				if rr := call("", bad, "bearer"); rr.Code != http.StatusBadRequest {
					t.Fatalf("invalid token response: %d %s", rr.Code, rr.Body.String())
				}
			}
			if rr := call("", supplied, "signature"); rr.Code != http.StatusBadRequest {
				t.Fatalf("signature accepted supplied bearer token: %d", rr.Code)
			}
			if rr := call("", supplied, "bearer"); rr.Code != http.StatusCreated {
				t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
			}
			clients, err := svc.Store.ListWebhookClients(ctx, u.AccountID)
			if err != nil || len(clients) != 1 {
				t.Fatalf("clients: %+v %v", clients, err)
			}
			id := clients[0].ID
			assertSecret := func(want string) {
				t.Helper()
				client, err := svc.Store.GetWebhookClient(ctx, u.AccountID, id)
				if err != nil {
					t.Fatal(err)
				}
				plain, err := svc.DecryptSecret(client.SecretEncrypted)
				if err != nil || string(plain) != want || client.SecretEncrypted == want {
					t.Fatalf("stored secret not encrypted/preserved: %v", err)
				}
			}
			assertSecret(supplied)
			for _, token := range []string{"", "replacement-token"} {
				rr := call(id, token, "bearer")
				wantCode := http.StatusOK
				if viaUI {
					wantCode = http.StatusSeeOther
				}
				if rr.Code != wantCode {
					t.Fatalf("update: %d %s", rr.Code, rr.Body.String())
				}
				want := token
				if want == "" {
					want = supplied
				}
				assertSecret(want)
			}
			if rr := call(id, "bad token", "bearer"); rr.Code != http.StatusBadRequest {
				t.Fatalf("invalid replacement accepted: %d", rr.Code)
			}
			assertSecret("replacement-token")
			for _, path := range []string{"/", "/v1/admin/clients/webhooks", "/v1/admin/clients"} {
				req := httptest.NewRequest("GET", path, nil)
				req.AddCookie(cookie)
				if path != "/" {
					req.Header.Set("Authorization", "Bearer "+key)
				}
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, req)
				if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), supplied) || strings.Contains(rr.Body.String(), "replacement-token") {
					t.Fatalf("secret-free view %s: %d", path, rr.Code)
				}
			}
			if rr := call("", "", "bearer"); rr.Code != http.StatusCreated {
				t.Fatalf("generate default: %d %s", rr.Code, rr.Body.String())
			}
			clients, err = svc.Store.ListWebhookClients(ctx, u.AccountID)
			if err != nil || len(clients) != 2 {
				t.Fatalf("generated clients: %v", err)
			}
			for _, client := range clients {
				if client.ID == id {
					continue
				}
				plain, err := svc.DecryptSecret(client.SecretEncrypted)
				if err != nil || len(plain) != 43 {
					t.Fatalf("generated token: length %d, error %v", len(plain), err)
				}
			}
		})
	}
}
