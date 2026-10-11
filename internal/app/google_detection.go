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
	start := time.Now()
	arrived, status, err := w.detectGooglePass(ctx, inbox)
	result := remoteSyncResult{started: start, new: arrived, status: status, err: err}
	if err != nil {
		result.status = "error"
	}
	w.remote.logRemoteSync(ctx, inbox.AccountID, inbox.ID, result)
}

// detectGooglePass is the timed body of a Gmail history detection pass. status is
// "complete" on a clean drain (or an established baseline) and "partial" when
// the pass returned early without draining history.
func (w *RemoteWorker) detectGooglePass(ctx context.Context, inbox model.Inbox) (int, string, error) {
	t, e := w.remote.GoogleAccess(ctx, inbox.AccountID, inbox.ID)
	if e != nil {
		return 0, "partial", e
	}
	anchor, e := w.svc.Store.GoogleDetection(ctx, inbox.ID)
	if e != nil {
		return 0, "partial", e
	}
	if anchor == "" {
		p, e := w.remote.Google.Profile(ctx, t)
		if e == nil {
			_ = w.svc.Store.SaveGoogleDetection(ctx, inbox.ID, p.HistoryID)
		}
		return 0, "complete", e
	}
	arrived := 0
	page := ""
	for n := 0; n < 20; n++ {
		h, e := w.remote.Google.History(ctx, t, anchor, page)
		if e != nil {
			var mb *model.MailboxError
			if errors.As(e, &mb) && mb.Kind == model.ErrKindNotFound {
				// An expired history cursor is a routine resync trigger, not a
				// failed pass: reset the anchor and schedule a full reconcile.
				// Report partial rather than error so the log is not alarming.
				profile, pe := w.remote.Google.Profile(ctx, t)
				if pe == nil {
					_ = w.svc.Store.SaveGoogleDetection(ctx, inbox.ID, profile.HistoryID)
					w.remote.ScheduleRefresh(inbox.AccountID, inbox.ID)
				}
				return arrived, "partial", nil
			}
			return arrived, "partial", e
		}
		for _, history := range h.History {
			for _, added := range history.MessagesAdded {
				g, e := w.remote.Google.Get(ctx, t, added.Message.ID, "metadata")
				if e != nil {
					return arrived, "partial", e
				}
				if !containsFoldApp(g.LabelIDs, "INBOX") {
					continue
				}
				if _, e = w.remote.cacheGoogle(ctx, inbox.AccountID, inbox.ID, g); e != nil {
					return arrived, "partial", e
				}
				in := googleInput(g)
				date := time.UnixMilli(g.InternalDate)
				a, inserted, e := w.svc.Store.RecordRemoteArrival(ctx, inbox.AccountID, inbox.ID, store.RemoteArrivalInput{FolderPath: "gmail:" + g.ID, RFCMessageID: in.RFCMessageID, FromName: in.FromName, FromAddress: in.FromAddress, Subject: in.Subject, SizeBytes: in.SizeBytes, InternalDate: &date})
				if e != nil {
					return arrived, "partial", e
				}
				if inserted {
					arrived++
					w.onNewArrival(ctx, inbox, a)
				}
			}
		}
		page = h.NextPageToken
		if page == "" {
			_ = w.svc.Store.SaveGoogleDetection(ctx, inbox.ID, h.HistoryID)
			return arrived, "complete", nil
		}
	}
	return arrived, "partial", nil
}

func (w *RemoteWorker) googleArrivalRead(ctx context.Context, account, inbox string, a store.RemoteArrival) error {
	id, e := w.svc.Store.GoogleLocalID(ctx, account, inbox, strings.TrimPrefix(a.FolderPath, "gmail:"))
	if e != nil {
		return e
	}
	_, e = w.remote.SetRemoteRead(ctx, model.Principal{AccountID: account, Admin: true}, inbox, id, true)
	return e
}
