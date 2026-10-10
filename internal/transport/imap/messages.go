package imap

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// MaxSearchResults bounds a live search to a sane page size so a broad query
// cannot exhaust memory. Callers page with the returned cursor when the limit
// is reached.
const MaxSearchResults = 500

// HeaderFields are the RFC5322 headers the adapter requests for a header fetch.
// They are the minimal set needed to thread and summarise a message; the full
// header is available from the raw MIME fetch.
var HeaderFields = []string{
	"From", "To", "Cc", "Bcc", "Subject", "Date", "Message-ID",
	"In-Reply-To", "References", "Content-Type",
}

// Locator names a message on the remote server: its folder, the folder's
// UIDVALIDITY, and its UID. It is the adapter's own locator type (mapping 1:1 to
// model.RemoteLocator at the app boundary) so the protocol package carries no
// store dependency.
//
// UIDValidity scopes UID: when the live UIDVALIDITY differs, the UID is stale
// and MessageID must be used to re-resolve the message.
type Locator struct {
	FolderPath  string
	UIDValidity uint32
	UID         uint32
	MessageID   string
}

// MessageHeader is the header/thread metadata of one remote message.
type MessageHeader struct {
	FolderPath   string
	UIDValidity  uint32
	UID          uint32
	SeqNum       uint32
	MessageID    string
	InReplyTo    string
	References   []string
	Subject      string
	From         Address
	To           []string
	CC           []string
	BCC          []string
	Date         time.Time
	InternalDate time.Time
	Size         int64
	Flags        []string
	Read         bool
	Flagged      bool
	Answered     bool
	Draft        bool
	HasAttach    bool
	// BodyStructure is the parsed BODYSTRUCTURE, when it was requested.
	BodyStructure *BodyStructure
}

// Address is a minimal, provider-neutral email address.
type Address struct {
	Name    string
	Address string
}

// BodyStructure is a lightweight, serialisable description of a message's MIME
// structure, derived from the IMAP BODYSTRUCTURE. It enumerates attachments
// without downloading them.
type BodyStructure struct {
	MediaType   string
	Params      map[string]string
	Disposition string
	Filename    string
	Size        uint32
	Parts       []BodyStructure
}

// SearchQuery is a provider-neutral search request. All populated fields are
// ANDed. Unset fields are ignored.
type SearchQuery struct {
	UIDs       []uint32
	Since      time.Time
	Before     time.Time
	SentSince  time.Time
	SentBefore time.Time
	From       string
	To         string
	Cc         string
	Bcc        string
	Subject    string
	Body       string
	Text       string
	// MessageID matches the Message-ID header (angle brackets are stripped).
	MessageID string
	Header    []HeaderMatch
	Flags     []string
	NotFlags  []string
	// Seen, when non-nil, forces \Seen presence/absence.
	Seen    *bool
	Larger  int64
	Smaller int64
	// Limit bounds the number of returned UIDs. Zero uses MaxSearchResults.
	Limit int
	// BeforeUID, when non-zero, restricts the search to UIDs strictly less than
	// it. Combined with descending ordering it is the pagination cursor for a
	// newest-first search: the next page resumes from the lowest UID already
	// returned.
	BeforeUID uint32
	// AfterUID, when non-zero, restricts the search to UIDs strictly greater
	// than it (the IMAP range "(AfterUID+1):*"). With the default ascending
	// ordering it is the forward pagination cursor: a bounded page returns the
	// OLDEST matching UIDs above the cursor and NextCursor advances past them, so
	// a new-arrival detector never has to load the whole folder to find them.
	AfterUID uint32
	// NewestFirst orders results newest-first (descending UID) and, when the
	// result set exceeds Limit, truncates from the newest end so a bounded search
	// returns the most recent matches rather than the oldest. NextCursor then
	// carries the lowest returned UID to resume with BeforeUID.
	NewestFirst bool
	// NoLimit requests the complete matching UID set (no truncation). It is the
	// reconcile path's way to obtain a folder's full UID set so removals can be
	// pruned against a complete snapshot. The caller must bound the result itself
	// (a UID set is only four bytes per message).
	NoLimit bool
}

