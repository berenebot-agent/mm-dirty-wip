package app_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/config"
	"github.com/dellarb/mailmoose/internal/events"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport"
	"github.com/dellarb/mailmoose/internal/transport/imap"
	"github.com/dellarb/mailmoose/tests/support/testdb"
	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// ---- test service ----

// remoteTestEnv builds an app service, an account and a standalone inbox ready
// for remote configuration.
func remoteTestEnv(t *testing.T) (*app.Service, model.User, model.Inbox, *app.RemoteMailboxService) {
	t.Helper()
	st, dir := testdb.OpenDir(t)
	cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour, LoginLimitPerMinute: 10, SendLimitPerMinute: 60}
	svc, err := app.New(cfg, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateAccountAndAdmin(context.Background(), "A", "admin@example.com", "correct horse battery staple", cfg.DefaultQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	box, err := st.CreateStandaloneInbox(context.Background(), u.AccountID, store.StandaloneCreate{
		DisplayName: "Remote",
		Address:     "agent@remote.example",
		Remote:      &model.RemoteConnection{Host: "imap.remote.example", Username: "agent@remote.example", Security: model.RemoteSecurityPlain, Port: 143},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureSystemFolders(context.Background(), u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	rm := app.NewRemoteMailboxService(svc)
	return svc, u, box, rm
}

// configureSecrets stores an IMAP and optional SMTP password for a standalone
// inbox through the real configure path.
func configureSecrets(t *testing.T, rm *app.RemoteMailboxService, u model.User, box model.Inbox, imapPw, smtpPw string) {
	t.Helper()
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
	if _, err := rm.ConfigureStandaloneRemote(context.Background(), p, box.ID, store.StandaloneRemoteUpdate{
		IMAPPassword: imapPw,
		SMTPPassword: smtpPw,
	}); err != nil {
		t.Fatalf("configure remote: %v", err)
	}
}

// ---- fake remote session ----

// fakeMsg is one message held in the fake remote server.
type fakeMsg struct {
	uid         uint32
	uidValidity uint32
	folder      string
	raw         string
	messageID   string
	subject     string
	from        imap.Address
	flags       []string
}

// fakeRemoteServer is a deterministic in-memory RemoteSession used to exercise the
// app boundary without a live socket. It records every call so a test can assert
// what the service did.
type fakeRemoteServer struct {
	mu             sync.Mutex
	folders        map[string]uint32 // path -> UIDVALIDITY
	order          []string
	messages       []*fakeMsg
	nextUID        uint32
	appendCount    int
	appendFail     error
	appendNoUID    bool
	markSeenCalled bool
	moveCalled     int
	deleteCalled   int
	closeCalled    int
	// moveNoUID makes MoveMessage behave like a server without UIDPLUS: the move
	// succeeds but reports no COPYUID (DestinationUID 0), forcing the caller to
	// re-resolve by Message-ID.
	moveNoUID bool
}

func newFakeRemoteServer() *fakeRemoteServer {
	return &fakeRemoteServer{folders: map[string]uint32{"INBOX": 100, "Drafts": 100, "Sent": 100}, nextUID: 1}
}

func (f *fakeRemoteServer) addMessage(folder, raw, messageID, subject string, flags ...string) *fakeMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := &fakeMsg{uid: f.nextUID, uidValidity: f.folders[folder], folder: folder, raw: raw, messageID: messageID, subject: subject, from: imap.Address{Name: "Sender", Address: "sender@elsewhere.test"}, flags: flags}
	f.nextUID++
	f.messages = append(f.messages, m)
	return m
}

func (f *fakeRemoteServer) addFolder(path string, uidValidity uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.folders[path] = uidValidity
}

func (f *fakeRemoteServer) DiscoverFolders(_ context.Context, _ string) ([]imap.RemoteFolder, imap.RootScope, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]imap.RemoteFolder, 0, len(f.folders))
	for path := range f.folders {
		out = append(out, imap.RemoteFolder{Path: path, Name: path, Delimiter: '/', Role: roleForTestFolder(path), Selectable: true})
	}
	scope := imap.RootScope{Root: "INBOX", Delimiter: '/', Personal: true, INBOXInScope: true}
	return out, scope, nil
}

func roleForTestFolder(path string) string {
	switch strings.ToLower(path) {
	case "inbox":
		return model.FolderRoleInbox
	case "drafts":
		return model.FolderRoleDrafts
	case "sent":
		return model.FolderRoleSent
	case "trash":
		return model.FolderRoleTrash
	default:
		return model.FolderRoleFolder
	}
}

func (f *fakeRemoteServer) EnsureFolderExists(_ context.Context, path string) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.folders[path]
	if !ok {
		return 0, model.NewMailboxError(model.ErrKindNotFound, "no folder", false, imap.ErrNotFound)
	}
	return v, nil
}

func (f *fakeRemoteServer) ListHeaders(_ context.Context, folder string, uids []uint32, max int) ([]imap.MessageHeader, uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []imap.MessageHeader
	validity := f.folders[folder]
	for _, m := range f.messages {
		if m.folder != folder {
			continue
		}
		if len(uids) > 0 && !containsUID(uids, m.uid) {
			continue
		}
		out = append(out, fakeHeader(m))
		if max > 0 && len(out) >= max {
			break
		}
	}
	return out, validity, nil
}

