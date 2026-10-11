package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

func (w *RemoteWorker) detectGoogle(ctx context.Context, inbox model.Inbox) {
	t, e := w.remote.GoogleAccess(ctx, inbox.AccountID, inbox.ID)
	if e != nil {
		return
	}
	anchor, e := w.svc.Store.GoogleDetection(ctx, inbox.ID)
	if e != nil {
		return
	}
	if anchor == "" {
		p, e := w.remote.Google.Profile(ctx, t)
		if e == nil {
			_ = w.svc.Store.SaveGoogleDetection(ctx, inbox.ID, p.HistoryID)
		}
		return
	}
	page := ""
	for n := 0; n < 20; n++ {
		h, e := w.remote.Google.History(ctx, t, anchor, page)
		if e != nil {
			var mb *model.MailboxError
			if errors.As(e, &mb) && mb.Kind == model.ErrKindNotFound {
				profile, pe := w.remote.Google.Profile(ctx, t)
				if pe == nil {
					_ = w.svc.Store.SaveGoogleDetection(ctx, inbox.ID, profile.HistoryID)
					w.remote.ScheduleRefresh(inbox.AccountID, inbox.ID)
				}
			}
			return
		}
		for _, history := range h.History {
			for _, added := range history.MessagesAdded {
				g, e := w.remote.Google.Get(ctx, t, added.Message.ID, "metadata")
				if e != nil {
					return
				}
				if !containsFoldApp(g.LabelIDs, "INBOX") {
					continue
				}
				if _, e = w.remote.cacheGoogle(ctx, inbox.AccountID, inbox.ID, g); e != nil {
					return
				}
				in := googleInput(g)
				date := time.UnixMilli(g.InternalDate)
				a, inserted, e := w.svc.Store.RecordRemoteArrival(ctx, inbox.AccountID, inbox.ID, store.RemoteArrivalInput{FolderPath: "gmail:" + g.ID, RFCMessageID: in.RFCMessageID, FromName: in.FromName, FromAddress: in.FromAddress, Subject: in.Subject, SizeBytes: in.SizeBytes, InternalDate: &date})
				if e != nil {
					return
				}
				if inserted {
					w.onNewArrival(ctx, inbox, a)
				}
			}
		}
		page = h.NextPageToken
		if page == "" {
			_ = w.svc.Store.SaveGoogleDetection(ctx, inbox.ID, h.HistoryID)
			return
		}
	}
}

func (w *RemoteWorker) googleArrivalRead(ctx context.Context, account, inbox string, a store.RemoteArrival) error {
	id, e := w.svc.Store.GoogleLocalID(ctx, account, inbox, strings.TrimPrefix(a.FolderPath, "gmail:"))
	if e != nil {
		return e
	}
	_, e = w.remote.SetRemoteRead(ctx, model.Principal{AccountID: account, Admin: true}, inbox, id, true)
	return e
}