// HeaderMatch is an arbitrary header search term.
type HeaderMatch struct {
	Key   string
	Value string
}

// SearchResult is the outcome of a live search. UIDs are ascending. Completeness
// reports whether the result set could be enumerated fully. NextCursor is the
// UID to resume from when the result was truncated at the limit.
type SearchResult struct {
	UIDs         []uint32
	Completeness ListCompleteness
	NextCursor   uint32
}

// ListCompleteness mirrors model.ListCompleteness so the protocol package stays
// model-independent.
type ListCompleteness string

const (
	CompletenessComplete ListCompleteness = "complete"
	CompletenessPartial  ListCompleteness = "partial"
	CompletenessUnknown  ListCompleteness = "unknown"
)

// Search runs a live UID SEARCH in the given folder. It EXAMINEs the folder
// first so a missing folder is reported as not_found, and holds the selection
// for the whole search.
func (a *Adapter) Search(ctx context.Context, folder string, q SearchQuery) (SearchResult, error) {
	var res SearchResult
	err := a.withExamine(ctx, folder, func(_ *imap.SelectData) error {
		cmd := a.conn.UIDSearch(searchCriteria(q), nil)
		data, err := cmd.Wait()
		if err != nil {
			return wrapErr(err)
		}
		var uids []uint32
		if all, ok := data.All.(imap.UIDSet); ok {
			nums, ok := all.Nums()
			if !ok {
				return Ambiguous("search returned a dynamic UID set", nil)
			}
			uids = make([]uint32, 0, len(nums))
			for _, u := range nums {
				uids = append(uids, uint32(u))
			}
		}
		sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })

		res = SearchResult{Completeness: CompletenessComplete}
		if q.NoLimit {
			// The complete UID set: no truncation, so the caller can prune against
			// a full snapshot.
			if uids == nil {
				uids = []uint32{}
			}
			res.UIDs = uids
			return nil
		}
		limit := q.Limit
		if limit <= 0 {
			limit = MaxSearchResults
		}
		if len(uids) > limit {
			if q.NewestFirst {
				// Keep the newest `limit` and resume below the lowest returned UID.
				uids = uids[len(uids)-limit:]
				res.NextCursor = uids[0]
			} else {
				res.NextCursor = uids[limit]
				uids = uids[:limit]
			}
			res.Completeness = CompletenessPartial
		} else if q.NewestFirst && limit > 0 && len(uids) == limit {
			// A full page may have more older matches; offer a resume cursor.
			res.NextCursor = uids[0]
		}
		if q.NewestFirst {
			for i, j := 0, len(uids)-1; i < j; i, j = i+1, j-1 {
				uids[i], uids[j] = uids[j], uids[i]
			}
		}
		if uids == nil {
			uids = []uint32{}
		}
		res.UIDs = uids
		return nil
	})
	if err != nil {
		return SearchResult{}, err
	}
	return res, nil
}

