package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/idgen"
	"github.com/dellarb/mailmoose/internal/mailparse"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/safepath"
	"github.com/dellarb/mailmoose/internal/store"
)

// HandoffOutcome is the result of a remote Drafts append. A publisher never
// claims guaranteed exactly-once delivery: Confirmed is true only when the
// destination is known (an APPENDUID was reported, or a follow-up lookup found
// the appended draft). When Confirmed is false and err is nil the caller must
// treat the result as ambiguous, not as success and not as a blind retry.
type HandoffOutcome struct {
	// Confirmed reports that the appended draft was located on the remote server.
	Confirmed bool
	// Found reports that at least one remote match exists. It distinguishes a
	// definitive "not present" (Found=false, Confirmed=false) from "present but
	// ambiguous" (Found=true, Confirmed=false, Ambiguous=true), which the retry
	// decision depends on.
	Found bool
	// Ambiguous reports more than one remote match. A retry must not append when
	// Ambiguous is set: a second copy could be created.
	Ambiguous bool
	// RemoteUID is the appended message's UID when the server reported one, else
	// zero.
	RemoteUID uint32
	// RemoteFolder is the folder the draft was appended to.
	RemoteFolder string
}

// HandoffPublisher publishes a RemoteDraft handoff to a standalone inbox's
// connected remote server. The production implementation is
// RemoteHandoffPublisher (backed by the IMAP transport), installed by
// InstallRemoteBridges; tests use a deterministic fake.
//
// Append appends the frozen raw MIME to the inbox's remote Drafts folder with the
// stable handoff correlation header set, and reports whether the append was
// confirmed. When the server does not report an APPENDUID and confirmation is
// uncertain, Append returns Confirmed=false, err=nil; the caller then calls
// Lookup to verify by handoff id and Message-ID before deciding between Published
// and Ambiguous.
type HandoffPublisher interface {
	Append(ctx context.Context, inboxID string, raw []byte, messageID, handoffID string) (HandoffOutcome, error)
	// Lookup searches the inbox's remote Drafts folder for a previously appended
	// handoff by its stable handoff id (falling back to the Message-ID), returning
	// Confirmed=true with the locator when exactly one match exists.
	Lookup(ctx context.Context, inboxID, handoffID, messageID string) (HandoffOutcome, error)
}

// SetHandoffPublisher installs the remote-Drafts publisher. It is called once at
// startup by cmd/server when the remote integration is available. It sets the
// single bridge on this Service (never a global).
func (s *Service) SetHandoffPublisher(p HandoffPublisher) { s.HandoffPublisher = p }

// handoffFolderRole is the remote folder role a RemoteDraft handoff targets.
const handoffFolderRole = model.FolderRoleDrafts

