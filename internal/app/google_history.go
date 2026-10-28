package app

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
	"github.com/dellarb/mammothmail/internal/transport/gmail"
)

type gmailHistoryItem struct {
	ID      string `json:"id"`
	Deleted bool   `json:"deleted,omitempty"`
}

type gmailHistoryPending struct {
	Items []gmailHistoryItem `json:"items"`
	Next  string             `json:"next"`
	Final string             `json:"final"`
}

type gmailHistoryExpired struct{ error }

// drainGoogleHistory makes paced history work resumable inside a page. The
// original anchor is retained until every page and item is durably applied.
// Snapshotting IDs prevents repeated requests from moving the set under an
// intra-page offset. No message body or credentials enter this table.
func (m *RemoteMailboxService) drainGoogleHistory(ctx context.Context, account, inbox, purpose, anchor string, client *gmail.Client, token string, apply func(context.Context, gmailHistoryItem) error) (string, bool, error) {
	state, err := m.Service.Store.GmailHistoryPage(ctx, account, inbox, purpose)
	if err != nil {
		return anchor, false, err
	}
	if state.Anchor != anchor {
		state = store.GmailHistoryPage{Anchor: anchor}
	}
	budget := 10
	for page := 0; page < 5 && budget > 0; page++ {
		var pending gmailHistoryPending
		if state.Payload != "" {
			if err := json.Unmarshal([]byte(state.Payload), &pending); err != nil {
				return anchor, false, err
			}
		} else {
			h, err := client.History(ctx, token, anchor, state.PageToken)
			if err != nil {
				var mb *model.MailboxError
				if errors.As(err, &mb) && mb.Kind == model.ErrKindNotFound {
					return anchor, false, &gmailHistoryExpired{err}
				}
				return anchor, false, err
			}
			if h.HistoryID == "" {
				return anchor, false, oneError(model.ErrKindUnavailable, "Gmail history response lacks a cursor")
			}
			budget--
			ids := make(map[string]bool)
			for _, history := range h.History {
				if purpose == "detection" {
					for _, added := range history.MessagesAdded {
						ids[added.Message.ID] = false
					}
					continue
				}
				for _, set := range [][]gmail.HistoryMessage{history.MessagesAdded, history.LabelsAdded, history.LabelsRemoved} {
					for _, changed := range set {
						if _, exists := ids[changed.Message.ID]; !exists {
							ids[changed.Message.ID] = false
						}
					}
				}
				for _, deleted := range history.MessagesDeleted {
					ids[deleted.Message.ID] = true
				}
			}
			for id, deleted := range ids {
				pending.Items = append(pending.Items, gmailHistoryItem{ID: id, Deleted: deleted})
			}
			sort.Slice(pending.Items, func(i, j int) bool { return pending.Items[i].ID < pending.Items[j].ID })
			pending.Next, pending.Final = h.NextPageToken, h.HistoryID
			b, err := json.Marshal(pending)
			if err != nil {
				return anchor, false, err
			}
			state.Payload = string(b)
			if err = m.Service.Store.SaveGmailHistoryPage(ctx, account, inbox, purpose, state); err != nil {
				return anchor, false, err
			}
		}
		for state.Offset < len(pending.Items) {
			if budget <= 0 {
				return anchor, false, nil
			}
			if err = apply(ctx, pending.Items[state.Offset]); err != nil {
				return anchor, false, err
			}
			budget--
			state.Offset++
			if err = m.Service.Store.SaveGmailHistoryPage(ctx, account, inbox, purpose, state); err != nil {
				return anchor, false, err
			}
		}
		if pending.Next == "" {
			// A no-change response may have Final == Anchor. Never retain a
			// completed snapshot: the next poll must discover later changes.
			// Clear before advancing the cursor; a crash can replay completed
			// items safely, but can never skip unapplied ones.
			if err = m.Service.Store.ClearGmailHistoryPage(ctx, account, inbox, purpose); err != nil {
				return anchor, false, err
			}
			return pending.Final, true, nil
		}
		state.PageToken, state.Payload, state.Offset = pending.Next, "", 0
		if err = m.Service.Store.SaveGmailHistoryPage(ctx, account, inbox, purpose, state); err != nil {
			return anchor, false, err
		}
	}
	return anchor, false, nil
}
