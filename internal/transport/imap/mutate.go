package imap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
)

// FlagSeen, FlagFlagged, FlagAnswered, FlagDraft, FlagDeleted are the system
// flags the adapter exposes by name. Any other IMAP flag may be passed verbatim.
const (
	FlagSeen     = "\\Seen"
	FlagFlagged  = "\\Flagged"
	FlagAnswered = "\\Answered"
	FlagDraft    = "\\Draft"
	FlagDeleted  = "\\Deleted"
)

// FolderCreateError is returned by CreateFolder for the specific refusal cases a
// caller may want to distinguish. All of them are also *model.MailboxError.
var (
	// ErrFolderNotEmpty is returned when deleting a folder that still has
	// children and the server refuses the non-empty delete.
	ErrFolderNotEmpty = errors.New("imap: folder is not empty")
)

// CreateFolder creates a new folder at path. The delimiter is the one resolved
// for the scope. It returns a not_found error if the parent hierarchy is absent
// and the server requires it to exist.
func (a *Adapter) CreateFolder(ctx context.Context, path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("imap: a folder path is required")
	}
	if a.conn == nil {
		return wrapErr(ErrNotConnected)
	}
	if err := a.conn.Create(path, nil).Wait(); err != nil {
		return wrapErr(err)
	}
	return nil
}

// RenameFolder renames oldPath to newPath.
func (a *Adapter) RenameFolder(ctx context.Context, oldPath, newPath string) error {
	a.clearSelected()
	oldPath = strings.TrimSpace(oldPath)
	newPath = strings.TrimSpace(newPath)
	if oldPath == "" || newPath == "" {
		return fmt.Errorf("imap: old and new folder paths are required")
	}
	if a.conn == nil {
		return wrapErr(ErrNotConnected)
	}
	if err := a.conn.Rename(oldPath, newPath, nil).Wait(); err != nil {
		return wrapErr(err)
	}
	return nil
}

// DeleteFolder deletes a folder. The IMAP DELETE command removes a folder only
// when it has no children on servers that enforce the rule; the adapter never
// recursively deletes children. When the server refuses because the folder is
// non-empty the returned error wraps ErrFolderNotEmpty.
func (a *Adapter) DeleteFolder(ctx context.Context, path string) error {
	a.clearSelected()
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("imap: a folder path is required")
	}
	if a.conn == nil {
		return wrapErr(ErrNotConnected)
	}
	if err := a.conn.Delete(path).Wait(); err != nil {
		wrapped := wrapErr(err)
		if isNonEmptyFolder(err) {
			return fmt.Errorf("%w: %v", ErrFolderNotEmpty, wrapped)
		}
		return wrapped
	}
	return nil
}

func isNonEmptyFolder(err error) bool {
	var statusErr *imap.Error
	if errors.As(err, &statusErr) {
		text := strings.ToLower(statusErr.Text)
		return strings.Contains(text, "not empty") || strings.Contains(text, "children") || strings.Contains(text, "haschild")
	}
	return false
}