// requestRemoteDraft creates a one-way handoff of a frozen draft to the inbox's
// connected remote Drafts folder. It requires Assistant or Owner on the inbox.
// The frozen raw MIME (with the handoff correlation header) is written once and
// is immutable; the worker appends it. The draft is never sent by MailMoose, and
// its own notification is queued independently of publication.
func (s *Service) requestRemoteDraft(ctx context.Context, p model.Principal, d model.Draft) (model.Draft, error) {
	to, err := cleanAddresses(d.To)
	if err != nil {
		return model.Draft{}, err
	}
	if len(to) == 0 {
		return model.Draft{}, fmt.Errorf("recipient required")
	}
	if strings.TrimSpace(d.Text) == "" && strings.TrimSpace(d.HTML) == "" {
		return model.Draft{}, fmt.Errorf("message body is required")
	}
	inbox, err := s.Store.GetInboxInternal(ctx, p.AccountID, d.InboxID)
	if err != nil {
		return model.Draft{}, err
	}
	if inbox.Kind != model.InboxKindStandalone {
		return model.Draft{}, store.ErrHandoffUnsupported
	}
	atts, err := s.Store.ListDraftAttachments(ctx, p, d.ID)
	if err != nil {
		return model.Draft{}, err
	}
	atts, err = s.ensureAttachmentHashes(ctx, p.AccountID, d.ID, atts)
	if err != nil {
		return model.Draft{}, err
	}
	contentHash := draftContentHash(d, atts)

	// The remote Drafts folder is resolved from the inbox's folder map up front
	// so publication is deterministic even if the folder is renamed locally later.
	folder, ferr := s.Store.GetSystemFolder(ctx, p.AccountID, d.InboxID, handoffFolderRole)
	if ferr != nil || strings.TrimSpace(folder.Path) == "" {
		return model.Draft{}, fmt.Errorf("%w: this inbox has no remote Drafts folder", store.ErrHandoffUnsupported)
	}

	handoffID := idgen.New("hnd")
	messageID := fmt.Sprintf("<%s@%s>", strings.TrimPrefix(idgen.New("msg"), "msg_"), domainOf(inbox.Address))
	// Freeze the exact bytes that will be appended. The handoff header and the
	// Message-ID travel with the frozen copy, so the remote draft is correlatable
	// and the handoff's own notification can be excluded from future detection.
	raw, err := s.buildHandoffMIME(ctx, inbox, d, atts, messageID, handoffID)
	if err != nil {
		return model.Draft{}, err
	}
	if int64(len(raw)) > s.Config.MaxMessageBytes {
		return model.Draft{}, fmt.Errorf("draft exceeds the maximum message size")
	}
	path := s.handoffPath()
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return model.Draft{}, err
	}
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		return model.Draft{}, err
	}
	rel, _ := filepath.Rel(s.Config.DataDir, path)

	created, events, err := s.Store.CreateAssistantHandling(ctx, p, store.AssistantHandlingInsert{
		DraftID:      d.ID,
		InboxID:      d.InboxID,
		Mode:         model.AuthoringRemoteDraft,
		ContentHash:  contentHash,
		HandoffID:    handoffID,
		MessageID:    messageID,
		RemoteFolder: folder.Path,
		RawPath:      filepath.ToSlash(rel),
		SizeBytes:    int64(len(raw)),
	})
	if err != nil {
		_ = os.Remove(path)
		return model.Draft{}, err
	}
	s.publishAll(events)
	// The notification is a separate concern from publication: if it cannot be
	// queued (for example no outbound provider), the handoff is still created and
	// remains publishable, and the notification is reported unavailable rather
	// than failing the request.
	if err := s.enqueueHandoffNotification(ctx, p.AccountID, created); err != nil {
		s.Log.Warn("handoff notification not queued", "handoff_id", created.HandoffID, "inbox_id", created.InboxID, "error", err)
	}
	return s.Store.GetDraft(ctx, p, d.ID)
}

// PublishHandoffs publishes due remote-draft handoffs through the injected
// HandoffPublisher. It is called by the outbox worker. When no publisher is
// injected (a deployment that wired no remote integration, or a test) it is a
// no-op: a handoff stays queued rather than being failed.
func (s *Service) PublishHandoffs(ctx context.Context) {
	if s.HandoffPublisher == nil {
		return
	}
	// A transient append failure leaves the handoff Pending, so it would be
	// re-claimed immediately. Attempt each handoff at most once per pass and let
	// the next worker tick retry it, so a persistent transient fault cannot spin
	// this loop.
	attempted := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		r, ok, err := s.Store.ClaimNextHandoff(ctx, "handoff")
		if err != nil {
			s.Log.Error("handoff claim", "error", err)
			return
		}
		if !ok {
			return
		}
		if attempted[r.ID] {
			return
		}
		attempted[r.ID] = true
		s.publishOneHandoff(ctx, r)
	}
}

