package store

import (
	"context"
	"strings"

	"github.com/dellarb/mammothmail/internal/model"
)

// This file holds the durable record of a remote placement. A remote label or
// folder change is part of the durable truth, not only a transient queue
// notification: the event names the canonical identities the API reports.

// RecordRemoteMessageEvent records a durable event for a cached remote
// message. It is the remote counterpart of the local message events.
func (s *Store) RecordRemoteMessageEvent(ctx context.Context, accountID, inboxID, messageID, typ string, payload map[string]any) (model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.Event{}, err
	}
	defer tx.Rollback()
	ev, err := insertEventTx(ctx, tx, accountID, inboxID, typ, messageID, payload)
	if err != nil {
		return model.Event{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.Event{}, err
	}
	return ev, nil
}

// RemoteMessageFolderID returns the canonical folder id of a cached remote
// message: the single folder its provider state places it in, or empty when
// it has none.
//
// A Gmail message is placed by the first system label it carries (Trash,
// Spam, Drafts, Inbox, Sent). Another provider's message is placed by the
// folder it was indexed in. A user label is not a location, so it places
// nothing.
func (s *Store) RemoteMessageFolderID(ctx context.Context, accountID, inboxID, messageID string) (string, error) {
	if s.IsGoogle(ctx, accountID, inboxID) {
		return s.googleMessageFolderID(ctx, accountID, inboxID, messageID)
	}
	rec, err := s.GetRemoteMessage(ctx, accountID, inboxID, messageID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(rec.FolderPath) == "" {
		return "", nil
	}
	f, err := s.GetFolder(ctx, accountID, inboxID, rec.FolderPath)
	if err != nil {
		if err == ErrNotFound {
			return "", nil
		}
		return "", err
	}
	return f.ID, nil
}

// googleMessageFolderID resolves the folder a cached Gmail message belongs to.
func (s *Store) googleMessageFolderID(ctx context.Context, accountID, inboxID, messageID string) (string, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT label_id FROM google_label_memberships WHERE inbox_id=? AND message_id=?`, inboxID, messageID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	carried := map[string]bool{}
	for rows.Next() {
		var label string
		if err = rows.Scan(&label); err != nil {
			return "", err
		}
		carried[label] = true
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	for _, label := range GoogleFolderPrecedence {
		if !carried[label] {
			continue
		}
		f, err := s.GetFolder(ctx, accountID, inboxID, label)
		if err != nil {
			if err == ErrNotFound {
				return "", nil
			}
			return "", err
		}
		return f.ID, nil
	}
	return "", nil
}