// SetFlags adds or removes flags on a message in a folder. It selects the folder
// read-write, validates UIDVALIDITY, then issues a UID STORE. The updated flags
// are returned when the server reports them (no SILENT).
func (a *Adapter) SetFlags(ctx context.Context, loc Locator, add, remove []string) ([]string, error) {
	var flags []string
	err := a.withSelectRW(ctx, loc.FolderPath, func(data *imap.SelectData) error {
		if err := checkUIDValidity(loc.UIDValidity, data.UIDValidity); err != nil {
			return err
		}
		if len(add) > 0 {
			f, err := a.storeFlags(ctx, loc.UID, imap.StoreFlagsAdd, add)
			if err != nil {
				return err
			}
			flags = f
		}
		if len(remove) > 0 {
			f, err := a.storeFlags(ctx, loc.UID, imap.StoreFlagsDel, remove)
			if err != nil {
				return err
			}
			flags = f
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return flags, nil
}

// MarkSeen is the common case of SetFlags: it sets \Seen.
func (a *Adapter) MarkSeen(ctx context.Context, loc Locator) error {
	_, err := a.SetFlags(ctx, loc, []string{FlagSeen}, nil)
	return err
}

// MarkUnseen removes \Seen.
func (a *Adapter) MarkUnseen(ctx context.Context, loc Locator) error {
	_, err := a.SetFlags(ctx, loc, nil, []string{FlagSeen})
	return err
}

// storeFlags issues one UID STORE and returns the resulting flags if the server
// echoed them.
func (a *Adapter) storeFlags(ctx context.Context, uid uint32, op imap.StoreFlagsOp, flags []string) ([]string, error) {
	store := &imap.StoreFlags{Op: op, Silent: false}
	for _, f := range flags {
		store.Flags = append(store.Flags, toIMAPFlag(f))
	}
	cmd := a.conn.Store(imap.UIDSetNum(imap.UID(uid)), store, nil)
	var out []string
	for {
		if err := ctx.Err(); err != nil {
			_ = cmd.Close()
			return nil, wrapErr(err)
		}
		msg := cmd.Next()
		if msg == nil {
			break
		}
		buf, cerr := msg.Collect()
		if cerr != nil {
			_ = cmd.Close()
			return nil, wrapErr(cerr)
		}
		for _, f := range buf.Flags {
			out = append(out, string(f))
		}
	}
	if err := cmd.Close(); err != nil {
		return nil, wrapErr(err)
	}
	return out, nil
}

// MoveResult reports the destination of a move. DestinationUID is set only when
// the server returned a COPYUID (UIDPLUS/IMAP4rev2); when it is zero the caller
// must re-resolve the destination by Message-ID, because the destination UID was
// not reported. The move itself has succeeded when err is nil.
type MoveResult struct {
	SourceUID      uint32
	DestinationUID uint32
	UIDValidity    uint32
}

// MoveMessage moves a message from its folder to destFolder, preserving the
// destination UID when the server returns COPYUID. It validates the source
// UIDVALIDITY before moving. When the server lacks MOVE but supports UIDPLUS,
// go-imap falls back to COPY + STORE \Deleted + UID EXPUNGE, which is handled by
// the library. When it supports neither, the move is refused: the library's
// fallback would otherwise issue a blanket EXPUNGE that deletes unrelated
// \Deleted messages in the folder.
//
// The move is not atomic across the source check and the command; a concurrent
// deletion can make the source vanish, in which case the library reports an
// error and the caller reconciles.
func (a *Adapter) MoveMessage(ctx context.Context, loc Locator, destFolder string) (MoveResult, error) {
	destFolder = strings.TrimSpace(destFolder)
	if destFolder == "" {
		return MoveResult{}, fmt.Errorf("imap: a destination folder is required")
	}
	// The library's fallback for a server without MOVE is COPY + STORE \Deleted
	// + EXPUNGE, and it uses UID EXPUNGE only when UIDPLUS is present; with
	// neither MOVE nor UIDPLUS it issues a blanket EXPUNGE that would expunge
	// any other message already flagged \Deleted in the folder. Refuse rather
	// than risk unrelated data loss.
	if !a.caps.Move && !a.caps.UIDPlus {
		return MoveResult{}, Unsupported("moving a message requires MOVE or UIDPLUS; a blanket EXPUNGE is not issued")
	}
	res := MoveResult{SourceUID: loc.UID}
	err := a.withSelectRW(ctx, loc.FolderPath, func(data *imap.SelectData) error {
		if err := checkUIDValidity(loc.UIDValidity, data.UIDValidity); err != nil {
			return err
		}
		cmd := a.conn.Move(imap.UIDSetNum(imap.UID(loc.UID)), destFolder)
		moveData, err := cmd.Wait()
		if err != nil {
			return wrapErr(err)
		}
		if moveData != nil {
			res.UIDValidity = moveData.UIDValidity
			res.DestinationUID = firstUID(moveData.DestUIDs)
		}
		// A zero DestinationUID means the server did not report COPYUID; the
		// caller must reconcile by Message-ID rather than assume the UID.
		return nil
	})
	if err != nil {
		return MoveResult{}, err
	}
	return res, nil
}

// ExpungeUIDs removes the messages with the given UIDs from a folder using UID
// EXPUNGE, which requires UIDPLUS (or IMAP4rev2). When the server does not
// support it the adapter returns an unsupported error and never falls back to a
// blanket EXPUNGE, which could delete unrelated messages.
func (a *Adapter) ExpungeUIDs(ctx context.Context, folder string, uidValidity uint32, uids []uint32) error {
	if !a.caps.UIDPlus {
		return Unsupported("UID EXPUNGE requires UIDPLUS or IMAP4rev2; a blanket EXPUNGE is not issued")
	}
	if len(uids) == 0 {
		return nil
	}
	var set imap.UIDSet
	for _, u := range uids {
		set.AddNum(imap.UID(u))
	}
	return a.withSelectRW(ctx, folder, func(data *imap.SelectData) error {
		if err := checkUIDValidity(uidValidity, data.UIDValidity); err != nil {
			return err
		}
		cmd := a.conn.UIDExpunge(set)
		return wrapErr(cmd.Close())
	})
}

// DeleteMessage marks a message \Deleted and expunges it by UID. Both steps are
// UID-targeted: it never issues a blanket EXPUNGE. It requires UIDPLUS (or
// IMAP4rev2); without it the operation is refused before any flag is set, so a
// message is never left flagged \Deleted with the caller told the delete failed.
func (a *Adapter) DeleteMessage(ctx context.Context, loc Locator) error {
	// Refuse before marking the message \Deleted when the server cannot remove
	// it by UID: otherwise the message is left flagged \Deleted (hidden by many
	// clients) while the caller is told the delete failed, so MailMoose and the
	// provider disagree. A blanket EXPUNGE is never issued.
	if !a.caps.UIDPlus {
		return Unsupported("deleting a message requires UIDPLUS or IMAP4rev2; a blanket EXPUNGE is not issued")
	}
	if _, err := a.SetFlags(ctx, loc, []string{FlagDeleted}, nil); err != nil {
		return err
	}
	return a.ExpungeUIDs(ctx, loc.FolderPath, loc.UIDValidity, []uint32{loc.UID})
}

// AppendResult is the outcome of an APPEND. DestinationUID/UIDValidity are set
// only when the server returned an APPENDUID (UIDPLUS/IMAP4rev2). When
// DestinationUID is zero the append succeeded but the assigned UID is unknown;
// the caller must locate the message by its handoff marker or Message-ID, which
// is why the adapter never claims exact-once for an unreported append.
type AppendResult struct {
	DestinationUID uint32
	UIDValidity    uint32
	// Confirmed is true when the server reported an APPENDUID and the assigned
	// UID is known.
	Confirmed bool
}

// AppendReader appends a raw RFC5322 message from r to folder. size must be the
// exact number of bytes r will yield (the IMAP literal length). flags are applied
// to the appended message; drafts typically carry \Draft and \Seen. The append
// streams r through the client with bounded memory.
//
// When the server does not report an APPENDUID, AppendReader returns an
// AppendResult with Confirmed=false and a nil error only if the append command
// itself succeeded; if the command's outcome cannot be determined it returns an
// ambiguous error instead, so the caller reconciles rather than retries blindly.
func (a *Adapter) AppendReader(ctx context.Context, folder string, r io.Reader, size int64, flags []string, date time.Time) (AppendResult, error) {
	a.clearSelected()
	folder = strings.TrimSpace(folder)
	if folder == "" {
		return AppendResult{}, fmt.Errorf("imap: a folder is required")
	}
	if a.conn == nil {
		return AppendResult{}, wrapErr(ErrNotConnected)
	}
	options := &imap.AppendOptions{}
	for _, f := range flags {
		options.Flags = append(options.Flags, toIMAPFlag(f))
	}
	if !date.IsZero() {
		options.Time = date
	}

	cmd := a.conn.Append(folder, size, options)
	if _, err := copyWithContext(ctx, cmd, r); err != nil {
		_ = cmd.Close()
		return AppendResult{}, Ambiguous("append failed while writing the message", err)
	}
	if err := cmd.Close(); err != nil {
		return AppendResult{}, wrapErr(err)
	}
	data, err := cmd.Wait()
	if err != nil {
		return AppendResult{}, wrapErr(err)
	}
	res := AppendResult{}
	if data != nil && data.UID != 0 {
		res.DestinationUID = uint32(data.UID)
		res.UIDValidity = data.UIDValidity
		res.Confirmed = true
	}
	return res, nil
}

// AppendBytes is a convenience wrapper over AppendReader for an in-memory
// message. It sets the literal length from the byte slice.
func (a *Adapter) AppendBytes(ctx context.Context, folder string, raw []byte, flags []string, date time.Time) (AppendResult, error) {
	return a.AppendReader(ctx, folder, bytes.NewReader(raw), int64(len(raw)), flags, date)
}

// FindByMessageID searches folder for a message whose Message-ID header equals
// messageID. It returns the locator when exactly one match is found, an
// ambiguous error when more than one matches, and not_found when none match.
// This is the recovery path used after an ambiguous append or move.
func (a *Adapter) FindByMessageID(ctx context.Context, folder, messageID string) (Locator, error) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return Locator{}, fmt.Errorf("imap: a message-id is required")
	}
	res, err := a.Search(ctx, folder, SearchQuery{MessageID: messageID})
	if err != nil {
		return Locator{}, err
	}
	if len(res.UIDs) == 0 {
		return Locator{}, NotFound("no message with that Message-ID")
	}
	if len(res.UIDs) > 1 {
		return Locator{}, Ambiguous("more than one message has that Message-ID", nil)
	}
	uidValidity, err := a.folderUIDValidity(ctx, folder)
	if err != nil {
		return Locator{}, err
	}
	return Locator{FolderPath: folder, UIDValidity: uidValidity, UID: res.UIDs[0], MessageID: messageID}, nil
}

// FindByHeader searches folder for messages whose header key equals value and
// returns their locators. It is the general recovery primitive behind
// FindByMessageID for a caller-supplied handoff marker.
func (a *Adapter) FindByHeader(ctx context.Context, folder, key, value string) ([]Locator, error) {
	res, err := a.Search(ctx, folder, SearchQuery{Header: []HeaderMatch{{Key: key, Value: value}}})
	if err != nil {
		return nil, err
	}
	if len(res.UIDs) == 0 {
		return nil, nil
	}
	uidValidity, err := a.folderUIDValidity(ctx, folder)
	if err != nil {
		return nil, err
	}
	out := make([]Locator, 0, len(res.UIDs))
	for _, uid := range res.UIDs {
		out = append(out, Locator{FolderPath: folder, UIDValidity: uidValidity, UID: uid})
	}
	return out, nil
}

func firstUID(set imap.NumSet) uint32 {
	if set == nil {
		return 0
	}
	switch s := set.(type) {
	case imap.UIDSet:
		nums, ok := s.Nums()
		if !ok || len(nums) == 0 {
			return 0
		}
		return uint32(nums[0])
	case imap.SeqSet:
		nums, ok := s.Nums()
		if !ok || len(nums) == 0 {
			return 0
		}
		return nums[0]
	default:
		return 0
	}
}
