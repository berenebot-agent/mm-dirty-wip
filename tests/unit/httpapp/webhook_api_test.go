package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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
	if !strings.Contains(body, "Webhook delivery") || !strings.Contains(body, "Hooks") {
		t.Fatal("dashboard does not render the webhook client")
	}
}