// publishOneHandoff appends one handoff's frozen draft to the remote Drafts
// folder and settles its publication state. It never claims guaranteed
// exactly-once: when the append is not confirmed and the follow-up lookup cannot
// verify, it records the explicit ambiguous state instead of retrying blindly.
func (s *Service) publishOneHandoff(ctx context.Context, r model.AssistantHandlingRequest) {
	acct := s.accountForHandoff(ctx, r)
	if acct == "" {
		s.Log.Warn("handoff account unresolved", "handoff_id", r.HandoffID)
		return
	}
	inbox, err := s.Store.GetInboxInternal(ctx, acct, r.InboxID)
	if err != nil {
		s.settleHandoffError(ctx, acct, r, err)
		return
	}
	rawPath, err := safepath.Join(s.Config.DataDir, r.RawPath)
	if err != nil {
		s.settleHandoffError(ctx, acct, r, err)
		return
	}
	raw, err := os.ReadFile(rawPath)
	if err != nil {
		// The frozen file is missing: the append cannot be reproduced and must
		// not be retried with different bytes. Verify by lookup and settle.
		s.verifyHandoffByLookup(ctx, acct, r, inbox)
		return
	}
	// A prior pass may already have appended this handoff (the append succeeded
	// but its APPENDUID/reply was lost). Verify by lookup before a second append
	// so a retry after a transient verification failure cannot create a duplicate
	// draft. r.Attempts > 1 means this handoff has been claimed at least once
	// before this pass.
	if r.Attempts > 1 {
		outcome, lerr := s.HandoffPublisher.Lookup(ctx, inbox.ID, r.HandoffID, r.MessageID)
		switch {
		case lerr != nil:
			// Verification is temporarily impossible: stay pending and re-verify
			// on the next pass, bounded by the append-attempt budget.
			s.deferHandoffRetry(ctx, acct, r)
			return
		case outcome.Confirmed:
			s.settleHandoffPublished(ctx, acct, r, outcome)
			return
		case outcome.Ambiguous:
			s.markHandoffAmbiguous(ctx, acct, r, "more than one remote draft matched this handoff")
			return
		}
		// Definitively not present: fall through and append.
	}
	outcome, err := s.HandoffPublisher.Append(ctx, inbox.ID, raw, r.MessageID, r.HandoffID)
	if err != nil {
		s.settleHandoffError(ctx, acct, r, err)
		return
	}
	if !outcome.Confirmed {
		// The server did not report an APPENDUID: verify by lookup before
		// declaring success. An inconclusive lookup is an explicit ambiguous
		// state, never a retry that could append a duplicate.
		s.verifyHandoffByLookup(ctx, acct, r, inbox)
		return
	}
	s.settleHandoffPublished(ctx, acct, r, outcome)
}

// accountForHandoff resolves the account id that owns a handoff record by way of
// its inbox.
func (s *Service) accountForHandoff(ctx context.Context, r model.AssistantHandlingRequest) string {
	if r.InboxID == "" {
		return ""
	}
	acct, err := s.Store.InboxAccountID(ctx, r.InboxID)
	if err != nil {
		return ""
	}
	return acct
}

// verifyHandoffByLookup confirms a handoff whose append did not report a
// destination by searching the remote Drafts folder for the handoff marker. It
// records Published when exactly one match is found and Ambiguous otherwise.
func (s *Service) verifyHandoffByLookup(ctx context.Context, accountID string, r model.AssistantHandlingRequest, inbox model.Inbox) {
	outcome, err := s.HandoffPublisher.Lookup(ctx, inbox.ID, r.HandoffID, r.MessageID)
	if err != nil {
		// A transient lookup failure is not a definitive outcome: leave the
		// handoff pending so it is retried by lookup (never a blind re-append),
		// bounded by the append-attempt budget.
		s.deferHandoffRetry(ctx, accountID, r)
		return
	}
	if outcome.Confirmed {
		s.settleHandoffPublished(ctx, accountID, r, outcome)
		return
	}
	if outcome.Ambiguous {
		s.markHandoffAmbiguous(ctx, accountID, r, "more than one remote draft matched this handoff")
		return
	}
	// The append command was accepted but the draft cannot be located and no
	// other copy exists. Recording ambiguous (rather than re-appending) avoids
	// duplicating a draft that may exist but is not yet searchable.
	s.markHandoffAmbiguous(ctx, accountID, r, "append result could not be verified on the remote server")
}

