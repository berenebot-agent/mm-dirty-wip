package httpapp

import "github.com/dellarb/mailmoose/internal/model"

// AppJS returns the embedded browser bundle. It is exported so black-box tests
// can assert on user-visible dynamic behaviour without duplicating the asset.
func AppJS() []byte { return appJS }

// InboxReadiness exposes the per-inbox readiness derivation so callers and
// black-box tests can report a standalone inbox's IMAP/SMTP state distinctly from
// a domain's receiving provider. It returns, in order: per-domain sending
// readiness, per-domain receiving readiness, per-inbox sending readiness and
// per-inbox receiving readiness.
func InboxReadiness(domains []model.Domain, boxes []model.Inbox) (sending, receiving, inboxSending, inboxReceiving map[string]bool) {
	return inboxReadiness(domains, boxes)
}