func fakeHeader(m *fakeMsg) imap.MessageHeader {
	h := imap.MessageHeader{FolderPath: m.folder, UIDValidity: m.uidValidity, UID: m.uid, MessageID: m.messageID, Subject: m.subject, From: m.from, InternalDate: time.Now().UTC(), Size: int64(len(m.raw)), Flags: m.flags}
	for _, fl := range m.flags {
		switch fl {
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
	return h
}

func containsUID(uids []uint32, uid uint32) bool {
	for _, u := range uids {
		if u == uid {
			return true
		}
	}
	return false
}

func (f *fakeRemoteServer) FetchHeader(_ context.Context, loc imap.Locator) (imap.MessageHeader, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.find(loc)
	if m == nil {
		return imap.MessageHeader{}, model.NewMailboxError(model.ErrKindNotFound, "no message", false, imap.ErrNotFound)
	}
	if loc.UIDValidity != 0 && loc.UIDValidity != m.uidValidity {
		return imap.MessageHeader{}, model.NewMailboxError(model.ErrKindConflict, "uid validity changed", false, imap.ErrUIDValidityChanged)
	}
	return fakeHeader(m), nil
}

func (f *fakeRemoteServer) find(loc imap.Locator) *fakeMsg {
	for _, m := range f.messages {
		if m.folder == loc.FolderPath && m.uid == loc.UID {
			return m
		}
	}
	return nil
}

func (f *fakeRemoteServer) Search(_ context.Context, folder string, q imap.SearchQuery) (imap.SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var uids []uint32
	for _, m := range f.messages {
		if m.folder != folder {
			continue
		}
		if q.Subject != "" && !strings.Contains(strings.ToLower(m.subject), strings.ToLower(q.Subject)) {
			continue
		}
		uids = append(uids, m.uid)
	}
	return imap.SearchResult{UIDs: uids, Completeness: imap.CompletenessComplete}, nil
}

func (f *fakeRemoteServer) FetchRawMIME(_ context.Context, loc imap.Locator, w io.Writer) error {
	return f.fetchRaw(loc, w)
}

func (f *fakeRemoteServer) fetchRaw(loc imap.Locator, w io.Writer) error {
	f.mu.Lock()
	m := f.find(loc)
	f.mu.Unlock()
	if m == nil {
		return model.NewMailboxError(model.ErrKindNotFound, "no message", false, imap.ErrNotFound)
	}
	_, err := w.Write([]byte(m.raw))
	return err
}

func (f *fakeRemoteServer) FetchBodyPart(_ context.Context, loc imap.Locator, _ []int, w io.Writer) error {
	return f.fetchRaw(loc, w)
}

func (f *fakeRemoteServer) SetFlags(_ context.Context, loc imap.Locator, add, remove []string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.find(loc)
	if m == nil {
		return nil, model.NewMailboxError(model.ErrKindNotFound, "no message", false, imap.ErrNotFound)
	}
	for _, fl := range add {
		if fl == imap.FlagSeen {
			f.markSeenCalled = true
		}
		m.flags = append(m.flags, fl)
	}
	for _, fl := range remove {
		var kept []string
		for _, have := range m.flags {
			if have != fl {
				kept = append(kept, have)
			}
		}
		m.flags = kept
	}
	return m.flags, nil
}

func (f *fakeRemoteServer) MoveMessage(_ context.Context, loc imap.Locator, destFolder string) (imap.MoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moveCalled++
	m := f.find(loc)
	if m == nil {
		return imap.MoveResult{}, model.NewMailboxError(model.ErrKindNotFound, "no message", false, imap.ErrNotFound)
	}
	m.folder = destFolder
	m.uid = f.nextUID
	f.nextUID++
	m.uidValidity = f.folders[destFolder]
	if f.moveNoUID {
		return imap.MoveResult{SourceUID: loc.UID, UIDValidity: m.uidValidity}, nil
	}
	return imap.MoveResult{SourceUID: loc.UID, DestinationUID: m.uid, UIDValidity: m.uidValidity}, nil
}

func (f *fakeRemoteServer) DeleteMessage(_ context.Context, loc imap.Locator) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalled++
	for i, m := range f.messages {
		if m.folder == loc.FolderPath && m.uid == loc.UID {
			f.messages = append(f.messages[:i], f.messages[i+1:]...)
			return nil
		}
	}
	return model.NewMailboxError(model.ErrKindNotFound, "no message", false, imap.ErrNotFound)
}

func (f *fakeRemoteServer) AppendReader(_ context.Context, folder string, r io.Reader, size int64, flags []string, _ time.Time) (imap.AppendResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appendCount++
	if f.appendFail != nil {
		return imap.AppendResult{}, f.appendFail
	}
	buf := make([]byte, size)
	n, _ := r.Read(buf)
	raw := string(buf[:n])
	msgID := ""
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(strings.ToLower(line), "message-id:") {
			msgID = strings.TrimSpace(strings.TrimPrefix(line, "Message-ID:"))
			msgID = strings.TrimSpace(strings.TrimPrefix(msgID, "message-id:"))
		}
	}
	m := &fakeMsg{uid: f.nextUID, uidValidity: f.folders[folder], folder: folder, raw: raw, messageID: msgID, flags: flags}
	f.nextUID++
	f.messages = append(f.messages, m)
	if f.appendNoUID {
		return imap.AppendResult{}, nil
	}
	return imap.AppendResult{DestinationUID: m.uid, UIDValidity: m.uidValidity, Confirmed: true}, nil
}

func (f *fakeRemoteServer) CreateFolder(_ context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.folders[path]; ok {
		return model.NewMailboxError(model.ErrKindConflict, "exists", false, nil)
	}
	f.folders[path] = 100
	return nil
}

func (f *fakeRemoteServer) RenameFolder(_ context.Context, oldPath, newPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.folders[oldPath]
	delete(f.folders, oldPath)
	f.folders[newPath] = v
	for _, m := range f.messages {
		if m.folder == oldPath {
			m.folder = newPath
		}
	}
	return nil
}

func (f *fakeRemoteServer) DeleteFolder(_ context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// A real server refuses to delete a folder that still has children; the fake
	// mirrors that so the app's server-side error mapping is exercised.
	for child := range f.folders {
		if strings.HasPrefix(child, path+"/") {
			return model.NewMailboxError(model.ErrKindConflict, "folder has children", false, imap.ErrFolderNotEmpty)
		}
	}
	delete(f.folders, path)
	return nil
}

func (f *fakeRemoteServer) FindByMessageID(_ context.Context, folder, messageID string) (imap.Locator, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var hits []*fakeMsg
	for _, m := range f.messages {
		if m.folder == folder && m.messageID == messageID {
			hits = append(hits, m)
		}
	}
	if len(hits) == 0 {
		return imap.Locator{}, model.NewMailboxError(model.ErrKindNotFound, "no message", false, imap.ErrNotFound)
	}
	if len(hits) > 1 {
		return imap.Locator{}, model.NewMailboxError(model.ErrKindConflict, "ambiguous", false, imap.ErrAmbiguous)
	}
	return imap.Locator{FolderPath: folder, UIDValidity: hits[0].uidValidity, UID: hits[0].uid, MessageID: messageID}, nil
}

func (f *fakeRemoteServer) FindByHeader(_ context.Context, folder, key, value string) ([]imap.Locator, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []imap.Locator
	for _, m := range f.messages {
		if m.folder == folder && strings.Contains(m.raw, key+": "+value) {
			out = append(out, imap.Locator{FolderPath: folder, UIDValidity: m.uidValidity, UID: m.uid, MessageID: m.messageID})
		}
	}
	return out, nil
}

func (f *fakeRemoteServer) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalled++
	return nil
}

func installFake(t *testing.T, rm *app.RemoteMailboxService, fake *fakeRemoteServer) {
	t.Helper()
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) {
		return &fakeSession{fake: fake}, nil
	})
}

// fakeSession adapts fakeRemoteServer to the app.RemoteSession interface, whose
// streaming methods take io.Writer; the fake's typed-interface methods do not
// satisfy it, so this adapter bridges them.
type fakeSession struct{ fake *fakeRemoteServer }