// deferHandoffRetry leaves a handoff pending after a non-definitive verification
// failure, so the next pass re-verifies by lookup and never re-appends blindly.
// After the bounded attempt budget it becomes terminally ambiguous.
func (s *Service) deferHandoffRetry(ctx context.Context, accountID string, r model.AssistantHandlingRequest) {
	if r.Attempts >= maxHandoffAppendAttempts {
		s.markHandoffAmbiguous(ctx, accountID, r, "handoff verification did not complete within the retry budget")
		return
	}
	if err := s.Store.RecordHandoffAppendAttempt(ctx, accountID, r.ID, "handoff verification pending"); err != nil {
		s.Log.Warn("record handoff verification attempt", "handoff_id", r.HandoffID, "error", err)
	}
}

// markHandoffAmbiguous records the explicit ambiguous publication state and
// publishes its event. An ambiguous handoff is never automatically re-appended;
// the operator can resolve it (see CancelHandoff).
func (s *Service) markHandoffAmbiguous(ctx context.Context, accountID string, r model.AssistantHandlingRequest, reason string) {
	ev, err := s.Store.MarkHandoffAmbiguous(ctx, accountID, r.ID, reason)
	if err != nil {
		s.Log.Warn("mark handoff ambiguous", "handoff_id", r.HandoffID, "error", err)
		return
	}
	s.publishEventPtr(ev)
}

// settleHandoffPublished records a confirmed publication, then queues the local
// draft's cleanup (its content is now durably on the remote server). Cleanup is
// best-effort here and retried by the sweep; the workflow state is retained.
func (s *Service) settleHandoffPublished(ctx context.Context, accountID string, r model.AssistantHandlingRequest, outcome HandoffOutcome) {
	ev, err := s.Store.MarkHandoffPublished(ctx, accountID, r.ID, outcome.RemoteUID)
	if err != nil {
		s.Log.Warn("mark handoff published", "handoff_id", r.HandoffID, "error", err)
		return
	}
	s.publishEventPtr(ev)
	if err := s.Store.CleanupPublishedHandoffLocalDraft(ctx, accountID, r.ID); err != nil {
		s.Log.Warn("cleanup published handoff local draft", "handoff_id", r.HandoffID, "error", err)
	}
}

// settleHandoffError classifies an append failure. A permanent error is
// terminal; anything else leaves the handoff Pending so it is retried, because
// the append is verified by lookup and never blindly duplicated.
func (s *Service) settleHandoffError(ctx context.Context, accountID string, r model.AssistantHandlingRequest, err error) {
	// A permanent error, or one that has exhausted the bounded retry budget, is
	// terminal: bounded retries keep a persistent transient fault from growing the
	// pending queue without limit.
	if isPermanentHandoffError(err) || r.Attempts >= maxHandoffAppendAttempts {
		if ev, ferr := s.Store.MarkHandoffFailed(ctx, accountID, r.ID, publicHandoffError(err)); ferr == nil {
			s.publishEventPtr(ev)
		}
		return
	}
	if rerr := s.Store.RecordHandoffAppendAttempt(ctx, accountID, r.ID, publicHandoffError(err)); rerr != nil {
		s.Log.Warn("record handoff attempt", "handoff_id", r.HandoffID, "error", rerr)
	}
}

// maxHandoffAppendAttempts bounds how many transient append attempts a handoff
// makes before it is terminally failed. Without a bound a persistent transient
// fault (for example a server that always times out) would retry the append
// forever; the bound keeps the pending queue from growing without limit.
const maxHandoffAppendAttempts = 8

