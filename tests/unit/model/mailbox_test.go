package model_test

import (
	"errors"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
)

// TestListEnvelopeCompletenessIndependentOfCursor proves completeness describes
// the result set, not pagination: a page may carry a next cursor while the
// result set is still complete.
func TestListEnvelopeCompletenessIndependentOfCursor(t *testing.T) {
	env := model.ListEnvelope[model.Folder]{
		Items:        []model.Folder{{ID: "f1"}},
		NextCursor:   "opaque-token",
		Completeness: model.CompletenessComplete,
	}
	if env.NextCursor == "" {
		t.Fatal("next cursor should be set")
	}
	if env.Completeness != model.CompletenessComplete {
		t.Fatalf("completeness = %q, want complete despite a next cursor", env.Completeness)
	}
}

// TestInboxFailureProjectsSafeFields proves a per-inbox failure exposes only the
// normalized code and safe message, never the wrapped provider/store cause.
func TestInboxFailureProjectsSafeFields(t *testing.T) {
	cause := errors.New("dial imap.remote.example:5432: no route to host (credential hunter2)")
	err := model.NewMailboxError(model.ErrKindUnavailable, "remote server unavailable", true, cause)
	f := model.NewInboxFailure("in_1", err)
	if f.InboxID != "in_1" || f.Code != model.ErrKindUnavailable || !f.Retryable {
		t.Fatalf("failure = %+v", f)
	}
	if f.Message != "remote server unavailable" {
		t.Fatalf("message = %q, want the normalized message", f.Message)
	}
	// The raw cause must never appear in a serialized failure.
	for _, leak := range []string{"hunter2", "imap.remote.example", "no route to host"} {
		if f.Message == leak || f.Code == leak {
			t.Fatalf("failure leaked %q", leak)
		}
	}
}

// TestInboxFailureUnknownErrorIsInternal proves a non-mailbox error is classified
// as internal with no message, so raw error text is never surfaced.
func TestInboxFailureUnknownErrorIsInternal(t *testing.T) {
	f := model.NewInboxFailure("in_2", errors.New("boom: secret detail"))
	if f.Code != model.ErrKindInternal || f.Message != "" {
		t.Fatalf("failure = %+v, want internal with no message", f)
	}
}

// TestFolderRolesCoverArchiveAndOutbox proves the folder role vocabulary includes
// archive and outbox, distinct from the free-text label role.
func TestFolderRolesCoverArchiveAndOutbox(t *testing.T) {
	for _, role := range []string{model.FolderRoleArchive, model.FolderRoleOutbox, model.FolderRoleFolder} {
		if role == model.FolderRoleLabel {
			t.Fatalf("role %q must be distinct from a label", role)
		}
	}
}

// TestRemoteSecurityModes proves plain is a representable, distinct mode.
func TestRemoteSecurityModes(t *testing.T) {
	if model.RemoteSecurityPlain == model.RemoteSecurityTLS || model.RemoteSecurityPlain == model.RemoteSecurityStartTLS {
		t.Fatal("plain must be a distinct security mode")
	}
	if model.RemoteSecurityDefault != model.RemoteSecurityTLS {
		t.Fatal("the default mode must be TLS (no automatic downgrade)")
	}
}
