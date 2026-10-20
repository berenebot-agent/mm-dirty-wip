package gmail_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/transport/gmail"
)

// apiPrefix is the per-user Gmail REST path prefix the client appends.
const apiPrefix = "/gmail/v1/users/me"

// testClient builds a Client whose API and token endpoints point at a local
// fixture server. srv.Close and cleanup are registered on t.
func testClient(t *testing.T, h http.Handler) *gmail.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := gmail.NewClient(srv.Client())
	c.BaseURL = srv.URL
	c.TokenURL = srv.URL + "/token"
	return c
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

// assertKind asserts err is a *model.MailboxError of the given kind.
func assertKind(t *testing.T, err error, kind string) {
	t.Helper()
	var mb *model.MailboxError
	if !errors.As(err, &mb) {
		t.Fatalf("err = %v, want *model.MailboxError", err)
	}
	if mb.Kind != kind {
		t.Fatalf("kind = %q want %q (err %v)", mb.Kind, kind, err)
	}
}