// isPermanentHandoffError reports whether an append error is terminal.
func isPermanentHandoffError(err error) bool {
	return errors.Is(err, store.ErrHandoffUnsupported) || errors.Is(err, store.ErrForbidden) || errors.Is(err, store.ErrNotFound)
}

// publicHandoffError returns a safe description of a handoff failure. It never
// includes credential material or a raw provider response body.
func publicHandoffError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, store.ErrHandoffUnsupported) {
		return "remote draft handoff is not available for this inbox"
	}
	return "remote draft append failed"
}

// handoffPath returns a fresh path for a handoff's frozen raw MIME. It is kept
// under its own tree so mailbox cleanup never touches it.
func (s *Service) handoffPath() string {
	id := idgen.New("raw")
	return filepath.Join(s.Config.DataDir, "handoff", id[4:6], id[6:8], id+".eml")
}

// buildHandoffMIME renders the frozen raw MIME for a handoff, with the handoff
// correlation header as the first header line. Attachments are embedded so the
// frozen copy is self-contained. It uses the draft builder so Bcc recipients and
// reply headers travel with the stored draft (a draft has no MailMoose envelope).
func (s *Service) buildHandoffMIME(ctx context.Context, inbox model.Inbox, d model.Draft, atts []model.DraftAttachment, messageID, handoffID string) ([]byte, error) {
	parts := make([]mailparse.Attachment, 0, len(atts))
	for i, a := range atts {
		ap, perr := safepath.Join(s.Config.DataDir, a.RawPath)
		if perr != nil {
			return nil, perr
		}
		data, err := os.ReadFile(ap)
		if err != nil {
			return nil, err
		}
		contentType := strings.TrimSpace(a.ContentType)
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		filename := mailparse.SafeAttachmentFilename(a.Filename, i+1, contentType)
		parts = append(parts, mailparse.Attachment{Filename: filename, ContentType: contentType, Content: data})
	}
	from := model.Address{Name: inbox.DisplayName, Address: inbox.Address}
	if strings.TrimSpace(d.FromAddress) != "" {
		from.Address = d.FromAddress
		if strings.TrimSpace(d.FromName) != "" {
			from.Name = d.FromName
		}
	}
	// Resolve the reply source, when there is one, so the stored draft carries a
	// valid In-Reply-To/References and a human sending it replies in-thread. A
	// missing/unresolvable source omits the headers rather than failing the
	// handoff.
	inReply := ""
	var refs []string
	if strings.TrimSpace(d.ReplyToMessageID) != "" {
		if src, rerr := s.resolveSendSource(ctx, inbox.AccountID, d.InboxID, d.ReplyToMessageID); rerr == nil {
			inReply = ensureMessageID(src.RFCMessageID)
			for _, r := range src.References {
				refs = append(refs, ensureMessageID(r))
			}
			if inReply != "" {
				refs = appendUnique(refs, inReply)
			}
		}
	}
	raw, err := mailparse.BuildDraftMessage(mailparse.Address{Name: from.Name, Address: from.Address}, d.To, d.CC, d.BCC, d.Subject, d.Text, d.HTML, messageID, inReply, refs, time.Now().UTC(), parts)
	if err != nil {
		return nil, err
	}
	return injectHandoffHeader(raw, handoffID), nil
}

// injectHandoffHeader inserts the handoff correlation header as the first header
// line of a raw RFC5322 message.
func injectHandoffHeader(raw []byte, handoffID string) []byte {
	if strings.TrimSpace(handoffID) == "" {
		return raw
	}
	header := []byte(model.HandoffHeader + ": " + handoffID + "\r\n")
	out := make([]byte, 0, len(raw)+len(header))
	out = append(out, header...)
	out = append(out, raw...)
	return out
}