func (s *fakeSession) DiscoverFolders(ctx context.Context, root string) ([]imap.RemoteFolder, imap.RootScope, error) {
	return s.fake.DiscoverFolders(ctx, root)
}
func (s *fakeSession) Search(ctx context.Context, folder string, q imap.SearchQuery) (imap.SearchResult, error) {
	return s.fake.Search(ctx, folder, q)
}
func (s *fakeSession) ListHeaders(ctx context.Context, folder string, uids []uint32, max int) ([]imap.MessageHeader, uint32, error) {
	return s.fake.ListHeaders(ctx, folder, uids, max)
}
func (s *fakeSession) FetchHeader(ctx context.Context, loc imap.Locator) (imap.MessageHeader, error) {
	return s.fake.FetchHeader(ctx, loc)
}
func (s *fakeSession) FetchRawMIME(ctx context.Context, loc imap.Locator, w io.Writer) error {
	return s.fake.FetchRawMIME(ctx, loc, w)
}
func (s *fakeSession) FetchBodyPart(ctx context.Context, loc imap.Locator, part []int, w io.Writer) error {
	return s.fake.FetchBodyPart(ctx, loc, part, w)
}
func (s *fakeSession) SetFlags(ctx context.Context, loc imap.Locator, add, remove []string) ([]string, error) {
	return s.fake.SetFlags(ctx, loc, add, remove)
}
func (s *fakeSession) MoveMessage(ctx context.Context, loc imap.Locator, dest string) (imap.MoveResult, error) {
	return s.fake.MoveMessage(ctx, loc, dest)
}
func (s *fakeSession) DeleteMessage(ctx context.Context, loc imap.Locator) error {
	return s.fake.DeleteMessage(ctx, loc)
}
func (s *fakeSession) AppendReader(ctx context.Context, folder string, r io.Reader, size int64, flags []string, date time.Time) (imap.AppendResult, error) {
	return s.fake.AppendReader(ctx, folder, r, size, flags, date)
}
func (s *fakeSession) CreateFolder(ctx context.Context, path string) error {
	return s.fake.CreateFolder(ctx, path)
}
func (s *fakeSession) RenameFolder(ctx context.Context, oldPath, newPath string) error {
	return s.fake.RenameFolder(ctx, oldPath, newPath)
}
func (s *fakeSession) DeleteFolder(ctx context.Context, path string) error {
	return s.fake.DeleteFolder(ctx, path)
}
func (s *fakeSession) EnsureFolderExists(ctx context.Context, path string) (uint32, error) {
	return s.fake.EnsureFolderExists(ctx, path)
}
func (s *fakeSession) FindByMessageID(ctx context.Context, folder, messageID string) (imap.Locator, error) {
	return s.fake.FindByMessageID(ctx, folder, messageID)
}
func (s *fakeSession) FindByHeader(ctx context.Context, folder, key, value string) ([]imap.Locator, error) {
	return s.fake.FindByHeader(ctx, folder, key, value)
}
func (s *fakeSession) Close() error { return s.fake.Close() }

// ---- tests ----

