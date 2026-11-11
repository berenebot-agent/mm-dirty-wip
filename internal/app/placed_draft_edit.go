package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dellarb/mammothmail/internal/idgen"
	"github.com/dellarb/mammothmail/internal/mailparse"
	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
	"github.com/dellarb/mammothmail/internal/transport/imap"
	"github.com/dellarb/mammothmail/internal/transport/microsoft"
)

// A placed draft is a draft that already lives in a connected mailbox's Drafts
// folder. It is an ordinary draft to everyone involved: the operator edits it in
// their own mail client or here, and both see the same message. The app holds
// only its header metadata (the cached remote index) and fetches the body live,
// so nothing is duplicated locally and no draft consumes account storage.

// ErrPlacedDraftChanged reports that the draft in the connected mailbox changed
// after this editor loaded it. Saving would silently discard whatever the
// operator did in Outlook or Gmail, so the caller has to reload or overwrite.
var ErrPlacedDraftChanged = errors.New("the draft changed in the connected mailbox; reload it before saving")

// PlacedDraftFields is the editable envelope of a placed draft.
type PlacedDraftFields struct {
	To, CC, BCC           []string
	Subject, Text, HTML   string
	FromAddress, FromName string
}

// PlacedDraft is a placed draft read from the connected mailbox.
type PlacedDraft struct {
	ID         string
	InboxID    string
	FolderPath string
	Fields     PlacedDraftFields
	// Attachments are file names only: this version carries an existing
	// attachment through a save unchanged rather than editing it.
	Attachments []string
	SizeBytes   int64
	// Marker proves, at save time, that the copy the editor read is still the
	// copy on the server.
	Marker string
	// handoffID is the correlation header of the request that placed the draft,
	// preserved across edits so the placement stays attributable.
	handoffID string
}

// LoadPlacedDraft reads a placed draft's editable fields from the connected
// mailbox. The body is fetched transiently and never archived.
func (m *RemoteMailboxService) LoadPlacedDraft(ctx context.Context, p model.Principal, inboxID, draftID string) (PlacedDraft, error) {
	rec, err := m.placedDraftRecord(ctx, p, inboxID, draftID, false)
	if err != nil {
		return PlacedDraft{}, err
	}
	path, _, err := m.fetchRemoteRawFor(ctx, p.AccountID, inboxID, draftID, &rec)
	if err != nil {
		return PlacedDraft{}, err
	}
	defer m.CleanupRemoteRaw(path)
	out, err := placedDraftFromFile(path)
	if err != nil {
		return PlacedDraft{}, err
	}
	live, err := m.placedDraftLive(ctx, p.AccountID, inboxID, rec, path)
	if err != nil {
		return PlacedDraft{}, err
	}
	if live.bccKnown {
		out.Fields.BCC = live.bcc
	}
	out.ID = rec.ID
	out.InboxID = inboxID
	out.FolderPath = rec.FolderPath
	out.SizeBytes = rec.SizeBytes
	out.Marker = live.marker
	return out, nil
}