// publishEventPtr publishes a single optional event.
func (s *Service) publishEventPtr(ev *model.Event) {
	if ev == nil {
		return
	}
	s.publish(*ev)
}

// handoffNotificationSubject is the human-readable subject of a handoff
// notification. It never carries a control token.
func handoffNotificationSubject(draftSubject string) string {
	draftSubject = strings.TrimSpace(draftSubject)
	if draftSubject == "" {
		draftSubject = "(no subject)"
	}
	return "Draft placed in your Drafts folder: " + draftSubject
}

// enqueueHandoffNotification queues a notification that the handoff was created,
// through the existing outbound_workflow queue. The job records only the
// notification kind (model.WorkflowKindHandoff) and carries no approval token;
// the human reaches the draft in their own mail client's Drafts folder.
//
// Publication and notification are separate: a handoff whose notification cannot
// be queued or delivered is still published. When the inbox's domain has no
// outbound provider (the standalone case before the SMTP bridge is wired), the
// job is still enqueued and held, so a later integration can deliver it without
// re-creating the handoff or re-appending the draft.
func (s *Service) enqueueHandoffNotification(ctx context.Context, accountID string, r model.AssistantHandlingRequest) error {
	inbox, err := s.Store.GetInboxInternal(ctx, accountID, r.InboxID)
	if err != nil {
		return err
	}
	settings, err := s.Store.GetInboxAuthoringSettingsInternal(ctx, accountID, r.InboxID)
	if err != nil {
		return err
	}
	to := strings.TrimSpace(settings.NotifyAddress)
	if to == "" {
		// Blank override means the inbox's own connected address; if even that is
		// unavailable the notification cannot be addressed and is reported
		// unavailable rather than silently dropped.
		return fmt.Errorf("no notification address available for this inbox")
	}
	// The reviewed subject is read from the stored draft; the draft row still
	// exists at this point (it is cleaned up only after publication).
	d, derr := s.Store.GetDraftInternal(ctx, accountID, r.DraftID)
	subject := r.MessageID
	text := "A draft has been placed in the Drafts folder of your connected mailbox, ready for you to review and send from your own email client.\r\n\r\nThis message is a notification only; it is not an approval request and carries no token."
	if derr == nil {
		subject = d.Subject
		text = "A draft has been placed in the Drafts folder of your connected mailbox, ready for you to review and send from your own email client.\r\n\r\nSubject: " + d.Subject + "\r\n\r\nThis message is a notification only; it is not an approval request and carries no token."
	}
	from := model.Address{Name: inbox.DisplayName, Address: inbox.Address}
	msgID := fmt.Sprintf("<%s@%s>", strings.TrimPrefix(idgen.New("msg"), "msg_"), domainOf(inbox.Address))
	// The notification MIME carries the handoff correlation header so remote
	// detection can exclude it, but no approval token markers.
	body := mailparse.Address{Name: from.Name, Address: from.Address}
	raw, err := mailparse.BuildMessage(body, []string{to}, nil, nil, handoffNotificationSubject(subject), text, "", msgID, "", nil, time.Now().UTC(), nil)
	if err != nil {
		return err
	}
	raw = injectHandoffHeader(raw, r.HandoffID)
	path := s.handoffPath()
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		return err
	}
	rel, _ := filepath.Rel(s.Config.DataDir, path)
	provider := ""
	if sending, cerr := s.sendingConfigForInbox(ctx, accountID, inbox); cerr == nil {
		provider = sending.Provider
	}
	if _, err := s.Store.CommitHandoffNotification(ctx, accountID, r, store.WorkflowRecord{
		Inbox:     inbox,
		From:      from,
		To:        []string{to},
		Subject:   handoffNotificationSubject(subject),
		Text:      text,
		HTML:      "",
		RawPath:   filepath.ToSlash(rel),
		SizeBytes: int64(len(raw)),
	}, provider, "", msgID); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}