// searchCriteria translates a SearchQuery into an IMAP search criteria.
func searchCriteria(q SearchQuery) *imap.SearchCriteria {
	c := &imap.SearchCriteria{}
	if len(q.UIDs) > 0 {
		var set imap.UIDSet
		for _, u := range q.UIDs {
			set = append(set, imap.UIDRange{Start: imap.UID(u), Stop: imap.UID(u)})
		}
		c.UID = append(c.UID, set)
	}
	if q.BeforeUID > 1 {
		c.UID = append(c.UID, imap.UIDSet{imap.UIDRange{Start: 1, Stop: imap.UID(q.BeforeUID - 1)}})
	}
	if q.AfterUID > 0 {
		// "(AfterUID+1):*" — Stop 0 is the IMAP "*". A bounded ascending search
		// then returns the oldest matching UIDs above the cursor.
		c.UID = append(c.UID, imap.UIDSet{imap.UIDRange{Start: imap.UID(q.AfterUID + 1)}})
	}
	c.Since = q.Since
	c.Before = q.Before
	c.SentSince = q.SentSince
	c.SentBefore = q.SentBefore
	addHeader := func(key, value string) {
		if value != "" {
			c.Header = append(c.Header, imap.SearchCriteriaHeaderField{Key: key, Value: value})
		}
	}
	addHeader("From", q.From)
	addHeader("To", q.To)
	addHeader("Cc", q.Cc)
	addHeader("Bcc", q.Bcc)
	addHeader("Subject", q.Subject)
	if q.MessageID != "" {
		addHeader("Message-ID", strings.Trim(q.MessageID, "<>"))
	}
	for _, h := range q.Header {
		if strings.TrimSpace(h.Key) != "" {
			c.Header = append(c.Header, imap.SearchCriteriaHeaderField{Key: h.Key, Value: h.Value})
		}
	}
	if q.Body != "" {
		c.Body = append(c.Body, q.Body)
	}
	if q.Text != "" {
		c.Text = append(c.Text, q.Text)
	}
	for _, f := range q.Flags {
		if flag := toIMAPFlag(f); flag != "" {
			c.Flag = append(c.Flag, flag)
		}
	}
	for _, f := range q.NotFlags {
		if flag := toIMAPFlag(f); flag != "" {
			c.NotFlag = append(c.NotFlag, flag)
		}
	}
	if q.Seen != nil {
		if *q.Seen {
			c.Flag = append(c.Flag, imap.FlagSeen)
		} else {
			c.NotFlag = append(c.NotFlag, imap.FlagSeen)
		}
	}
	c.Larger = q.Larger
	c.Smaller = q.Smaller
	return c
}

// ListHeaders fetches header metadata for the given UIDs in a folder. When uids
// is empty it fetches every message in the folder (bounded by max). It requests
// the flags and BODYSTRUCTURE but never a body section, so listing never marks a
// message seen. The live UIDVALIDITY is returned alongside.
func (a *Adapter) ListHeaders(ctx context.Context, folder string, uids []uint32, max int) ([]MessageHeader, uint32, error) {
	options := &imap.FetchOptions{
		Envelope:      true,
		Flags:         true,
		InternalDate:  true,
		RFC822Size:    true,
		UID:           true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
	}

	var numSet imap.NumSet
	if len(uids) > 0 {
		var set imap.UIDSet
		for _, u := range uids {
			set = append(set, imap.UIDRange{Start: imap.UID(u), Stop: imap.UID(u)})
		}
		numSet = set
	} else {
		var all imap.SeqSet
		all.AddRange(1, 0) // 1:* — all messages
		numSet = all
	}

	out := []MessageHeader{}
	var uidValidity uint32
	err := a.withExamine(ctx, folder, func(data *imap.SelectData) error {
		uidValidity = data.UIDValidity
		cmd := a.conn.Fetch(numSet, options)
		for {
			msg := cmd.Next()
			if msg == nil {
				break
			}
			item, cerr := msg.Collect()
			if cerr != nil {
				_ = cmd.Close()
				return wrapErr(cerr)
			}
			out = append(out, headerFromBuffer(folder, uidValidity, item))
			if max > 0 && len(out) >= max {
				break
			}
		}
		return wrapErr(cmd.Close())
	})
	if err != nil {
		return nil, 0, err
	}
	return out, uidValidity, nil
}

// FetchHeader fetches one message's header metadata. A missing UID yields a
// not_found error and a stale UIDVALIDITY yields a conflict error. It never
// marks the message seen.
func (a *Adapter) FetchHeader(ctx context.Context, loc Locator) (MessageHeader, error) {
	options := &imap.FetchOptions{
		Envelope:      true,
		Flags:         true,
		InternalDate:  true,
		RFC822Size:    true,
		UID:           true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
	}

	var out MessageHeader
	err := a.withExamine(ctx, loc.FolderPath, func(data *imap.SelectData) error {
		if err := checkUIDValidity(loc.UIDValidity, data.UIDValidity); err != nil {
			return err
		}
		cmd := a.conn.Fetch(imap.UIDSetNum(imap.UID(loc.UID)), options)
		msg := cmd.Next()
		if msg == nil {
			_ = cmd.Close()
			return NotFound("")
		}
		item, cerr := msg.Collect()
		if cerr != nil {
			_ = cmd.Close()
			return wrapErr(cerr)
		}
		if err := cmd.Close(); err != nil {
			return wrapErr(err)
		}
		if item.UID == 0 {
			return NotFound("")
		}
		out = headerFromBuffer(loc.FolderPath, data.UIDValidity, item)
		return nil
	})
	if err != nil {
		return MessageHeader{}, err
	}
	return out, nil
}