// SavePlacedDraft writes an edited placed draft back to the connected mailbox.
// The provider keeps the draft in place where it can (Graph PATCH, Gmail
// drafts.update); IMAP has no in-place edit, so the message is appended and the
// previous copy is deleted. The marker must match the loaded copy, so a
// concurrent edit in the operator's own client is never silently overwritten.
//
// It returns the id of the cached row that now holds the draft: unchanged for
// Graph and Gmail, the appended copy's new row for IMAP.
func (m *RemoteMailboxService) SavePlacedDraft(ctx context.Context, p model.Principal, inboxID, draftID string, f PlacedDraftFields, marker string) (string, error) {
	// Editing a draft changes the mailbox, so it needs the same Assistant or
	// Owner authority every other mailbox change does; read access is not enough.
	rec, err := m.placedDraftRecord(ctx, p, inboxID, draftID, true)
	if err != nil {
		return "", err
	}
	// A save without a marker has nothing to prove the editor saw the current
	// copy, so it is refused rather than allowed to overwrite blindly.
	if strings.TrimSpace(marker) == "" {
		return "", ErrPlacedDraftChanged
	}
	path, _, err := m.fetchRemoteRawFor(ctx, p.AccountID, inboxID, draftID, &rec)
	if err != nil {
		// A draft the provider no longer holds under this id was changed or
		// removed elsewhere since the editor loaded it.
		if isRemoteGone(err) {
			return "", ErrPlacedDraftChanged
		}
		return "", err
	}
	// The marker is checked against the provider's current state, not the
	// cached index, which can lag an edit made in the operator's own client.
	live, err := m.placedDraftLive(ctx, p.AccountID, inboxID, rec, path)
	if err != nil {
		m.CleanupRemoteRaw(path)
		if isRemoteGone(err) {
			return "", ErrPlacedDraftChanged
		}
		return "", err
	}
	if live.marker != marker {
		m.CleanupRemoteRaw(path)
		return "", ErrPlacedDraftChanged
	}
	raw, err := m.rebuildPlacedDraft(ctx, p, rec, path, f)
	m.CleanupRemoteRaw(path)
	if err != nil {
		return "", err
	}
	if int64(len(raw)) > m.Service.Config.MaxMessageBytes {
		return "", fmt.Errorf("draft exceeds the maximum message size")
	}

	newID := rec.ID
	switch {
	case m.IsMicrosoft(ctx, p.AccountID, inboxID):
		t, c, e := m.microsoftAccess(ctx, p.AccountID, inboxID)
		if e != nil {
			return "", e
		}
		provider, e := m.Service.Store.MicrosoftMessage(ctx, p.AccountID, inboxID, draftID)
		if e != nil {
			return "", mapStoreError(e)
		}
		if _, e = m.Microsoft.UpdateDraft(ctx, t, c.Mailbox, provider, microsoft.DraftEdit{
			Subject: f.Subject, Text: f.Text, HTML: f.HTML, To: f.To, CC: f.CC, BCC: f.BCC,
		}); e != nil {
			return "", e
		}
	case m.IsGoogle(ctx, p.AccountID, inboxID):
		client, t, e := m.GoogleMailboxClient(ctx, p.AccountID, inboxID)
		if e != nil {
			return "", e
		}
		provider, _, _, e := m.Service.Store.GoogleMessage(ctx, p.AccountID, inboxID, draftID)
		if e != nil {
			return "", mapStoreError(e)
		}
		draftRef, e := client.DraftIDForMessage(ctx, t, provider)
		if e != nil {
			return "", e
		}
		if _, e = client.UpdateDraft(ctx, t, draftRef, bytes.NewReader(raw), ""); e != nil {
			return "", e
		}
	default:
		newID, err = m.replacePlacedDraftIMAP(ctx, p.AccountID, inboxID, rec, raw)
		if err != nil {
			return "", err
		}
	}
	// Refresh the cached index at once rather than waiting for the folder pull:
	// the operator lands back on the list, and a stale subject there would read
	// as a failed save. The pull that follows confirms flags and removals.
	if newID != rec.ID {
		// The append that produced the new row already seeded the index; only the
		// replaced row is left to drop.
		if e := m.Service.Store.DeleteRemoteMessage(ctx, p.AccountID, inboxID, rec.ID); e != nil {
			m.Service.Log.Debug("drop replaced draft row", "inbox_id", inboxID, "error", e)
		}
	} else {
		if _, e := m.seedPlacedDraft(ctx, p.AccountID, inboxID, rec.FolderPath, rec.UIDValidity, rec.UID, raw); e != nil {
			m.Service.Log.Debug("seed edited draft", "inbox_id", inboxID, "error", e)
		}
	}
	m.Service.requestRoleFolderRefresh(p.AccountID, inboxID, model.FolderRoleDrafts)
	return newID, nil
}