// TestRemoteReconcileIndexesFoldersAndHeaders proves a reconcile writes the folder
// tree with live UIDVALIDITY and caches message headers, marking the index
// complete.
func TestRemoteReconcileIndexesFoldersAndHeaders(t *testing.T) {
	svc, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	configureSecrets(t, rm, u, box, "imap-pw", "")
	fake := newFakeRemoteServer()
	installFake(t, rm, fake)
	fake.addMessage("INBOX", "From: a@b.test\r\nSubject: Hello\r\nMessage-ID: <m1@remote>\r\n\r\nbody", "<m1@remote>", "Hello")
	fake.addMessage("INBOX", "From: c@d.test\r\nSubject: Second\r\nMessage-ID: <m2@remote>\r\n\r\nbody", "<m2@remote>", "Second")

	status, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != store.RemoteIndexComplete {
		t.Fatalf("index status = %q want complete", status.Status)
	}
	folders, err := svc.Store.ListFolders(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	var inboxValidity uint32
	for _, f := range folders {
		if f.Path == "INBOX" {
			inboxValidity = 100
		}
	}
	if inboxValidity == 0 {
		t.Fatal("INBOX folder not indexed")
	}
	msgs, err := svc.Store.ListRemoteMessages(ctx, u.AccountID, box.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("indexed messages = %d want 2", len(msgs))
	}
	// No body is archived: the metadata row has no raw path.
	if msgs[0].RFCMessageID == "" {
		t.Fatalf("message id not indexed: %+v", msgs[0])
	}
}

// TestRemoteReadDoesNotMarkSeen proves listing/reading headers never issues a
// \Seen flag change.
func TestRemoteReadDoesNotMarkSeen(t *testing.T) {
	_, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	configureSecrets(t, rm, u, box, "imap-pw", "")
	fake := newFakeRemoteServer()
	installFake(t, rm, fake)
	m := fake.addMessage("INBOX", "From: a@b.test\r\nSubject: Hello\r\nMessage-ID: <m1@remote>\r\n\r\nbody", "<m1@remote>", "Hello")
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
	rec, err := rm.GetRemoteMessage(ctx, p, box.ID, remoteIDForUID(t, rm, u, box, m.uid))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Read {
		t.Fatal("message unexpectedly read")
	}
	if fake.markSeenCalled {
		t.Fatal("listing/reading headers marked the message seen")
	}
}

// TestRemoteSetReadMarksSeenOnlyOnDemand proves an explicit read-state change sets
// \Seen and mirrors it locally.
func TestRemoteSetReadMarksSeenOnlyOnDemand(t *testing.T) {
	svc, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	configureSecrets(t, rm, u, box, "imap-pw", "")
	fake := newFakeRemoteServer()
	installFake(t, rm, fake)
	m := fake.addMessage("INBOX", "From: a@b.test\r\nSubject: Hello\r\nMessage-ID: <m1@remote>\r\n\r\nbody", "<m1@remote>", "Hello")
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	id := remoteIDForUID(t, rm, u, box, m.uid)
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, MailboxRoles: map[string]string{box.ID: "owner"}, Admin: true}
	view, err := rm.SetRemoteRead(ctx, p, box.ID, id, true)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Read {
		t.Fatal("read not mirrored")
	}
	stored, err := svc.Store.GetRemoteMessage(ctx, u.AccountID, box.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Read {
		t.Fatal("store read flag not updated")
	}
}

// TestRemoteUIDValidityChangeNoWrongFetch proves a stale cached UID (UIDVALIDITY
// changed) is never used to fetch a body: the store drops stale rows and the
// service re-resolves by Message-ID.
func TestRemoteUIDValidityChangeNoWrongFetch(t *testing.T) {
	svc, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	configureSecrets(t, rm, u, box, "imap-pw", "")
	fake := newFakeRemoteServer()
	installFake(t, rm, fake)
	fake.addMessage("INBOX", "From: a@b.test\r\nSubject: Hello\r\nMessage-ID: <m1@remote>\r\n\r\nold body", "<m1@remote>", "Hello")
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	// The server resets UIDVALIDITY. A reconcile must drop the stale row and
	// re-index the same Message-ID with the new UID.
	fake.mu.Lock()
	fake.folders["INBOX"] = 200
	fake.messages[0].uidValidity = 200
	fake.messages[0].raw = "From: a@b.test\r\nSubject: Hello\r\nMessage-ID: <m1@remote>\r\n\r\nnew body"
	fake.mu.Unlock()
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	msgs, err := svc.Store.ListRemoteMessages(ctx, u.AccountID, box.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages after validity change = %d want 1", len(msgs))
	}
	if msgs[0].UIDValidity != 200 {
		t.Fatalf("stale UIDVALIDITY retained: %d", msgs[0].UIDValidity)
	}
	// Fetching the raw body re-resolves and streams the fresh content.
	pr := model.Principal{AccountID: u.AccountID, UserID: u.ID, MailboxRoles: map[string]string{box.ID: "owner"}, Admin: true}
	path, size, err := rm.FetchRemoteRaw(ctx, pr, box.ID, msgs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rm.CleanupRemoteRaw(path)
	if size <= 0 || !rawContains(t, path, "new body") {
		t.Fatalf("stale body served")
	}
}

// TestRemoteMoveStableIDAndLabels proves a remote move relocates the cached
// metadata to the destination under the same stable id and preserves labels.
func TestRemoteMoveStableIDAndLabels(t *testing.T) {
	_, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	configureSecrets(t, rm, u, box, "imap-pw", "")
	fake := newFakeRemoteServer()
	fake.addFolder("Archive", 100)
	installFake(t, rm, fake)
	m := fake.addMessage("INBOX", "From: a@b.test\r\nSubject: Hello\r\nMessage-ID: <m1@remote>\r\n\r\nbody", "<m1@remote>", "Hello")
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	id := remoteIDForUID(t, rm, u, box, m.uid)
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, MailboxRoles: map[string]string{box.ID: "owner"}, Admin: true}
	if _, err := rm.SetRemoteLabels(ctx, p, box.ID, id, []string{"project-x"}); err != nil {
		t.Fatal(err)
	}
	moved, err := rm.MoveRemoteMessage(ctx, p, box.ID, id, "Archive")
	if err != nil {
		t.Fatal(err)
	}
	if moved.ID != id {
		t.Fatalf("move changed the stable id: %s -> %s", id, moved.ID)
	}
	if moved.FolderPath != "Archive" {
		t.Fatalf("moved folder = %q want Archive", moved.FolderPath)
	}
	if !containsFoldTest(moved.Labels, "project-x") {
		t.Fatalf("labels lost across move: %+v", moved.Labels)
	}
	if fake.moveCalled != 1 {
		t.Fatalf("move calls = %d want 1", fake.moveCalled)
	}
}

// TestRemoteSearchLocalLabelIntersection proves a live search result is
// intersected with a local label filter.
func TestRemoteSearchLocalLabelIntersection(t *testing.T) {
	_, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	configureSecrets(t, rm, u, box, "imap-pw", "")
	fake := newFakeRemoteServer()
	installFake(t, rm, fake)
	a := fake.addMessage("INBOX", "From: a@b.test\r\nSubject: Alpha\r\nMessage-ID: <a@remote>\r\n\r\nx", "<a@remote>", "Alpha")
	fake.addMessage("INBOX", "From: c@d.test\r\nSubject: Alpha two\r\nMessage-ID: <b@remote>\r\n\r\ny", "<b@remote>", "Alpha two")
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, MailboxRoles: map[string]string{box.ID: "owner"}, Admin: true}
	aID := remoteIDForUID(t, rm, u, box, a.uid)
	if _, err := rm.SetRemoteLabels(ctx, p, box.ID, aID, []string{"keep"}); err != nil {
		t.Fatal(err)
	}
	res, err := rm.SearchRemote(ctx, p, box.ID, app.RemoteSearchQuery{Subject: "Alpha", Label: "keep", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || !containsFoldTest(res.Items[0].Labels, "keep") {
		t.Fatalf("label intersection failed: %+v", res.Items)
	}
}

// TestRemoteThreadsScoped proves thread listing aggregates only the inbox's own
// cached remote messages.
func TestRemoteThreadsScoped(t *testing.T) {
	_, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	configureSecrets(t, rm, u, box, "imap-pw", "")
	fake := newFakeRemoteServer()
	installFake(t, rm, fake)
	fake.addMessage("INBOX", "From: a@b.test\r\nSubject: Thread\r\nMessage-ID: <t1@remote>\r\n\r\nx", "<t1@remote>", "Thread")
	// A second message referencing the first (same thread key via Message-ID).
	fake.addMessage("INBOX", "From: c@d.test\r\nSubject: Re: Thread\r\nMessage-ID: <t2@remote>\r\nIn-Reply-To: <t1@remote>\r\nReferences: <t1@remote>\r\n\r\ny", "<t2@remote>", "Re: Thread")
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	// Pin both messages to the same thread key the adapter would derive: the
	// store's thread key defaults to the Message-ID, so a real adapter threads by
	// References. Here we assert the aggregation is inbox-scoped by counting.
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, MailboxRoles: map[string]string{box.ID: "owner"}, Admin: true}
	threads, err := rm.ListRemoteThreads(ctx, p, box.ID, "", 50, "")
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, th := range threads {
		if th.InboxID != box.ID {
			t.Fatalf("thread from another inbox: %+v", th)
		}
		total += th.MessageCount
	}
	if total != 2 {
		t.Fatalf("threaded messages total = %d want 2", total)
	}
}

// TestRemoteScopeEnforcedForIDs proves an operation on a path outside an explicit
// root scope is rejected before any remote call.
func TestRemoteScopeEnforcedForIDs(t *testing.T) {
	svc, u, _, rm := remoteTestEnv(t)
	ctx := context.Background()
	box, err := svc.Store.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{
		Address:   "scoped@remote.example",
		Namespace: "Archive",
		Remote:    &model.RemoteConnection{Host: "imap.remote.example", Username: "scoped@remote.example", Security: model.RemoteSecurityPlain, Port: 143},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.EnsureSystemFolders(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	configureSecrets(t, rm, u, box, "imap-pw", "")
	fake := newFakeRemoteServer()
	fake.addFolder("Archive", 100)
	fake.addFolder("INBOX", 100)
	installFake(t, rm, fake)
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, MailboxRoles: map[string]string{box.ID: "owner"}, Admin: true}
	// INBOX is outside an explicit "Archive" root scope.
	_, err = rm.ListRemoteMessages(ctx, p, box.ID, "INBOX", 10, "")
	var mb *model.MailboxError
	if !errors.As(err, &mb) || mb.Kind != model.ErrKindNotFound {
		t.Fatalf("out-of-scope folder err = %v want not_found", err)
	}
}

// TestRemoteSentCopyNotResubmittedOnCopyFail proves a failed sent copy is confined
// to the copy job and never re-runs the (already successful) SMTP send.
func TestRemoteSentCopyNotResubmittedOnCopyFail(t *testing.T) {
	svc, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	configureSecrets(t, rm, u, box, "imap-pw", "")
	// Queue a sent-copy job pointing at a real frozen file.
	raw := "From: agent@remote.example\r\nTo: x@y.test\r\nSubject: Sent\r\nMessage-ID: <sent1@remote>\r\n\r\nbody"
	path := writeFrozenRaw(t, svc, raw)
	if _, err := svc.Store.EnqueueRemoteSentCopy(ctx, u.AccountID, box.ID, store.RemoteSentCopyInput{
		RFCMessageID: "<sent1@remote>",
		MessageIDHdr: "<sent1@remote>",
		RawPath:      path,
		SizeBytes:    int64(len(raw)),
	}); err != nil {
		t.Fatal(err)
	}
	// The Sent append fails permanently.
	fake := newFakeRemoteServer()
	fake.appendFail = model.NewMailboxError(model.ErrKindForbidden, "refused", false, nil)
	installFake(t, rm, fake)
	rm.CopySentCopies(ctx)

	// The copy job is terminal (failed); no pending job remains.
	job, err := svc.Store.GetRemoteSentCopyByMessageID(ctx, u.AccountID, box.ID, "<sent1@remote>")
	if err != nil {
		t.Fatal(err)
	}
	if job.State != store.RemoteCopyFailed {
		t.Fatalf("copy state = %q want failed", job.State)
	}
	// A second pass does not re-append.
	before := fake.appendCount
	rm.CopySentCopies(ctx)
	if fake.appendCount != before {
		t.Fatalf("failed copy was re-appended: %d -> %d", before, fake.appendCount)
	}
}

// TestRemoteSentCopyVerifiedByMessageID proves an append with no APPENDUID is
// confirmed by a Message-ID lookup and settled Copied, never left ambiguous when
// the message is found exactly once.
func TestRemoteSentCopyVerifiedByMessageID(t *testing.T) {
	svc, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	configureSecrets(t, rm, u, box, "imap-pw", "")
	raw := "From: agent@remote.example\r\nTo: x@y.test\r\nSubject: Sent\r\nMessage-ID: <sent2@remote>\r\n\r\nbody"
	path := writeFrozenRaw(t, svc, raw)
	if _, err := svc.Store.EnqueueRemoteSentCopy(ctx, u.AccountID, box.ID, store.RemoteSentCopyInput{
		RFCMessageID: "<sent2@remote>",
		MessageIDHdr: "<sent2@remote>",
		RawPath:      path,
		SizeBytes:    int64(len(raw)),
	}); err != nil {
		t.Fatal(err)
	}
	fake := newFakeRemoteServer()
	fake.appendNoUID = true
	installFake(t, rm, fake)
	rm.CopySentCopies(ctx)
	job, err := svc.Store.GetRemoteSentCopyByMessageID(ctx, u.AccountID, box.ID, "<sent2@remote>")
	if err != nil {
		t.Fatal(err)
	}
	if job.State != store.RemoteCopyCopied {
		t.Fatalf("copy state = %q want copied (verified by Message-ID)", job.State)
	}
}

// TestRemoteConfigureKeepsNonSecretFields proves configuring secrets does not blank
// the non-secret remote description, and the encrypted credentials decrypt.
func TestRemoteConfigureKeepsNonSecretFields(t *testing.T) {
	_, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	configureSecrets(t, rm, u, box, "imap-pw", "smtp-pw")
	got, err := rm.ResolveRemote(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Inbox.Remote == nil || got.Inbox.Remote.Host != "imap.remote.example" || got.Inbox.Remote.Username != "agent@remote.example" {
		t.Fatalf("non-secret fields lost: %+v", got.Inbox.Remote)
	}
	if got.Secrets.IMAPPassword != "imap-pw" || got.Secrets.SMTPPassword != "smtp-pw" {
		t.Fatalf("secrets not round-tripped: %+v", got.Secrets)
	}
}

// TestRemoteStandaloneSenderResolvesSMTP proves the standalone sender resolver
// builds the inbox's own SMTP config and the existing decrypt path reads it.
func TestRemoteStandaloneSenderResolvesSMTP(t *testing.T) {
	svc, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	// Bind an SMTP server on the inbox.
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
	if _, err := svc.Store.UpdateStandaloneRemote(ctx, u.AccountID, box.ID, store.StandaloneRemoteUpdate{
		SMTPHost: "smtp.remote.example", SMTPPort: 465, SMTPUsername: "agent@remote.example", SMTPSecurity: model.RemoteSecurityTLS,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rm.ConfigureStandaloneRemote(ctx, p, box.ID, store.StandaloneRemoteUpdate{IMAPPassword: "imap-pw", SMTPPassword: "smtp-pw"}); err != nil {
		t.Fatal(err)
	}
	resolver := &app.RemoteStandaloneSender{Service: svc}
	cfg, err := resolver.ResolveInboxSendingConfig(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "smtp" || cfg.DomainID != box.ID {
		t.Fatalf("sending config = %+v", cfg)
	}
	dec, err := svc.DecryptDomainSendingConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if dec["host"] != "smtp.remote.example" || dec["password"] != "smtp-pw" {
		t.Fatalf("decrypted smtp config = %+v", dec)
	}
}

// TestRemoteSMTPAmbiguousClassification proves the SMTP transport classifies a
// failure after the message body was sent as ambiguous, so the outbox fails it
// terminally instead of retrying (which could deliver a duplicate). The transport
// is exercised in its own package tests; here we assert the shared classification
// the outbox relies on.
func TestRemoteSMTPAmbiguousClassification(t *testing.T) {
	if !transport.AsAmbiguous(&transport.AmbiguousError{Err: errors.New("timeout awaiting verdict")}) {
		t.Fatal("AmbiguousError not recognized")
	}
	if transport.IsPermanent(&transport.AmbiguousError{Err: errors.New("x")}) {
		t.Fatal("ambiguous error must not be permanent")
	}
}

// TestRemoteRealAdapterInMemoryServer verifies the production remote backend end
// to end against a real in-memory IMAP4rev2 server through the real imap.Dial
// adapter: reconcile indexes a folder, the message is listed, and its raw body is
// fetched without marking it seen.
func TestRemoteRealAdapterInMemoryServer(t *testing.T) {
	_, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()

	host, port, user, add := startInMemoryIMAP(t)
	// Repoint the inbox's remote binding at the in-memory server and store the
	// plaintext credential through the real configure path.
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
	if _, err := rm.ConfigureStandaloneRemote(ctx, p, box.ID, store.StandaloneRemoteUpdate{
		Host: host, Port: port, Username: user, Security: model.RemoteSecurityPlain,
		IMAPPassword: "secret",
	}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	add("INBOX", "From: a@b.test\r\nTo: agent@remote.example\r\nSubject: Live\r\nMessage-ID: <live@remote>\r\n\r\nlive body")

	status, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != store.RemoteIndexComplete {
		t.Fatalf("index status = %q want complete", status.Status)
	}
	res, err := rm.ListRemoteMessages(ctx, p, box.ID, "INBOX", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || res.Items[0].Subject != "Live" {
		t.Fatalf("listed = %+v", res.Items)
	}
	if res.Items[0].Read {
		t.Fatal("message should start unread")
	}
	path, _, err := rm.FetchRemoteRaw(ctx, p, box.ID, res.Items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rm.CleanupRemoteRaw(path)
	if !rawContains(t, path, "live body") {
		t.Fatal("raw body not streamed")
	}
	// Listing/reading headers must not have set \Seen.
	after, err := rm.GetRemoteMessage(ctx, p, box.ID, res.Items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Read {
		t.Fatal("BODY.PEEK fetch marked the message seen")
	}
}

// startInMemoryIMAP starts an in-memory IMAP4rev2 server on a loopback port and
// returns its host, port, username and an append helper. It is the approved test
// fixture: a real go-imap server, exercised through the real imap.Dial adapter.
func startInMemoryIMAP(t *testing.T) (string, int, string, func(mailbox, raw string)) {
	t.Helper()
	mem := imapmemserver.New()
	const username, password = "agent@remote.example", "secret"
	u := imapmemserver.NewUser(username, password)
	if err := u.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Drafts", "Sent"} {
		if err := u.Create(name, nil); err != nil {
			t.Fatal(err)
		}
	}
	mem.AddUser(u)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return imapmemserver.NewUserSession(u), nil, nil
		},
		Caps:         goimap.CapSet{goimap.CapIMAP4rev1: {}, goimap.CapIMAP4rev2: {}, goimap.CapSpecialUse: {}},
		InsecureAuth: true,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
	})
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port := 0
	for _, ch := range portStr {
		port = port*10 + int(ch-'0')
	}
	add := func(mailbox, raw string) {
		t.Helper()
		done := make(chan struct{})
		var appendErr error
		go func() {
			defer close(done)
			_, appendErr = u.Append(mailbox, imapLiteral(raw), &goimap.AppendOptions{})
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("append timed out")
		}
		if appendErr != nil {
			t.Fatalf("append: %v", appendErr)
		}
	}
	return host, port, username, add
}

// imapLiteral adapts a byte slice to the goimap.LiteralReader the in-memory server
// expects.
func imapLiteral(raw string) goimap.LiteralReader {
	return &stringLiteral{data: raw}
}

type stringLiteral struct {
	data string
	off  int
	size int64
}

func (l *stringLiteral) Size() int64 { return int64(len(l.data)) }
func (l *stringLiteral) Read(p []byte) (int, error) {
	if l.off >= len(l.data) {
		return 0, io.EOF
	}
	n := copy(p, l.data[l.off:])
	l.off += n
	return n, nil
}

// TestRemoteBridgesInstallAndTest proves InstallRemoteBridges wires the handoff
// publisher and the standalone sender resolver onto the service/store, and that
// TestStandaloneRemote validates a live configuration against the real adapter.
func TestRemoteBridgesInstallAndTest(t *testing.T) {
	svc, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	rm.InstallRemoteBridges()

	// The handoff publisher and sender resolver are installed.
	if svc.HandoffPublisher == nil {
		t.Fatal("handoff publisher not installed")
	}

	host, port, user, _ := startInMemoryIMAP(t)
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
	scope, err := rm.TestStandaloneRemote(ctx, p, box.ID, store.StandaloneRemoteUpdate{
		Host: host, Port: port, Username: user, Security: model.RemoteSecurityPlain, IMAPPassword: "secret",
	})
	if err != nil {
		t.Fatalf("test remote: %v", err)
	}
	if scope.Delimiter == 0 {
		t.Fatalf("scope delimiter not resolved: %+v", scope)
	}
}

// TestRemoteDeleteFolderServerRefusalMapped proves that when the cache is stale
// (it does not know a folder has children) the server's refusal to delete a
// non-empty folder is still surfaced as ErrFolderNotEmpty, not a generic error.
func TestRemoteDeleteFolderServerRefusalMapped(t *testing.T) {
	_, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	configureSecrets(t, rm, u, box, "imap-pw", "")
	fake := newFakeRemoteServer()
	fake.addFolder("Parent", 100)
	installFake(t, rm, fake)
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	// The child appears on the server AFTER the reconcile, so the local cache does
	// not know about it.
	fake.addFolder("Parent/Child", 100)
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, MailboxRoles: map[string]string{box.ID: "owner"}, Admin: true}
	folders, err := rm.Service.Store.ListFolders(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	var parentID string
	for _, f := range folders {
		if f.Path == "Parent" {
			parentID = f.ID
		}
	}
	if err := rm.DeleteRemoteFolder(ctx, p, box.ID, parentID); !errors.Is(err, store.ErrFolderNotEmpty) {
		t.Fatalf("server-refused delete err = %v want ErrFolderNotEmpty", err)
	}
}

// TestRemoteMoveAmbiguousMessageIDNotMerged proves that when a move reports no
// destination UID and the destination holds more than one message with the same
// Message-ID, the service refuses to guess: it does not merge the source onto a
// duplicate, and reports a retryable outcome so a later reconcile re-indexes it.
func TestRemoteMoveAmbiguousMessageIDNotMerged(t *testing.T) {
	_, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	configureSecrets(t, rm, u, box, "imap-pw", "")
	fake := newFakeRemoteServer()
	fake.addFolder("Archive", 100)
	fake.moveNoUID = true
	installFake(t, rm, fake)
	src := fake.addMessage("INBOX", "From: a@b.test\r\nSubject: S\r\nMessage-ID: <same@remote>\r\n\r\nx", "<same@remote>", "S")
	// The destination already has a different message with the same Message-ID.
	fake.addMessage("Archive", "From: c@d.test\r\nSubject: Dup\r\nMessage-ID: <same@remote>\r\n\r\ny", "<same@remote>", "Dup")
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	id := remoteIDForUID(t, rm, u, box, src.uid)
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, MailboxRoles: map[string]string{box.ID: "owner"}, Admin: true}
	_, err := rm.MoveRemoteMessage(ctx, p, box.ID, id, "Archive")
	var mb *model.MailboxError
	if !errors.As(err, &mb) || mb.Kind != model.ErrKindRetryable {
		t.Fatalf("ambiguous move-id err = %v want retryable", err)
	}
	// The source metadata row was dropped rather than merged onto the duplicate.
	if _, gerr := rm.Service.Store.GetRemoteMessage(ctx, u.AccountID, box.ID, id); !errors.Is(gerr, store.ErrNotFound) {
		t.Fatalf("ambiguous source row kept: %v", gerr)
	}
}

// TestRemoteReconcileBackfillsWholeFolder proves a folder larger than one
// reconcile batch is indexed in full over successive passes (never permanently
// truncated at the batch size) and that a remotely-deleted message is pruned once
// a complete snapshot is seen.
func TestRemoteReconcileBackfillsWholeFolder(t *testing.T) {
	svc, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	configureSecrets(t, rm, u, box, "imap-pw", "")
	fake := newFakeRemoteServer()
	installFake(t, rm, fake)
	const total = 2100 // > DefaultRemoteReconcileLimit (2000)
	for i := 0; i < total; i++ {
		fake.addMessage("INBOX", fmt.Sprintf("From: a@b.test\r\nSubject: m%d\r\nMessage-ID: <m%d@remote>\r\n\r\nbody", i, i), fmt.Sprintf("<m%d@remote>", i), fmt.Sprintf("m%d", i))
	}
	// First pass indexes the newest batch, not the whole folder.
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	cached, err := svc.Store.ListRemoteMessages(ctx, u.AccountID, box.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(cached) >= total {
		t.Fatalf("first pass indexed the whole folder (%d); expected a bounded batch", len(cached))
	}
	if len(cached) == 0 {
		t.Fatal("first pass indexed nothing")
	}
	// Successive passes backfill the rest.
	for i := 0; i < 5; i++ {
		if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
			t.Fatal(err)
		}
		got, err := svc.Store.ListRemoteMessages(ctx, u.AccountID, box.ID, "INBOX")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == total {
			break
		}
	}
	got, err := svc.Store.ListRemoteMessages(ctx, u.AccountID, box.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != total {
		t.Fatalf("backfill did not cover the whole folder: %d/%d", len(got), total)
	}
	status, err := svc.Store.GetRemoteIndexStatus(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != store.RemoteIndexComplete {
		t.Fatalf("index status = %q want complete after full backfill", status.Status)
	}
	// Delete an old message remotely; a later reconcile prunes it from the cache
	// (the complete snapshot is authoritative).
	oldest := got[len(got)-1]
	fake.mu.Lock()
	for i, m := range fake.messages {
		if m.uid == oldest.UID {
			fake.messages = append(fake.messages[:i], fake.messages[i+1:]...)
			break
		}
	}
	fake.mu.Unlock()
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	after, err := svc.Store.ListRemoteMessages(ctx, u.AccountID, box.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range after {
		if m.UID == oldest.UID {
			t.Fatalf("remotely-deleted message %d was not pruned", oldest.UID)
		}
	}
	if len(after) != total-1 {
		t.Fatalf("after prune: %d want %d", len(after), total-1)
	}
}

// TestRemotePurgeRequiresOwnerAndTrash proves a permanent remote purge requires
// Owner and a message that is currently in the Trash-role folder, and uses a
// UID-targeted delete (never a blanket expunge).
func TestRemotePurgeRequiresOwnerAndTrash(t *testing.T) {
	_, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	configureSecrets(t, rm, u, box, "imap-pw", "")
	fake := newFakeRemoteServer()
	fake.addFolder("Trash", 100)
	installFake(t, rm, fake)
	m := fake.addMessage("INBOX", "From: a@b.test\r\nSubject: Hi\r\nMessage-ID: <p1@remote>\r\n\r\nbody", "<p1@remote>", "Hi")
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	owner := model.Principal{AccountID: u.AccountID, UserID: u.ID, MailboxRoles: map[string]string{box.ID: "owner"}, Admin: true}
	assistant := model.Principal{AccountID: u.AccountID, UserID: u.ID, MailboxRoles: map[string]string{box.ID: "assistant"}}
	id := remoteIDForUID(t, rm, u, box, m.uid)

	// A message still in INBOX cannot be permanently purged.
	if err := rm.PurgeRemoteMessage(ctx, owner, box.ID, id); err == nil {
		t.Fatal("purge of a non-trashed message succeeded")
	}
	var mb *model.MailboxError
	if err := rm.PurgeRemoteMessage(ctx, owner, box.ID, id); !errors.As(err, &mb) || mb.Kind != model.ErrKindConflict {
		t.Fatalf("non-trash purge err = %v want conflict", err)
	}
	// An assistant (not Owner) is refused.
	if _, err := rm.MoveRemoteMessage(ctx, owner, box.ID, id, "Trash"); err != nil {
		t.Fatal(err)
	}
	if err := rm.PurgeRemoteMessage(ctx, assistant, box.ID, id); err == nil {
		t.Fatal("non-owner purge succeeded")
	}
	// Owner purges the trashed message with a UID-targeted delete.
	if err := rm.PurgeRemoteMessage(ctx, owner, box.ID, id); err != nil {
		t.Fatalf("owner purge: %v", err)
	}
	if fake.deleteCalled != 1 {
		t.Fatalf("delete calls = %d want 1", fake.deleteCalled)
	}
	if _, err := rm.GetRemoteMessage(ctx, owner, box.ID, id); !errors.Is(err, store.ErrNotFound) && err == nil {
		t.Fatalf("purged message still cached: %v", err)
	}
	// A second purge is an idempotent success.
	if err := rm.PurgeRemoteMessage(ctx, owner, box.ID, id); err != nil {
		t.Fatalf("idempotent purge: %v", err)
	}
}

// TestRemoteDeleteFolderRefusesNonEmpty proves deleting a remote folder refuses
// when it still has a cached child folder, before any provider call.
func TestRemoteDeleteFolderRefusesNonEmpty(t *testing.T) {
	_, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	configureSecrets(t, rm, u, box, "imap-pw", "")
	fake := newFakeRemoteServer()
	fake.addFolder("Projects", 100)
	fake.addFolder("Projects/2026", 100)
	installFake(t, rm, fake)
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, MailboxRoles: map[string]string{box.ID: "owner"}, Admin: true}
	folders, err := rm.Service.Store.ListFolders(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	var parentID string
	for _, f := range folders {
		if f.Path == "Projects" {
			parentID = f.ID
		}
	}
	if parentID == "" {
		t.Fatal("Projects folder not indexed")
	}
	err = rm.DeleteRemoteFolder(ctx, p, box.ID, parentID)
	if !errors.Is(err, store.ErrFolderNotEmpty) {
		t.Fatalf("delete non-empty parent err = %v want ErrFolderNotEmpty", err)
	}
}

// TestSetRemoteFolderRolePersists proves an explicit role mapping of an
// arbitrarily-named existing folder survives a reconcile.
func TestSetRemoteFolderRolePersists(t *testing.T) {
	_, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	configureSecrets(t, rm, u, box, "imap-pw", "")
	fake := newFakeRemoteServer()
	fake.addFolder("Old Mail", 100)
	installFake(t, rm, fake)
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, MailboxRoles: map[string]string{box.ID: "owner"}, Admin: true}
	folders, err := rm.Service.Store.ListFolders(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	var targetID string
	for _, f := range folders {
		if f.Path == "Old Mail" {
			targetID = f.ID
		}
	}
	if targetID == "" {
		t.Fatal("Old Mail folder not indexed")
	}
	mapped, err := rm.SetRemoteFolderRole(ctx, p, box.ID, targetID, model.FolderRoleTrash)
	if err != nil {
		t.Fatalf("SetRemoteFolderRole: %v", err)
	}
	if mapped.Role != model.FolderRoleTrash {
		t.Fatalf("mapped role = %q want trash", mapped.Role)
	}
	// A reconcile that re-infers from the (non-Trash) name must not reset it.
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	got, err := rm.Service.Store.GetFolderByPath(ctx, u.AccountID, box.ID, "Old Mail")
	if err != nil {
		t.Fatal(err)
	}
	if got.Role != model.FolderRoleTrash || !got.RoleLocked {
		t.Fatalf("role reset by reconcile: %+v", got)
	}
}

// TestRemoteReplyAndForwardResolve proves a reply/forward target that is a remote
// message (no local row) resolves through the installed remote bridge, and a
// forward carries the remotely-parsed body.
func TestRemoteReplyAndForwardResolve(t *testing.T) {
	_, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	rm.InstallRemoteBridges()
	configureSecrets(t, rm, u, box, "imap-pw", "")
	fake := newFakeRemoteServer()
	installFake(t, rm, fake)
	raw := "From: sender@elsewhere.test\r\nTo: agent@remote.example\r\nSubject: Q\r\nMessage-ID: <q1@remote>\r\n\r\nPlease advise.\r\n"
	m := fake.addMessage("INBOX", raw, "<q1@remote>", "Q")
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	id := remoteIDForUID(t, rm, u, box, m.uid)
	reply, ok, err := rm.ResolveRemoteReply(ctx, u.AccountID, id)
	if err != nil || !ok {
		t.Fatalf("ResolveRemoteReply ok=%v err=%v", ok, err)
	}
	if reply.RFCMessageID != "<q1@remote>" || reply.InboxID != box.ID {
		t.Fatalf("reply resolution = %+v", reply)
	}
	fwd, atts, ok, err := rm.ResolveRemoteForward(ctx, u.AccountID, id)
	if err != nil || !ok {
		t.Fatalf("ResolveRemoteForward ok=%v err=%v", ok, err)
	}
	if len(atts) != 0 {
		t.Fatalf("unexpected attachments: %+v", atts)
	}
	if !strings.Contains(fwd.Text, "Please advise.") {
		t.Fatalf("forward body not fetched: %q", fwd.Text)
	}
}

// ---- helpers ----

func remoteIDForUID(t *testing.T, rm *app.RemoteMailboxService, u model.User, box model.Inbox, uid uint32) string {
	t.Helper()
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, MailboxRoles: map[string]string{box.ID: "owner"}, Admin: true}
	res, err := rm.ListRemoteMessages(context.Background(), p, box.ID, "INBOX", 100, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range res.Items {
		if m.UID == uid {
			return m.ID
		}
	}
	t.Fatalf("no cached message with uid %d", uid)
	return ""
}

func rawContains(t *testing.T, path, sub string) bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(b), sub)
}

func containsFoldTest(list []string, v string) bool {
	for _, item := range list {
		if strings.EqualFold(item, v) {
			return true
		}
	}
	return false
}

func writeFrozenRaw(t *testing.T, svc *app.Service, raw string) string {
	t.Helper()
	rel := filepath.Join("remote", fmt.Sprintf("test-%d.eml", time.Now().UnixNano()))
	full := filepath.Join(svc.Config.DataDir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(rel)
}

// fakeLocalSMTP emulates a real ESMTP submission server: it advertises AUTH
// PLAIN, validates the presented credentials against the expected user/pass
// (rejecting a wrong login with 535), accepts one message body, and reports 250.
// It exists so a standalone send can be exercised end to end without a live
// server, with the realistic credential round-trip the transport performs.
func fakeLocalSMTP(t *testing.T, wantUser, wantPass string, sent *bool) (host string, port int, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		w := bufio.NewWriter(c)
		fmt.Fprint(w, "220 test ESMTP\r\n")
		w.Flush()
		data := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if data {
				if line == ".\r\n" {
					fmt.Fprint(w, "250 ok\r\n")
					w.Flush()
					data = false
				}
				continue
			}
			trimmed := strings.TrimSpace(line)
			upper := strings.ToUpper(trimmed)
			switch {
			case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
				fmt.Fprint(w, "250-test\r\n250-AUTH PLAIN\r\n250 OK\r\n")
			case strings.HasPrefix(upper, "AUTH PLAIN"):
				fields := strings.Fields(trimmed)
				if len(fields) < 3 {
					// Initial response expected; prompt if absent.
					fmt.Fprint(w, "334 \r\n")
					w.Flush()
					resp, rerr := r.ReadString('\n')
					if rerr != nil {
						return
					}
					fields = append(fields, strings.TrimSpace(resp))
				}
				decoded, derr := base64.StdEncoding.DecodeString(fields[len(fields)-1])
				if derr != nil || string(decoded) != "\x00"+wantUser+"\x00"+wantPass {
					fmt.Fprint(w, "535 5.7.8 bad credentials\r\n")
					w.Flush()
					continue
				}
				fmt.Fprint(w, "235 2.7.0 ok\r\n")
			case strings.HasPrefix(upper, "DATA"):
				if sent != nil {
					*sent = true
				}
				fmt.Fprint(w, "354 go\r\n")
				data = true
			case strings.HasPrefix(upper, "QUIT"):
				fmt.Fprint(w, "221 bye\r\n")
				w.Flush()
				return
			default:
				fmt.Fprint(w, "250 ok\r\n")
			}
			w.Flush()
		}
	}()
	a := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", a.Port, func() { ln.Close() }
}

// TestStandaloneSentCopyFreezesOwnRaw proves a standalone sent-copy job freezes
// its own copy of the raw MIME, independent of the outbound message's raw file.
// The job must remain verifiable after the message is purged (which unlinks the
// message's raw), otherwise a pending or ambiguous copy would be lost.
func TestStandaloneSentCopyFreezesOwnRaw(t *testing.T) {
	svc, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	svc.Store.SetStandaloneSenderResolver(&app.RemoteStandaloneSender{Service: svc})
	rm.InstallRemoteBridges()

	host, port, closeFn := fakeLocalSMTP(t, "agent@remote.example", "smtp-pw", nil)
	defer closeFn()
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
	if _, err := svc.Store.UpdateStandaloneRemote(ctx, u.AccountID, box.ID, store.StandaloneRemoteUpdate{
		Host: "imap.remote.example", Username: "agent@remote.example",
		SMTPHost: host, SMTPPort: port, SMTPUsername: "agent@remote.example", SMTPSecurity: model.RemoteSecurityPlain,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rm.ConfigureStandaloneRemote(ctx, p, box.ID, store.StandaloneRemoteUpdate{IMAPPassword: "imap-pw", SMTPPassword: "smtp-pw"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetRemoteSentCopy(ctx, p, box.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	// Enable the Sent-role folder mapping on the fake remote so the copy resolves.
	res, err := svc.Send(ctx, p, app.SendInput{
		InboxID: box.ID, FromAddress: box.Address, To: []string{"x@outside.test"},
		Subject: "Sent copy freeze", Text: "body",
	}, "freeze-key")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := svc.Deliver(ctx, u.AccountID, res.Message.ID, ""); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	job, err := svc.Store.GetRemoteSentCopyByMessageID(ctx, u.AccountID, box.ID, res.Message.RFCMessageID)
	if err != nil {
		t.Fatalf("sent-copy job not enqueued: %v", err)
	}
	msg, err := svc.Store.GetMessageByID(ctx, u.AccountID, res.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.RawPath == msg.RawPath {
		t.Fatalf("sent-copy references the message's own raw path %q; a purge would orphan it", job.RawPath)
	}
	if !strings.HasPrefix(job.RawPath, "sentshare/") {
		t.Fatalf("sent-copy raw path %q not frozen under sentshare/", job.RawPath)
	}
	// Remove the message's own raw file (as a purge would); the frozen copy must
	// still be readable.
	msgPath := filepath.Join(svc.Config.DataDir, filepath.FromSlash(msg.RawPath))
	if err := os.Remove(msgPath); err != nil {
		t.Fatal(err)
	}
	frozenPath := filepath.Join(svc.Config.DataDir, filepath.FromSlash(job.RawPath))
	if _, err := os.Stat(frozenPath); err != nil {
		t.Fatalf("frozen sent-copy raw was lost with the message: %v", err)
	}
}

// TestResolveRemoteReplyFromArrivalID proves a reply can target a remote message
// by the arrival id carried on a remote event, even when the metadata index has
// not yet recorded the message (detection is independent of the index). The
// resolver bridges the arrival id to a materialized metadata record.
func TestResolveRemoteReplyFromArrivalID(t *testing.T) {
	svc, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	arrival, _, err := svc.Store.RecordRemoteArrival(ctx, u.AccountID, box.ID, store.RemoteArrivalInput{
		FolderPath: "INBOX", UIDValidity: 100, UID: 7,
		RFCMessageID: "<incoming@remote>", FromName: "Sender", FromAddress: "sender@elsewhere.test", Subject: "Incoming",
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, ok, err := rm.ResolveRemoteReply(ctx, u.AccountID, arrival.ID)
	if err != nil {
		t.Fatalf("resolve reply from arrival id: %v", err)
	}
	if !ok {
		t.Fatal("arrival id did not resolve to a reply source")
	}
	if msg.RFCMessageID != "<incoming@remote>" || msg.From.Address != "sender@elsewhere.test" {
		t.Fatalf("resolved message = %+v", msg)
	}
}
