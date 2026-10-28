package app

import (
	"errors"

	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
)

// ErrRemoteNotBound is returned when a remote operation is requested for a
// standalone inbox that has no configured remote connector yet.
var ErrRemoteNotBound = errors.New("standalone inbox has no remote connector configured")

// MailboxBackendKind names where a mailbox's data lives. It is the per-inbox
// classification the HTTP layer reports; the live dispatch itself happens in
// internal/httpapp/mailbox_access.go.
type MailboxBackendKind string

const (
	// BackendLocal is the local SQLite store: messages are persisted and their
	// bodies archived under /data. Domain inboxes are always local.
	BackendLocal MailboxBackendKind = "local"
	// BackendRemote is a remote IMAP account: headers/thread metadata are cached
	// locally but bodies and attachments are fetched live and never archived.
	BackendRemote MailboxBackendKind = "remote"
)

// mapStoreError converts a store error to a normalized *model.MailboxError.
func mapStoreError(err error) error {
	if err == nil {
		return nil
	}
	var mb *model.MailboxError
	if errors.As(err, &mb) {
		return err
	}
	switch {
	case errors.Is(err, store.ErrInvalidCursor):
		return model.NewMailboxError(model.ErrKindInvalid, "invalid cursor", false, err)
	case errors.Is(err, store.ErrNotFound):
		return model.NewMailboxError(model.ErrKindNotFound, "mailbox not found", false, err)
	case errors.Is(err, store.ErrForbidden):
		return model.NewMailboxError(model.ErrKindForbidden, "not permitted", false, err)
	case errors.Is(err, store.ErrConflict):
		return model.NewMailboxError(model.ErrKindConflict, "conflicting change", false, err)
	case errors.Is(err, store.ErrQuota):
		return model.NewMailboxError(model.ErrKindQuota, "storage quota exceeded", false, err)
	case errors.Is(err, store.ErrRemoteMetadataQuota):
		return model.NewMailboxError(model.ErrKindQuota, "remote metadata storage quota exceeded", false, err)
	case errors.Is(err, store.ErrStandaloneRequired):
		return model.NewMailboxError(model.ErrKindUnsupported, "operation requires a standalone inbox", false, err)
	default:
		return model.NewMailboxError(model.ErrKindInternal, "", false, err)
	}
}
