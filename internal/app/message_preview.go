package app

import (
	"context"
	"io"
	"time"

	"github.com/dellarb/mailmoose/internal/mailparse"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/transport/imap"
)

type remotePreviewSession interface {
	FetchPreview(context.Context, imap.Locator, int64, io.Writer) error
}

// ProcessRemotePreviews is driven only by the process-owned worker, never by
// list/search/detail reads. At most ten new arrivals are attempted per turn.
// Existing mail is not queued, raw MIME is not archived, and failure is harmless.
func (m *RemoteMailboxService) ProcessRemotePreviews(ctx context.Context) (int, error) {
	rows, err := m.Service.Store.PendingRemotePreviews(ctx, 10)
	if err != nil {
		return 0, err
	}
	processed := 0
	for _, rec := range rows {
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		preview := ""
		box, err := m.Service.Store.GetInboxInternal(ctx, rec.AccountID, rec.InboxID)
		if err == nil && box.Enabled && box.RemoteConfigured && box.Kind == model.InboxKindStandalone && m.folderInScope(box, rec.FolderPath) {
			pass, cancel := context.WithTimeout(ctx, 5*time.Second)
			sess, err := m.openRemoteSession(pass, box)
			if err == nil {
				if fetcher, ok := sess.(remotePreviewSession); ok {
					var buf limitedBuffer
					writer := &limitedWriter{w: &buf, limit: mailparse.PreviewBytesLimit}
					loc := imap.Locator{FolderPath: rec.FolderPath, UIDValidity: rec.UIDValidity, UID: rec.UID}
					if fetcher.FetchPreview(pass, loc, mailparse.PreviewBytesLimit, writer) == nil {
						preview = mailparse.PreviewBytes(buf.Bytes())
					}
				}
				sess.Close()
			}
			cancel()
		}
		if ctx.Err() != nil {
			return processed, ctx.Err()
		}
		if err := m.Service.Store.CompleteRemotePreview(ctx, rec.AccountID, rec.InboxID, rec.ID, preview); err != nil {
			return processed, err
		}
		processed++
	}
	return processed, nil
}

func (w *RemoteWorker) drainRemotePreviews() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if w.remote != nil && !w.stopping() {
		_, _ = w.remote.ProcessRemotePreviews(ctx)
	}
}