// placedDraftRecord resolves a cached row and refuses anything that is not the
// Drafts folder of this inbox.
func (m *RemoteMailboxService) placedDraftRecord(ctx context.Context, p model.Principal, inboxID, draftID string, write bool) (store.RemoteMessage, error) {
	authorize := m.authorizeRead
	if write {
		authorize = m.authorizeAssist
	}
	inbox, err := authorize(ctx, p, inboxID)
	if err != nil {
		return store.RemoteMessage{}, err
	}
	folder, ferr := m.Service.Store.GetSystemFolder(ctx, p.AccountID, inboxID, model.FolderRoleDrafts)
	if ferr != nil || strings.TrimSpace(folder.Path) == "" {
		return store.RemoteMessage{}, model.NewMailboxError(model.ErrKindNotFound, "this inbox has no Drafts folder", false, nil)
	}
	rec, err := m.Service.Store.GetRemoteMessage(ctx, p.AccountID, inboxID, draftID)
	if err != nil {
		return store.RemoteMessage{}, mapStoreError(err)
	}
	if rec.FolderPath != folder.Path || !m.folderInScope(inbox, rec.FolderPath) {
		return store.RemoteMessage{}, model.NewMailboxError(model.ErrKindNotFound, "draft is not in this inbox's Drafts folder", false, nil)
	}
	return rec, nil
}

// replacePlacedDraftIMAP appends the edited message to the Drafts folder under
// the same Message-ID and then removes the previous copy. Append first, delete
// second: a failure between the two leaves a duplicate the folder pull reconciles
// by Message-ID, never a lost draft.
func (m *RemoteMailboxService) replacePlacedDraftIMAP(ctx context.Context, accountID, inboxID string, rec store.RemoteMessage, raw []byte) (string, error) {
	sess, _, err := m.open(ctx, accountID, inboxID)
	if err != nil {
		return "", err
	}
	defer sess.Close()
	res, err := sess.AppendReader(ctx, rec.FolderPath, bytes.NewReader(raw), int64(len(raw)), []string{imap.FlagDraft, imap.FlagSeen}, time.Now().UTC())
	if err != nil {
		return "", normalizeRemoteError(err)
	}
	loc := imap.Locator{FolderPath: rec.FolderPath, UIDValidity: rec.UIDValidity, UID: rec.UID}
	if !res.Confirmed || res.DestinationUID == 0 {
		// The server accepted the append without telling us the new UID, so
		// resolve the copy by its own Message-ID before removing the old one.
		found, ferr := sess.FindByMessageID(ctx, rec.FolderPath, rec.RFCMessageID)
		if ferr != nil {
			return "", normalizeRemoteError(ferr)
		}
		res.DestinationUID, res.UIDValidity = found.UID, found.UIDValidity
	}
	row, serr := m.seedPlacedDraft(ctx, accountID, inboxID, rec.FolderPath, res.UIDValidity, res.DestinationUID, raw)
	if derr := sess.DeleteMessage(ctx, loc); derr != nil {
		// A leftover copy is recoverable by the pull, a failed save is not.
		m.Service.Log.Warn("delete replaced remote draft", "inbox_id", inboxID, "error", normalizeRemoteError(derr))
	}
	if serr != nil {
		// The write succeeded; the index will catch up on the pull. Returning the
		// old id keeps the caller's redirect valid.
		m.Service.Log.Debug("seed replaced draft", "inbox_id", inboxID, "error", serr)
		return rec.ID, nil
	}
	return row.ID, nil
}

// seedPlacedDraft writes a draft's header metadata into the cached remote index.
// It is best-effort and idempotent on the same (folder, uidvalidity, uid) key the
// folder pull uses, so seeding can never duplicate a row.
func (m *RemoteMailboxService) seedPlacedDraft(ctx context.Context, accountID, inboxID, folderPath string, uidValidity, uid uint32, raw []byte) (store.RemoteMessage, error) {
	if len(raw) == 0 || uid == 0 || strings.TrimSpace(folderPath) == "" {
		return store.RemoteMessage{}, fmt.Errorf("a placed draft needs raw MIME, a folder and a uid")
	}
	in, err := remoteDraftIndexInput(raw, folderPath, uidValidity, uid)
	if err != nil {
		return store.RemoteMessage{}, err
	}
	return m.Service.Store.UpsertRemoteMessage(ctx, accountID, inboxID, in)
}

