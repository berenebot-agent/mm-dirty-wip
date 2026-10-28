package gmail_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dellarb/mammothmail/internal/transport/gmail"
)

func TestIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
		fail             bool
	}{
		{"name", `{"name":"Google Person"}`, "Google Person", 200, false},
		{"missing", `{}`, "", 200, false},
		{"denied", `{}`, "", 403, true},
		{"unavailable", `{}`, "", 503, true},
		{"invalid", `broken`, "", 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer access" {
					t.Errorf("unexpected identity request: %s %v", r.Method, r.Header)
				}
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := gmail.NewClient(srv.Client())
			c.UserInfoURL = srv.URL
			info, err := c.Identity(context.Background(), "access")
			if (err != nil) != tc.fail || info.Name != tc.want {
				t.Fatalf("identity %#v, error %v", info, err)
			}
		})
	}
}
