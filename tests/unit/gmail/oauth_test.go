package gmail_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/transport/gmail"
)

func TestExchange(t *testing.T) {
	var gotPath string
	var gotForm url.Values
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"at-1","refresh_token":"rt-1","expires_in":3600,"scope":"https://mail.google.com/","token_type":"Bearer"}`)
	}))

	tok, err := c.Exchange(context.Background(), "cid", "csecret", "https://app.example/cb", "the-code", "pkce-verifier")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if gotPath != "/token" {
		t.Fatalf("path = %q", gotPath)
	}
	want := map[string]string{
		"grant_type":    "authorization_code",
		"code":          "the-code",
		"client_id":     "cid",
		"client_secret": "csecret",
		"redirect_uri":  "https://app.example/cb",
		"code_verifier": "pkce-verifier",
	}
	for k, v := range want {
		if gotForm.Get(k) != v {
			t.Errorf("form[%s] = %q want %q", k, gotForm.Get(k), v)
		}
	}
	if tok.AccessToken != "at-1" || tok.RefreshToken != "rt-1" || tok.ExpiresIn != 3600 || tok.TokenType != "Bearer" || tok.Scope != "https://mail.google.com/" {
		t.Fatalf("token = %#v", tok)
	}
}

func TestRefresh(t *testing.T) {
	var gotForm url.Values
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"at-2","expires_in":1800,"token_type":"Bearer"}`)
	}))

	tok, err := c.Refresh(context.Background(), "cid", "csecret", "rt-1")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if gotForm.Get("grant_type") != "refresh_token" || gotForm.Get("refresh_token") != "rt-1" || gotForm.Get("client_id") != "cid" {
		t.Fatalf("form = %v", gotForm)
	}
	if tok.AccessToken != "at-2" || tok.RefreshToken != "" {
		t.Fatalf("token = %#v", tok)
	}
}

func TestExchangeInvalidGrant(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_grant","error_description":"token expired at https://secret.example/xyz"}`)
	}))

	_, err := c.Exchange(context.Background(), "cid", "csecret", "https://app.example/cb", "bad-code", "")
	var mb *model.MailboxError
	if !errors.As(err, &mb) {
		t.Fatalf("err = %v, want *model.MailboxError", err)
	}
	if mb.Kind != model.ErrKindAuth {
		t.Fatalf("kind = %q want %q", mb.Kind, model.ErrKindAuth)
	}
	if mb.Message == "" || errors.Is(err, nil) {
		t.Fatalf("unsafe message %q", mb.Message)
	}
	if contains(err.Error(), "secret.example") || contains(err.Error(), "invalid_grant") {
		t.Fatalf("error leaked provider detail: %q", err.Error())
	}
}

func TestExchangeRequiresInputs(t *testing.T) {
	c := gmail.NewClient(nil)
	_, err := c.Exchange(context.Background(), "", "s", "r", "c", "")
	assertKind(t, err, model.ErrKindInvalid)
	_, err = c.Refresh(context.Background(), "cid", "s", "")
	assertKind(t, err, model.ErrKindInvalid)
}