// remoteDraftIndexInput projects a draft's raw MIME onto a cached metadata row.
// A placed draft is \Draft \Seen on the server, and the local row has to agree
// from its first read or the draft counts as new mail.
func remoteDraftIndexInput(raw []byte, folderPath string, uidValidity, uid uint32) (store.RemoteMessageInput, error) {
	parsed, err := mailparse.ParseBytes(raw)
	if err != nil {
		return store.RemoteMessageInput{}, err
	}
	in := store.RemoteMessageInput{
		FolderPath:   folderPath,
		UIDValidity:  uidValidity,
		UID:          uid,
		RFCMessageID: parsed.RFCMessageID,
		InReplyTo:    parsed.InReplyTo,
		References:   parsed.References,
		FromName:     parsed.From.Name,
		FromAddress:  parsed.From.Address,
		To:           parsed.To,
		CC:           parsed.CC,
		Subject:      parsed.Subject,
		Snippet:      draftSnippet(parsed.Text),
		SizeBytes:    int64(len(raw)),
		HasAttach:    len(parsed.Attachments) > 0,
		Read:         true,
		Draft:        true,
		Flags:        []string{"\\Draft", "\\Seen"},
	}
	if !parsed.Date.IsZero() {
		at := parsed.Date.UTC().Format(time.RFC3339Nano)
		in.SentAt = &at
		in.ReceivedAt = &at
		in.InternalDate = &at
	}
	return in, nil
}

// rebuildPlacedDraft serialises the edited draft back to RFC 5322, keeping the
// original Message-ID, thread headers and attachments so the message stays the
// same draft to every reader.
func (m *RemoteMailboxService) rebuildPlacedDraft(ctx context.Context, p model.Principal, rec store.RemoteMessage, path string, f PlacedDraftFields) ([]byte, error) {
	parsed, err := mailparse.ParseFile(path)
	if err != nil {
		return nil, err
	}
	handoffID, err := placedDraftHandoffID(path)
	if err != nil {
		return nil, err
	}
	parts := make([]mailparse.Attachment, 0, len(parsed.Attachments))
	for _, a := range parsed.Attachments {
		part := mimePartPath(a.PartPath)
		if len(part) == 0 {
			continue
		}
		att, aerr := m.FetchRemoteAttachment(ctx, p, rec.InboxID, rec.ID, part, a.Filename, a.ContentType)
		if aerr != nil {
			return nil, aerr
		}
		data, rerr := os.ReadFile(att.Path)
		m.CleanupRemoteRaw(att.Path)
		if rerr != nil {
			return nil, rerr
		}
		contentType := strings.TrimSpace(a.ContentType)
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		parts = append(parts, mailparse.Attachment{Filename: a.Filename, ContentType: contentType, Content: data})
	}
	from := parsed.From
	if strings.TrimSpace(f.FromAddress) != "" {
		from = mailparse.Address{Address: f.FromAddress, Name: f.FromName}
	}
	messageID := parsed.RFCMessageID
	if strings.TrimSpace(messageID) == "" {
		messageID = fmt.Sprintf("<%s@%s>", strings.TrimPrefix(idgen.New("msg"), "msg_"), "draft.local")
	}
	date := parsed.Date
	if date.IsZero() {
		date = time.Now().UTC()
	}
	raw, err := mailparse.BuildDraftMessage(from, f.To, f.CC, f.BCC, f.Subject, f.Text, f.HTML, messageID, parsed.InReplyTo, parsed.References, date.UTC(), parts)
	if err != nil {
		return nil, err
	}
	return injectHandoffHeader(raw, handoffID), nil
}

// placedDraftFromFile reads the editable fields of a draft's raw MIME.
func placedDraftFromFile(path string) (PlacedDraft, error) {
	parsed, err := mailparse.ParseFile(path)
	if err != nil {
		return PlacedDraft{}, err
	}
	out := PlacedDraft{Fields: PlacedDraftFields{
		To: parsed.To, CC: parsed.CC, BCC: parsed.BCC, Subject: parsed.Subject, Text: parsed.Text, HTML: parsed.HTML,
		FromAddress: parsed.From.Address, FromName: parsed.From.Name,
	}}
	for _, a := range parsed.Attachments {
		out.Attachments = append(out.Attachments, a.Filename)
	}
	handoffID, err := placedDraftHandoffID(path)
	if err != nil {
		return PlacedDraft{}, err
	}
	out.handoffID = handoffID
	return out, nil
}