// headerFromBuffer maps a FETCH buffer into a MessageHeader.
func headerFromBuffer(folder string, uidValidity uint32, buf *imapclient.FetchMessageBuffer) MessageHeader {
	h := MessageHeader{
		FolderPath:   folder,
		UIDValidity:  uidValidity,
		UID:          uint32(buf.UID),
		SeqNum:       buf.SeqNum,
		InternalDate: buf.InternalDate,
		Size:         buf.RFC822Size,
	}
	if buf.Envelope != nil {
		h.Subject = buf.Envelope.Subject
		h.MessageID = buf.Envelope.MessageID
		h.InReplyTo = firstString(buf.Envelope.InReplyTo)
		// The IMAP ENVELOPE does not expose the References header, so References
		// is left empty rather than mislabelled as In-Reply-To. Callers that need
		// the full reference chain fetch and parse the raw message.
		h.Date = buf.Envelope.Date
		if len(buf.Envelope.From) > 0 {
			h.From = addressFromIMAP(buf.Envelope.From[0])
		}
		h.To = addressesFromIMAP(buf.Envelope.To)
		h.CC = addressesFromIMAP(buf.Envelope.Cc)
		h.BCC = addressesFromIMAP(buf.Envelope.Bcc)
	}
	for _, f := range buf.Flags {
		h.Flags = append(h.Flags, string(f))
		switch f {
		case imap.FlagSeen:
			h.Read = true
		case imap.FlagFlagged:
			h.Flagged = true
		case imap.FlagAnswered:
			h.Answered = true
		case imap.FlagDraft:
			h.Draft = true
		}
	}
	if buf.BodyStructure != nil {
		bs := bodyStructureFromIMAP(buf.BodyStructure)
		h.BodyStructure = &bs
		h.HasAttach = bs.HasAttachment()
	}
	return h
}

// HasAttachment reports whether the structure contains an attachment part.
func (bs BodyStructure) HasAttachment() bool {
	if strings.EqualFold(bs.Disposition, "attachment") {
		return true
	}
	for i := range bs.Parts {
		if bs.Parts[i].HasAttachment() {
			return true
		}
	}
	return false
}

// bodyStructureFromIMAP converts the library's body structure into the adapter's
// serialisable form. A single-part message has no children; a multipart message
// keeps its direct children as Parts.
func bodyStructureFromIMAP(bs imap.BodyStructure) BodyStructure {
	switch root := bs.(type) {
	case *imap.BodyStructureSinglePart:
		node := BodyStructure{
			MediaType: root.MediaType(),
			Params:    root.Params,
			Size:      root.Size,
			Filename:  root.Filename(),
		}
		if disp := root.Disposition(); disp != nil {
			node.Disposition = disp.Value
		}
		return node
	case *imap.BodyStructureMultiPart:
		out := BodyStructure{MediaType: root.MediaType()}
		for _, child := range root.Children {
			out.Parts = append(out.Parts, bodyStructureFromIMAP(child))
		}
		return out
	default:
		return BodyStructure{}
	}
}

// FetchRawMIME streams the full raw RFC5322 message identified by loc into w
// using BODY.PEEK[] so \Seen is never set. The literal is copied in chunks and
// never fully buffered.
func (a *Adapter) FetchRawMIME(ctx context.Context, loc Locator, w io.Writer) error {
	return a.streamSection(ctx, loc, imap.FetchItemBodySection{Peek: true}, w)
}