// placedDraftHandoffID reads the correlation header of the request that placed
// the draft, so an edit keeps the placement attributable.
func placedDraftHandoffID(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	msg, err := mail.ReadMessage(bufio.NewReader(f))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(msg.Header.Get(model.HandoffHeader)), nil
}

// placedDraftLiveState is what the provider reports about a draft right now.
type placedDraftLiveState struct {
	// marker identifies the copy that was read. It changes whenever the draft is
	// saved anywhere, including in the operator's own mail client.
	marker string
	// bcc is the draft's Bcc list when the provider reports it separately from
	// the MIME (Graph's MIME rendering omits Bcc); bccKnown says whether to use it.
	bcc      []string
	bccKnown bool
}

// placedDraftLive asks the provider for the draft's current revision instead of
// trusting the cached index. rawPath is the body just fetched for the same draft:
//   - Graph: the item's changeKey, which moves on every change.
//   - Gmail: a message is immutable and drafts.update replaces it with a new id,
//     so the draft is current exactly while a draft still holds this message id.
//   - IMAP: there is no revision token, so the marker is the UID plus a hash of
//     the bytes just fetched; any edit changes the bytes, and a replaced draft
//     has a new UID.
func (m *RemoteMailboxService) placedDraftLive(ctx context.Context, accountID, inboxID string, rec store.RemoteMessage, rawPath string) (placedDraftLiveState, error) {
	switch {
	case m.IsMicrosoft(ctx, accountID, inboxID):
		t, c, err := m.microsoftAccess(ctx, accountID, inboxID)
		if err != nil {
			return placedDraftLiveState{}, err
		}
		provider, err := m.Service.Store.MicrosoftMessage(ctx, accountID, inboxID, rec.ID)
		if err != nil {
			return placedDraftLiveState{}, mapStoreError(err)
		}
		msg, err := m.Microsoft.DraftState(ctx, t, c.Mailbox, provider)
		if err != nil {
			return placedDraftLiveState{}, err
		}
		if strings.TrimSpace(msg.ChangeKey) == "" {
			return placedDraftLiveState{}, fmt.Errorf("the connected mailbox did not report a draft revision")
		}
		bcc := make([]string, 0, len(msg.BCCRecipients))
		for _, r := range msg.BCCRecipients {
			if a := strings.ToLower(strings.TrimSpace(r.EmailAddress.Address)); a != "" {
				bcc = append(bcc, a)
			}
		}
		return placedDraftLiveState{marker: "ms:" + msg.ChangeKey, bcc: bcc, bccKnown: true}, nil
	case m.IsGoogle(ctx, accountID, inboxID):
		provider, _, _, err := m.Service.Store.GoogleMessage(ctx, accountID, inboxID, rec.ID)
		if err != nil {
			return placedDraftLiveState{}, mapStoreError(err)
		}
		client, t, err := m.GoogleMailboxClient(ctx, accountID, inboxID)
		if err != nil {
			return placedDraftLiveState{}, err
		}
		if _, err = client.DraftIDForMessage(ctx, t, provider); err != nil {
			// No draft holds this message any more: it was edited or removed.
			return placedDraftLiveState{}, ErrPlacedDraftChanged
		}
		return placedDraftLiveState{marker: "g:" + provider}, nil
	default:
		data, err := os.ReadFile(rawPath)
		if err != nil {
			return placedDraftLiveState{}, err
		}
		sum := sha256.Sum256(data)
		return placedDraftLiveState{marker: fmt.Sprintf("i:%d/%d/%s", rec.UIDValidity, rec.UID, hex.EncodeToString(sum[:]))}, nil
	}
}

// isRemoteGone reports whether a provider error means the item is no longer
// there under the id the cache holds.
func isRemoteGone(err error) bool {
	var me *model.MailboxError
	return errors.As(err, &me) && me.Kind == model.ErrKindNotFound
}

// mimePartPath parses an IMAP-style MIME part path ("1.2") into its indices.
func mimePartPath(path string) []int {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	fields := strings.Split(path, ".")
	out := make([]int, 0, len(fields))
	for _, field := range fields {
		n, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || n <= 0 {
			return nil
		}
		out = append(out, n)
	}
	return out
}