// FetchBodyPart streams a specific MIME part (a text body or an attachment)
// using BODY.PEEK[part] so \Seen is not set. An empty part selects the whole
// message. Attachment parts are streamed with bounded memory.
func (a *Adapter) FetchBodyPart(ctx context.Context, loc Locator, part []int, w io.Writer) error {
	return a.streamSection(ctx, loc, imap.FetchItemBodySection{Part: part, Peek: true}, w)
}

// streamSection is the common BODY.PEEK[...] streaming fetch. It selects the
// folder read-only, validates UIDVALIDITY and streams the first matching literal
// to w, honouring context cancellation. The selection is held for the duration
// of the stream, so a large attachment fetch cannot be overtaken by a concurrent
// folder switch.
func (a *Adapter) streamSection(ctx context.Context, loc Locator, section imap.FetchItemBodySection, w io.Writer) error {
	if loc.FolderPath == "" || loc.UID == 0 {
		return fmt.Errorf("imap: a folder path and uid are required")
	}
	options := &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{&section},
	}
	return a.withExamine(ctx, loc.FolderPath, func(data *imap.SelectData) error {
		if err := checkUIDValidity(loc.UIDValidity, data.UIDValidity); err != nil {
			return err
		}
		cmd := a.conn.Fetch(imap.UIDSetNum(imap.UID(loc.UID)), options)
		defer cmd.Close()

		found := false
		for {
			if err := ctx.Err(); err != nil {
				return wrapErr(err)
			}
			msg := cmd.Next()
			if msg == nil {
				break
			}
			for {
				if err := ctx.Err(); err != nil {
					return wrapErr(err)
				}
				item := msg.Next()
				if item == nil {
					break
				}
				if bodySection, ok := item.(imapclient.FetchItemDataBodySection); ok {
					if bodySection.Literal == nil {
						continue
					}
					found = true
					if _, err := copyWithContext(ctx, w, bodySection.Literal); err != nil {
						return wrapErr(err)
					}
				}
			}
		}
		if err := cmd.Close(); err != nil {
			return wrapErr(err)
		}
		if !found {
			return NotFound("")
		}
		return nil
	})
}

// copyWithContext copies src to dst, aborting promptly when ctx is cancelled.
// The IMAP literal reader blocks on the socket, so cancellation is observed
// between chunks.
func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 32*1024)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return written, werr
			}
			written += int64(n)
		}
		if rerr == io.EOF {
			return written, nil
		}
		if rerr != nil {
			return written, rerr
		}
	}
}

// folderUIDValidity returns the folder's current UIDVALIDITY.
func (a *Adapter) folderUIDValidity(ctx context.Context, folder string) (uint32, error) {
	data, err := a.examine(ctx, folder)
	if err != nil {
		return 0, err
	}
	return data.UIDValidity, nil
}

// checkUIDValidity compares a stored locator's UIDVALIDITY against the live one.
// Zero means the caller had no prior value; the current one is accepted.
func checkUIDValidity(expected, actual uint32) error {
	if expected != 0 && expected != actual {
		return conflictError("uid_validity_changed", fmt.Sprintf("the remote folder was reset (stored %d, live %d); re-resolve by Message-ID", expected, actual))
	}
	return nil
}

func firstString(l []string) string {
	if len(l) == 0 {
		return ""
	}
	return l[0]
}

func addressFromIMAP(a imap.Address) Address {
	return Address{Name: a.Name, Address: a.Addr()}
}

func addressesFromIMAP(l []imap.Address) []string {
	out := make([]string, 0, len(l))
	for i := range l {
		if addr := l[i].Addr(); addr != "" {
			out = append(out, addr)
		}
	}
	return out
}

func toIMAPFlag(f string) imap.Flag {
	f = strings.TrimSpace(f)
	if f == "" {
		return ""
	}
	if strings.HasPrefix(f, "\\") || strings.HasPrefix(f, "$") {
		return imap.Flag(f)
	}
	switch strings.ToLower(f) {
	case "seen", "read":
		return imap.FlagSeen
	case "flagged":
		return imap.FlagFlagged
	case "answered":
		return imap.FlagAnswered
	case "draft":
		return imap.FlagDraft
	case "deleted":
		return imap.FlagDeleted
	default:
		return imap.Flag(f)
	}
}
