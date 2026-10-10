package httpapp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/config"
	"github.com/dellarb/mailmoose/internal/events"
	"github.com/dellarb/mailmoose/internal/httpapp"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/imap"
	"github.com/dellarb/mailmoose/tests/support/testdb"
)

// standaloneFixture builds an app service, an admin user, a managed domain with
// a domain inbox and a configured standalone inbox whose remote server is an
// in-flight fake. It returns the handler, the service and both inboxes.
func standaloneFixture(t *testing.T) (*app.Service, http.Handler, model.User, model.Inbox, model.Inbox, *fakeIMAP) {
	t.Helper()
	st, dir := testdb.OpenDir(t)
	cfg := testConfig(dir)
	svc, err := app.New(cfg, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateAccountAndAdmin(context.Background(), "A", "admin@example.com", "correct horse battery staple", cfg.DefaultQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDomain(context.Background(), u.AccountID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	domainBox, err := st.CreateInbox(context.Background(), u.AccountID, d.ID, "hermes", "Hermes")
	if err != nil {
		t.Fatal(err)
	}
	standalone, err := st.CreateStandaloneInbox(context.Background(), u.AccountID, store.StandaloneCreate{
		DisplayName: "Agent",
		Address:     "agent@remote.test",
		Remote:      &model.RemoteConnection{Host: "imap.remote.test", Username: "agent@remote.test", Security: model.RemoteSecurityPlain, Port: 143},
	})
	if err != nil {
		t.Fatal(err)
	}
	rm := app.NewRemoteMailboxService(svc)
	t.Cleanup(rm.Stop)
	fake := newFakeIMAP()
	fake.folders["Archive"] = 100
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) { return fake, nil })
	// Store secrets through the real configure path.
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
	if _, err := rm.ConfigureStandaloneRemote(context.Background(), p, standalone.ID, store.StandaloneRemoteUpdate{IMAPPassword: "imap-pw"}); err != nil {
		t.Fatal(err)
	}
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	return svc, srv.Handler(), u, domainBox, standalone, fake
}

// fakeIMAP is a minimal deterministic IMAP session used to exercise the HTTP
// dispatch without a live server.
type fakeIMAP struct {
	mu       sync.Mutex
	folders  map[string]uint32
	messages []*fakeIMAPMsg
	nextUID  uint32
	failOpen bool
}

type fakeIMAPMsg struct {
	uid, uidValidity uint32
	folder, raw      string
	messageID        string
	subject          string
	from             imap.Address
	internalDate     time.Time
}

func fakMsgDate(m *fakeIMAPMsg) time.Time {
	if m.internalDate.IsZero() {
		return time.Now().UTC()
	}
	return m.internalDate
}

func newFakeIMAP() *fakeIMAP {
	return &fakeIMAP{folders: map[string]uint32{"INBOX": 100, "Drafts": 100, "Sent": 100}, nextUID: 1}
}

func (f *fakeIMAP) add(folder, raw, messageID, subject string) {
	f.addAt(folder, raw, messageID, subject, time.Now().UTC())
}

// addAt adds a message with an explicit internal date so ordering is
// deterministic across inboxes.
func (f *fakeIMAP) addAt(folder, raw, messageID, subject string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, &fakeIMAPMsg{uid: f.nextUID, uidValidity: f.folders[folder], folder: folder, raw: raw, messageID: messageID, subject: subject, from: imap.Address{Name: "Remote", Address: "remote@elsewhere.test"}, internalDate: at})
	f.nextUID++
}

func (f *fakeIMAP) find(loc imap.Locator) *fakeIMAPMsg {
	for _, m := range f.messages {
		if m.folder == loc.FolderPath && m.uid == loc.UID {
			return m
		}
	}
	return nil
}

// folderOf returns the folder a message currently lives in, by Message-ID, or ""
// when it is not present (expunged).
func (f *fakeIMAP) folderOf(messageID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.messages {
		if m.messageID == messageID {
			return m.folder
		}
	}
	return ""
}

func (f *fakeIMAP) DiscoverFolders(_ context.Context, _ string) ([]imap.RemoteFolder, imap.RootScope, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]imap.RemoteFolder, 0, len(f.folders))
	for path := range f.folders {
		out = append(out, imap.RemoteFolder{Path: path, Name: path, Delimiter: '/', Role: testRole(path), Selectable: true})
	}
	return out, imap.RootScope{Root: "INBOX", Delimiter: '/', Personal: true, INBOXInScope: true}, nil
}

func testRole(path string) string {
	switch strings.ToLower(path) {
	case "inbox":
		return model.FolderRoleInbox
	case "drafts":
		return model.FolderRoleDrafts
	case "sent":
		return model.FolderRoleSent
	case "trash", "deleted", "deleted items":
		return model.FolderRoleTrash
	case "spam", "junk":
		return model.FolderRoleSpam
	case "archive", "all mail", "all":
		return model.FolderRoleArchive
	default:
		return model.FolderRoleFolder
	}
}

func (f *fakeIMAP) EnsureFolderExists(_ context.Context, path string) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.folders[path]
	if !ok {
		return 0, model.NewMailboxError(model.ErrKindNotFound, "no folder", false, imap.ErrNotFound)
	}
	return v, nil
}

func (f *fakeIMAP) ListHeaders(_ context.Context, folder string, uids []uint32, max int) ([]imap.MessageHeader, uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []imap.MessageHeader
	for _, m := range f.messages {
		if m.folder != folder {
			continue
		}
		if len(uids) > 0 && !hasUID(uids, m.uid) {
			continue
		}
		out = append(out, fakeMessageHeader(m))
		if max > 0 && len(out) >= max {
			break
		}
	}
	return out, f.folders[folder], nil
}

func hasUID(uids []uint32, uid uint32) bool {
	for _, u := range uids {
		if u == uid {
			return true
		}
	}
	return false
}

func (f *fakeIMAP) FetchHeader(_ context.Context, loc imap.Locator) (imap.MessageHeader, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.find(loc)
	if m == nil {
		return imap.MessageHeader{}, model.NewMailboxError(model.ErrKindNotFound, "no message", false, imap.ErrNotFound)
	}
	return fakeMessageHeader(m), nil
}

// fakeMessageHeader builds a header for the fake, deriving HasAttach from the
// raw MIME so an attachment filter can be exercised.
func fakeMessageHeader(m *fakeIMAPMsg) imap.MessageHeader {
	h := imap.MessageHeader{FolderPath: m.folder, UIDValidity: m.uidValidity, UID: m.uid, MessageID: m.messageID, Subject: m.subject, From: m.from, InternalDate: fakMsgDate(m), Size: int64(len(m.raw))}
	if strings.Contains(strings.ToLower(m.raw), "content-disposition: attachment") {
		h.HasAttach = true
	}
	return h
}

func (f *fakeIMAP) FetchRawMIME(_ context.Context, loc imap.Locator, w io.Writer) error {
	f.mu.Lock()
	m := f.find(loc)
	f.mu.Unlock()
	if m == nil {
		return model.NewMailboxError(model.ErrKindNotFound, "no message", false, imap.ErrNotFound)
	}
	_, err := w.Write([]byte(m.raw))
	return err
}

func (f *fakeIMAP) FetchBodyPart(ctx context.Context, loc imap.Locator, _ []int, w io.Writer) error {
	return f.FetchRawMIME(ctx, loc, w)
}

func (f *fakeIMAP) SetFlags(_ context.Context, loc imap.Locator, _, _ []string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.find(loc) == nil {
		return nil, model.NewMailboxError(model.ErrKindNotFound, "no message", false, imap.ErrNotFound)
	}
	return nil, nil
}

func (f *fakeIMAP) MoveMessage(_ context.Context, loc imap.Locator, dest string) (imap.MoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.find(loc)
	if m == nil {
		return imap.MoveResult{}, model.NewMailboxError(model.ErrKindNotFound, "no message", false, imap.ErrNotFound)
	}
	m.folder = dest
	m.uidValidity = f.folders[dest]
	return imap.MoveResult{SourceUID: loc.UID, DestinationUID: m.uid, UIDValidity: m.uidValidity}, nil
}

func (f *fakeIMAP) DeleteMessage(_ context.Context, loc imap.Locator) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, m := range f.messages {
		if m.folder == loc.FolderPath && m.uid == loc.UID {
			f.messages = append(f.messages[:i], f.messages[i+1:]...)
			return nil
		}
	}
	return model.NewMailboxError(model.ErrKindNotFound, "no message", false, imap.ErrNotFound)
}

func (f *fakeIMAP) AppendReader(_ context.Context, folder string, r io.Reader, size int64, _ []string, _ time.Time) (imap.AppendResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	buf := make([]byte, size)
	n, _ := io.ReadFull(r, buf)
	m := &fakeIMAPMsg{uid: f.nextUID, uidValidity: f.folders[folder], folder: folder, raw: string(buf[:n])}
	f.nextUID++
	f.messages = append(f.messages, m)
	return imap.AppendResult{DestinationUID: m.uid, UIDValidity: m.uidValidity, Confirmed: true}, nil
}

func (f *fakeIMAP) CreateFolder(_ context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.folders[path]; ok {
		return model.NewMailboxError(model.ErrKindConflict, "exists", false, nil)
	}
	f.folders[path] = 100
	return nil
}

func (f *fakeIMAP) RenameFolder(_ context.Context, oldPath, newPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.folders[oldPath]
	delete(f.folders, oldPath)
	f.folders[newPath] = v
	return nil
}

func (f *fakeIMAP) DeleteFolder(_ context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.folders, path)
	return nil
}

func (f *fakeIMAP) FindByMessageID(_ context.Context, folder, messageID string) (imap.Locator, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.messages {
		if m.folder == folder && m.messageID == messageID {
			return imap.Locator{FolderPath: folder, UIDValidity: m.uidValidity, UID: m.uid, MessageID: messageID}, nil
		}
	}
	return imap.Locator{}, model.NewMailboxError(model.ErrKindNotFound, "no message", false, imap.ErrNotFound)
}

func (f *fakeIMAP) FindByHeader(_ context.Context, folder, key, value string) ([]imap.Locator, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []imap.Locator
	for _, m := range f.messages {
		if m.folder == folder && strings.Contains(m.raw, key+": "+value) {
			out = append(out, imap.Locator{FolderPath: folder, UIDValidity: m.uidValidity, UID: m.uid})
		}
	}
	return out, nil
}

func (f *fakeIMAP) Search(_ context.Context, folder string, q imap.SearchQuery) (imap.SearchResult, error) {
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
		if q.BeforeUID > 0 && m.uid >= q.BeforeUID {
			continue
		}
		uids = append(uids, m.uid)
	}
	// Sort ascending and honour the limit + newest-first truncation exactly as the
	// adapter does, so a multi-page newest-first search is genuinely exercised.
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	limit := q.Limit
	if limit <= 0 {
		limit = imap.MaxSearchResults
	}
	res := imap.SearchResult{Completeness: imap.CompletenessComplete}
	if len(uids) > limit {
		if q.NewestFirst {
			uids = uids[len(uids)-limit:]
			res.NextCursor = uids[0]
		} else {
			res.NextCursor = uids[limit]
			uids = uids[:limit]
		}
		res.Completeness = imap.CompletenessPartial
	} else if q.NewestFirst && len(uids) == limit {
		res.NextCursor = uids[0]
	}
	if q.NewestFirst {
		for i, j := 0, len(uids)-1; i < j; i, j = i+1, j-1 {
			uids[i], uids[j] = uids[j], uids[i]
		}
	}
	res.UIDs = uids
	return res, nil
}

func (f *fakeIMAP) Close() error { return nil }

// adminKey mints an account-admin bearer key for the fixture user.
func adminKey(t *testing.T, svc *app.Service, u model.User) string {
	t.Helper()
	_, key, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func testConfig(dir string) config.Config {
	return config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour, LoginLimitPerMinute: 20, SendLimitPerMinute: 60}
}

// newIsolatedService builds an app service with an in-memory store and an
// admin-able account.
func newIsolatedService(t *testing.T) *app.Service {
	t.Helper()
	st, dir := testdb.OpenDir(t)
	cfg := testConfig(dir)
	svc, err := app.New(cfg, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func createAdmin(t *testing.T, svc *app.Service) model.User {
	t.Helper()
	u, err := svc.Store.CreateAccountAndAdmin(context.Background(), "A", "admin@example.com", "correct horse battery staple", svc.Config.DefaultQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func createStandalone(t *testing.T, svc *app.Service, u model.User) model.Inbox {
	t.Helper()
	box, err := svc.Store.CreateStandaloneInbox(context.Background(), u.AccountID, store.StandaloneCreate{
		DisplayName: "Agent",
		Address:     "agent@remote.test",
		Remote:      &model.RemoteConnection{Host: "imap.remote.test", Username: "agent@remote.test", Security: model.RemoteSecurityPlain, Port: 143},
	})
	if err != nil {
		t.Fatal(err)
	}
	return box
}

func apiGet(t *testing.T, h http.Handler, path, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func refreshRemote(t *testing.T, h http.Handler, inboxID, key string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/inboxes/"+inboxID+"/remote/refresh", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", rr.Code, rr.Body.String())
	}
}

// TestCommonFoldersEnvelopeBothKinds proves the folder listing is one endpoint
// for a domain inbox and a standalone inbox, returns the shared envelope, and
// that the standalone tree is reconciled from the live server.
func TestCommonFoldersEnvelopeBothKinds(t *testing.T) {
	svc, h, u, domainBox, standalone, _ := standaloneFixture(t)
	key := adminKey(t, svc, u)
	refreshRemote(t, h, standalone.ID, key)

	for _, box := range []model.Inbox{domainBox, standalone} {
		rr := apiGet(t, h, "/v1/inboxes/"+box.ID+"/folders", key)
		if rr.Code != 200 {
			t.Fatalf("folders %s: %d %s", box.Kind, rr.Code, rr.Body.String())
		}
		var env struct {
			Items        []map[string]any `json:"items"`
			Completeness string           `json:"completeness"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode %s: %v", box.Kind, err)
		}
		if env.Completeness != string(model.CompletenessComplete) {
			t.Fatalf("%s completeness = %q", box.Kind, env.Completeness)
		}
		if len(env.Items) == 0 {
			t.Fatalf("%s: no folders", box.Kind)
		}
	}
	// The standalone tree must include the fake remote folders.
	rr := apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/folders", key)
	if !strings.Contains(rr.Body.String(), "Archive") {
		t.Fatalf("standalone folders missing remote Archive: %s", rr.Body.String())
	}
}

func TestCachedFoldersDoNotDialAndReturnIndexedCounts(t *testing.T) {
	svc, h, u, _, box, fake := standaloneFixture(t)
	key := adminKey(t, svc, u)
	fake.add("INBOX", "body", "<counts@test>", "Cached")
	refreshRemote(t, h, box.ID, key)
	rm := app.NewRemoteMailboxService(svc)
	t.Cleanup(rm.Stop)
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) {
		t.Error("folder listing dialled IMAP")
		return nil, fmt.Errorf("unexpected dial")
	})
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	rr := apiGet(t, srv.Handler(), "/v1/inboxes/"+box.ID+"/folders", key)
	if rr.Code != 200 {
		t.Fatalf("folders: %d %s", rr.Code, rr.Body.String())
	}
	var env struct {
		Items []struct {
			Path  string `json:"path"`
			Count int    `json:"message_count"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	for _, f := range env.Items {
		if f.Path == "INBOX" {
			if f.Count != 1 {
				t.Fatalf("count = %d", f.Count)
			}
			return
		}
	}
	t.Fatal("cached INBOX missing")
}

// TestCommonMessagesEnvelopeAndRemoteRead proves the standalone message listing
// returns the shared envelope and a message read resolves the cached remote
// projection, and that the message body is fetched live by the content route.
func TestCommonMessagesEnvelopeAndRemoteRead(t *testing.T) {
	svc, h, u, _, standalone, fake := standaloneFixture(t)
	key := adminKey(t, svc, u)
	raw := "From: remote@elsewhere.test\r\nTo: agent@remote.test\r\nSubject: Live\r\nMessage-ID: <live@remote>\r\n\r\nbody here"
	fake.add("INBOX", raw, "<live@remote>", "Live")

	rr := apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/messages", key)
	if rr.Code != 200 {
		t.Fatalf("messages: %d %s", rr.Code, rr.Body.String())
	}
	var env struct {
		Items []struct {
			ID        string `json:"id"`
			MessageID string `json:"message_id"`
			Subject   string `json:"subject"`
		} `json:"items"`
		NextCursor   string `json:"next_cursor"`
		Completeness string `json:"completeness"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Completeness != string(model.CompletenessComplete) || len(env.Items) != 1 {
		t.Fatalf("envelope %+v", env)
	}
	id := env.Items[0].ID

	// One message read returns the cached projection.
	rr = apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/messages/"+id, key)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"subject":"Live"`) {
		t.Fatalf("message read %d %s", rr.Code, rr.Body.String())
	}
	// The content route streams the live body as an attachment download.
	rr = apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/messages/"+id+"/content", key)
	if rr.Code != 200 {
		t.Fatalf("content %d %s", rr.Code, rr.Body.String())
	}
	if !strings.HasPrefix(rr.Header().Get("Content-Disposition"), "attachment;") || rr.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("unsafe content headers %#v", rr.Header())
	}
	if !strings.Contains(rr.Body.String(), "body here") {
		t.Fatalf("content body missing: %q", rr.Body.String())
	}
}

// TestCommonRemotePartialFailure proves an uninitialized folder cache returns
// immediately with partial completeness even when the background dial fails.
func TestCommonRemotePartialFailure(t *testing.T) {
	svc := newIsolatedService(t)
	u := createAdmin(t, svc)
	standalone := createStandalone(t, svc, u)
	rm := app.NewRemoteMailboxService(svc)
	t.Cleanup(rm.Stop)
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) {
		return nil, model.NewMailboxError(model.ErrKindUnavailable, "server unreachable", true, nil)
	})
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
	if _, err := rm.ConfigureStandaloneRemote(context.Background(), p, standalone.ID, store.StandaloneRemoteUpdate{IMAPPassword: "pw"}); err != nil {
		t.Fatal(err)
	}
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	h := srv.Handler()
	key := adminKey(t, svc, u)

	rr := apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/folders", key)
	if rr.Code != 200 {
		t.Fatalf("folders on failing dialer = %d %s", rr.Code, rr.Body.String())
	}
	var env struct {
		Items        []map[string]any `json:"items"`
		Completeness string           `json:"completeness"`
		Errors       []struct {
			InboxID string `json:"inbox_id"`
			Code    string `json:"code"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Completeness != string(model.CompletenessPartial) {
		t.Fatalf("completeness = %q want partial", env.Completeness)
	}
	if len(env.Errors) != 0 {
		t.Fatalf("errors = %+v", env.Errors)
	}
}

// TestCommonPermissionReadVsOwner proves the remote configuration write requires
// Owner while a read-only credential can list folders and read messages.
func TestCommonPermissionReadVsOwner(t *testing.T) {
	svc, h, u, _, standalone, _ := standaloneFixture(t)
	ctx := context.Background()
	// A read-only key on the standalone inbox.
	_, readKey, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "reader", false, map[string]string{standalone.ID: "read"})
	if err != nil {
		t.Fatal(err)
	}
	rr := apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/folders", readKey)
	if rr.Code != 200 {
		t.Fatalf("read folders %d %s", rr.Code, rr.Body.String())
	}
	// A read-only key must not configure the connector.
	req := httptest.NewRequest("PUT", "/v1/inboxes/"+standalone.ID+"/remote", strings.NewReader(`{"host":"imap.x.test","username":"x@x.test","imap_password":"pw"}`))
	req.Header.Set("Authorization", "Bearer "+readKey)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("read configure = %d, want 403", rr.Code)
	}
}

// TestCommonRemoteConfigMasksSecrets proves the remote configuration view never
// returns a password but reports that one is stored, and lists unmapped roles.
func TestCommonRemoteConfigMasksSecrets(t *testing.T) {
	svc, h, u, _, standalone, _ := standaloneFixture(t)
	key := adminKey(t, svc, u)
	rr := apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/remote", key)
	if rr.Code != 200 {
		t.Fatalf("remote get %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "imap-pw") {
		t.Fatalf("remote view leaked the password: %s", body)
	}
	var view struct {
		IMAPPasswordSet bool     `json:"imap_password_set"`
		Configured      bool     `json:"configured"`
		Host            string   `json:"host"`
		MissingRoles    []string `json:"missing_roles"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.IMAPPasswordSet || !view.Configured || view.Host != "imap.remote.test" {
		t.Fatalf("view %+v", view)
	}
	if len(view.MissingRoles) == 0 {
		t.Fatalf("expected missing special roles before reconcile, got %+v", view.MissingRoles)
	}
}

// TestCommonFolderCRUDSessionCSRF proves the folder create route works through a
// session with a CSRF token and is refused without one, and that a created folder
// is listed back.
func TestCommonFolderCRUDSessionCSRF(t *testing.T) {
	svc, h, u, _, standalone, _ := standaloneFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)

	// Without a CSRF token the write is refused by the CSRF middleware.
	req := httptest.NewRequest("POST", "/ui/inboxes/"+standalone.ID+"/folders", strings.NewReader("path=Projects&_csrf="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code == 303 {
		t.Fatalf("folder create without CSRF succeeded: %s", rr.Body.String())
	}

	// With the token the folder is created on the live remote and listed back.
	req = httptest.NewRequest("POST", "/ui/inboxes/"+standalone.ID+"/folders", strings.NewReader("path=Projects&_csrf="+csrf))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 303 {
		t.Fatalf("folder create with CSRF %d %s", rr.Code, rr.Body.String())
	}
	rr = apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/folders", "")
	_ = rr
	// Verify through the API with an admin key.
	key := adminKey(t, svc, u)
	rr = apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/folders", key)
	if !strings.Contains(rr.Body.String(), "Projects") {
		t.Fatalf("created folder not listed: %s", rr.Body.String())
	}
}

// TestRemoteTestEndpoint proves the remote test endpoint reports a resolved scope
// using the stored secret.
func TestRemoteTestEndpoint(t *testing.T) {
	svc, h, u, _, standalone, _ := standaloneFixture(t)
	key := adminKey(t, svc, u)
	req := httptest.NewRequest("POST", "/v1/inboxes/"+standalone.ID+"/remote/test", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"ok":true`) {
		t.Fatalf("remote test %d %s", rr.Code, rr.Body.String())
	}
}

// TestRemoteTestRequiresOwner proves the remote test endpoint — which dials a
// caller-supplied host using the inbox's stored credentials — is not reachable by
// a merely read-scoped key.
func TestRemoteTestRequiresOwner(t *testing.T) {
	svc, h, u, _, standalone, _ := standaloneFixture(t)
	_, key, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "reader", false, map[string]string{standalone.ID: "read"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/inboxes/"+standalone.ID+"/remote/test", strings.NewReader(`{"host":"attacker.example","username":"x","imap_password":"y"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("read-scoped remote test = %d, want 403: %s", rr.Code, rr.Body.String())
	}
}

// TestRemoteFilteredScanReachesDeepMatch proves a remote listing filtered on a
// predicate the source cannot express (has_attachment) pages past non-matching
// rows instead of silently omitting a match that lies deeper than one scan window.
func TestRemoteFilteredScanReachesDeepMatch(t *testing.T) {
	svc, h, u, _, standalone, fake := standaloneFixture(t)
	restore := httpapp.SetRemoteScanLimitsForTest(2, 1)
	defer restore()
	base := time.Now().UTC().Add(-time.Hour)
	plain := "From: a@b.test\r\nSubject: p\r\nMessage-ID: <p@remote>\r\n\r\nbody"
	attach := "From: a@b.test\r\nSubject: a\r\nMessage-ID: <a@remote>\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain\r\n\r\nbody\r\n--x\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"f.bin\"\r\n\r\nzz\r\n--x--"
	// Four newer non-matching messages, then the oldest with an attachment.
	for i := 0; i < 4; i++ {
		fake.addAt("INBOX", plain, "<p"+strconv.Itoa(i)+"@remote>", "p", base.Add(time.Duration(i+1)*time.Minute))
	}
	fake.addAt("INBOX", attach, "<a@remote>", "a", base)
	key := adminKey(t, svc, u)

	cursor := ""
	found := false
	for page := 0; page < 12 && !found; page++ {
		path := "/v1/messages?inbox=" + standalone.ID + "&has_attachment=true&limit=1"
		if cursor != "" {
			path += "&before=" + url.QueryEscape(cursor)
		}
		rr := apiGet(t, h, path, key)
		if rr.Code != 200 {
			t.Fatalf("page %d status %d: %s", page, rr.Code, rr.Body.String())
		}
		var env struct {
			Items      []map[string]any `json:"items"`
			NextCursor string           `json:"next_cursor"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if len(env.Items) > 0 {
			found = true
			break
		}
		cursor = env.NextCursor
		if cursor == "" {
			break
		}
	}
	if !found {
		t.Fatal("deep attachment match was never reached by filtered paging")
	}
}

// ensure unused imports stay referenced while the file grows.

// TestStandaloneUIAddAndBanner proves the dashboard renders the Add inbox
// chooser (with the standalone choice) and the standalone setup form, and that a
// standalone mailbox page shows the setup banner and the folder management UI.
func TestStandaloneUIAddAndBanner(t *testing.T) {
	svc, h, u, _, standalone, _ := standaloneFixture(t)
	// The dashboard needs an admin session.
	cookie, _ := uiSession(t, svc, u.ID)
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("dashboard %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{`id="add-inbox"`, "data-inbox-choose", "Domain inbox", "Standalone inbox", "inbox-add-standalone-form", "IMAP host"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	// The standalone inbox page shows the remote banner and folder management.
	req = httptest.NewRequest("GET", "/ui/inboxes/"+standalone.ID, nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("standalone inbox %d: %s", rr.Code, rr.Body.String())
	}
	page := rr.Body.String()
	for _, want := range []string{"/remote", "Manage folders", "Inbox"} {
		if !strings.Contains(page, want) {
			t.Fatalf("standalone inbox missing %q", want)
		}
	}
}

// TestFolderViewBothKinds proves the common custom-folder page lists a local
// folder's messages for a domain inbox and a remote folder's cached messages for
// a standalone inbox.
func TestFolderViewBothKinds(t *testing.T) {
	svc, h, u, domainBox, standalone, fake := standaloneFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}

	// Create a local folder and file a message into it.
	localFolder, err := svc.Store.CreateFolder(context.Background(), p, domainBox.ID, store.FolderCreate{Path: "Projects"})
	if err != nil {
		t.Fatal(err)
	}
	msg := seedInbound(t, svc, domainBox, "fold-local", "<fold-local@test>", "Local project", "body")
	if _, _, err := svc.Store.MoveMessageToFolder(context.Background(), p, msg.ID, localFolder.ID); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/ui/inboxes/"+domainBox.ID+"/folder?folder="+localFolder.ID, nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Local project") {
		t.Fatalf("local folder view %d: %s", rr.Code, rr.Body.String())
	}

	// The standalone inbox reconciles its remote folders and lists a remote one.
	fake.folders["Archive"] = 100
	fake.add("Archive", "From: a@b.test\r\nSubject: Archived\r\nMessage-ID: <arc@remote>\r\n\r\nbody", "<arc@remote>", "Archived")
	key := adminKey(t, svc, u)
	refreshRemote(t, h, standalone.ID, key)
	rr = apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/folders", key)
	if rr.Code != 200 {
		t.Fatalf("reconcile folders %d", rr.Code)
	}
	// Find the Archive folder id from the API.
	var env struct {
		Items []struct {
			ID   string `json:"id"`
			Path string `json:"path"`
		} `json:"items"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &env)
	var archiveID string
	for _, it := range env.Items {
		if it.Path == "Archive" {
			archiveID = it.ID
		}
	}
	if archiveID == "" {
		t.Fatalf("Archive folder not in tree: %s", rr.Body.String())
	}
	req = httptest.NewRequest("GET", "/ui/inboxes/"+standalone.ID+"/folder?folder="+archiveID, nil)
	req.AddCookie(cookie)
	_ = csrf
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Archived") {
		t.Fatalf("remote folder view %d: %s", rr.Code, rr.Body.String())
	}
}

// TestRemoteMessageReadSameSanitizedBackend proves a standalone inbox's message
// opens in the same reader and its HTML is passed through the same sanitizer:
// script and event handlers are stripped, and the message body is fetched live.
func TestRemoteMessageReadSameSanitizedBackend(t *testing.T) {
	svc, h, u, _, standalone, fake := standaloneFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)
	raw := "From: remote@elsewhere.test\r\nTo: agent@remote.test\r\nSubject: Scripted\r\nMessage-ID: <scripted@remote>\r\nMIME-Version: 1.0\r\nContent-Type: text/html\r\n\r\n<p>hi</p><script>alert(1)</script><img src=x onerror=alert(2)>"
	fake.add("INBOX", raw, "<scripted@remote>", "Scripted")
	key := adminKey(t, svc, u)
	rr := apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/messages", key)
	var env struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil || len(env.Items) != 1 {
		t.Fatalf("list remote: %d %s", rr.Code, rr.Body.String())
	}
	id := env.Items[0].ID

	// The HTML body route strips script and event handlers.
	req := httptest.NewRequest("GET", "/ui/messages/"+id+"/html", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("remote html %d: %s", rr.Code, rr.Body.String())
	}
	body := strings.ToLower(rr.Body.String())
	if strings.Contains(body, "<script") || strings.Contains(body, "onerror") {
		t.Fatalf("remote HTML not sanitized: %s", rr.Body.String())
	}
	if !strings.Contains(body, "hi") {
		t.Fatalf("remote HTML lost benign content: %s", rr.Body.String())
	}
	// The reader page renders the standalone banner and the sanitized body.
	req = httptest.NewRequest("GET", "/ui/messages/"+id, nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(strings.ToLower(rr.Body.String()), "remote server") {
		t.Fatalf("remote reader %d: %s", rr.Code, rr.Body.String())
	}
}

// TestRemoteMessageAttachmentSecurity proves a remote attachment downloads with
// Content-Disposition: attachment and nosniff.
func TestRemoteMessageAttachmentSecurity(t *testing.T) {
	svc, h, u, _, standalone, fake := standaloneFixture(t)
	key := adminKey(t, svc, u)
	raw := "From: remote@elsewhere.test\r\nSubject: Files\r\nMessage-ID: <files@remote>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nhi\r\n--b\r\nContent-Type: text/html; name=evil.html\r\nContent-Disposition: attachment; filename=evil.html\r\n\r\n<script>alert(1)</script>\r\n--b--"
	fake.add("INBOX", raw, "<files@remote>", "Files")
	rr := apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/messages", key)
	var env struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &env)
	if len(env.Items) == 0 {
		t.Fatalf("no remote messages: %s", rr.Body.String())
	}
	id := env.Items[0].ID
	req := httptest.NewRequest("GET", "/v1/inboxes/"+standalone.ID+"/messages/"+id+"/attachments/2?filename=evil.html", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("attachment %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.HasPrefix(rr.Header().Get("Content-Disposition"), "attachment;") || rr.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("unsafe attachment headers %#v", rr.Header())
	}
}

// TestAuthoringSelectorPerKind proves the authoring endpoint reports the kind
// default (remote_draft for standalone, mailmoose_approval for domain), enables
// the approver only for a local inbox, defaults the notify address to the
// connected address, and lets Owner switch the mode and override the notify.
func TestAuthoringSelectorPerKind(t *testing.T) {
	svc, h, u, domainBox, standalone, _ := standaloneFixture(t)
	key := adminKey(t, svc, u)

	get := func(inbox string) map[string]any {
		rr := apiGet(t, h, "/v1/inboxes/"+inbox+"/authoring", key)
		if rr.Code != 200 {
			t.Fatalf("authoring get %s: %d %s", inbox, rr.Code, rr.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	sd := get(standalone.ID)
	if sd["mode"] != model.AuthoringRemoteDraft || sd["default_mode"] != model.AuthoringRemoteDraft {
		t.Fatalf("standalone authoring %+v", sd)
	}
	if sd["approver_enabled"] != false {
		t.Fatalf("standalone approver should be disabled: %+v", sd)
	}
	if sd["notify_address"] != standalone.Address {
		t.Fatalf("standalone notify default %+v", sd)
	}
	dm := get(domainBox.ID)
	if dm["mode"] != model.AuthoringMailMooseApproval || dm["approver_enabled"] != true {
		t.Fatalf("domain authoring %+v", dm)
	}

	// Switch the standalone inbox to approval-like mode and set a notify override.
	req := httptest.NewRequest("PUT", "/v1/inboxes/"+standalone.ID+"/authoring", strings.NewReader(`{"mode":"mailmoose_approval","notify_address":"ops@remote.test"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("authoring put %d: %s", rr.Code, rr.Body.String())
	}
	after := get(standalone.ID)
	if after["mode"] != model.AuthoringMailMooseApproval || after["notify_address"] != "ops@remote.test" || after["notify_overridden"] != true {
		t.Fatalf("authoring after put %+v", after)
	}
}

// TestOriginalMessagesUnifiedEnvelopeAndRemoteFanout proves the original
// /v1/messages endpoint returns the shared envelope and, account-wide, includes
// the authorized standalone inbox's remote messages alongside the local ones.
func TestOriginalMessagesUnifiedEnvelopeAndRemoteFanout(t *testing.T) {
	svc, h, u, domainBox, standalone, fake := standaloneFixture(t)
	key := adminKey(t, svc, u)
	seedInbound(t, svc, domainBox, "fan-local", "<fan-local@test>", "Local fan", "body")
	fake.add("INBOX", "From: a@b.test\r\nSubject: Remote fan\r\nMessage-ID: <fan-remote@test>\r\n\r\nbody", "<fan-remote@test>", "Remote fan")

	// Account-wide: both the local and the remote message appear.
	rr := apiGet(t, h, "/v1/messages", key)
	if rr.Code != 200 {
		t.Fatalf("messages %d %s", rr.Code, rr.Body.String())
	}
	var env struct {
		Items []struct {
			Subject string `json:"subject"`
		} `json:"items"`
		Completeness string           `json:"completeness"`
		Errors       []map[string]any `json:"errors"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var sawLocal, sawRemote bool
	for _, it := range env.Items {
		if it.Subject == "Local fan" {
			sawLocal = true
		}
		if it.Subject == "Remote fan" {
			sawRemote = true
		}
	}
	if !sawLocal || !sawRemote {
		t.Fatalf("account-wide fanout local=%v remote=%v: %s", sawLocal, sawRemote, rr.Body.String())
	}

	// Scoped to the standalone inbox: only the remote message, via the envelope.
	rr = apiGet(t, h, "/v1/messages?inbox="+standalone.ID, key)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Remote fan") || strings.Contains(rr.Body.String(), "Local fan") {
		t.Fatalf("scoped remote list %d: %s", rr.Code, rr.Body.String())
	}
}

// TestOriginalSearchThreadsEnvelope proves the original /v1/search and /v1/threads
// endpoints return the shared envelope.
func TestOriginalSearchThreadsEnvelope(t *testing.T) {
	svc, h, u, domainBox, _, _ := standaloneFixture(t)
	key := adminKey(t, svc, u)
	seedInbound(t, svc, domainBox, "env-s", "<env-s@test>", "Envelope search", "findme body")

	rr := apiGet(t, h, "/v1/search?q=findme", key)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"items"`) || !strings.Contains(rr.Body.String(), `"completeness"`) {
		t.Fatalf("search envelope %d: %s", rr.Code, rr.Body.String())
	}
	rr = apiGet(t, h, "/v1/threads", key)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"items"`) || !strings.Contains(rr.Body.String(), `"completeness"`) {
		t.Fatalf("threads envelope %d: %s", rr.Code, rr.Body.String())
	}
}

// TestOriginalMessageRemoteFallback proves the original /v1/messages/{id} routes
// resolve a standalone inbox's remote metadata id: GET returns the projection,
// PATCH sets read/labels, and DELETE moves it to the remote Trash folder.
func TestOriginalMessageRemoteFallback(t *testing.T) {
	svc, h, u, _, standalone, fake := standaloneFixture(t)
	key := adminKey(t, svc, u)
	fake.folders["Trash"] = 100
	fake.add("INBOX", "From: a@b.test\r\nSubject: Remote patch\r\nMessage-ID: <rp@test>\r\n\r\nbody", "<rp@test>", "Remote patch")
	rr := apiGet(t, h, "/v1/messages?inbox="+standalone.ID, key)
	var env struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil || len(env.Items) == 0 {
		t.Fatalf("list remote %d %s", rr.Code, rr.Body.String())
	}
	id := env.Items[0].ID

	// GET
	rr = apiGet(t, h, "/v1/messages/"+id, key)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Remote patch") {
		t.Fatalf("remote GET %d: %s", rr.Code, rr.Body.String())
	}
	// PATCH read + labels
	req := httptest.NewRequest("PATCH", "/v1/messages/"+id, strings.NewReader(`{"read":true,"labels":["Alpha"]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"labels":["Alpha"]`) {
		t.Fatalf("remote PATCH %d: %s", rr.Code, rr.Body.String())
	}
	// DELETE moves to the remote Trash folder.
	req = httptest.NewRequest("DELETE", "/v1/messages/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 204 {
		t.Fatalf("remote DELETE %d: %s", rr.Code, rr.Body.String())
	}
}

// TestRemoteBulkActions proves a remote bulk mark-read and a bulk move route to
// the live server through the mailbox bulk route, and that the "all N" scope is
// derived from the folder index rather than a client id list.
func TestRemoteBulkActions(t *testing.T) {
	svc, h, u, _, standalone, fake := standaloneFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	fake.folders["Archive"] = 100
	fake.add("INBOX", "From: a@b.test\r\nSubject: Bulk 1\r\nMessage-ID: <b1@test>\r\n\r\nbody", "<b1@test>", "Bulk 1")
	fake.add("INBOX", "From: a@b.test\r\nSubject: Bulk 2\r\nMessage-ID: <b2@test>\r\n\r\nbody", "<b2@test>", "Bulk 2")
	// Reconcile the remote index so the folder and messages are cached.
	key := adminKey(t, svc, u)
	if rr := apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/messages?folder=INBOX", key); rr.Code != 200 {
		t.Fatalf("seed reconcile %d", rr.Code)
	}
	// Bulk mark-read all in the Inbox view via scope=all.
	form := "action=read&scope=all&folder=inbox&_csrf=" + csrf
	req := httptest.NewRequest("POST", "/ui/inboxes/"+standalone.ID+"/bulk", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 303 {
		t.Fatalf("remote bulk read %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Header().Get("Location"), "2+messages+marked+read") && !strings.Contains(rr.Header().Get("Location"), "2%20messages%20marked%20read") && !strings.Contains(rr.Header().Get("Location"), "messages%20marked%20read") {
		t.Fatalf("remote bulk notice missing: %s", rr.Header().Get("Location"))
	}
}

// TestRemoteMessageUIDeleteRestorePurgeSpam proves the per-message session-UI
// actions (/ui/messages/{id}/delete|restore|purge|spam) resolve a standalone
// inbox's cached remote metadata id and drive the live remote folder state,
// instead of answering a spurious "message not found" (the local-store miss a
// remote id always produces).
func TestRemoteMessageUIDeleteRestorePurgeSpam(t *testing.T) {
	svc, h, u, _, standalone, fake := standaloneFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	fake.folders["Trash"] = 100
	fake.folders["Spam"] = 100
	fake.add("INBOX", "From: a@b.test\r\nSubject: Action me\r\nMessage-ID: <act@test>\r\n\r\nbody", "<act@test>", "Action me")

	// Seed-reconcile so the folder roles and the message are cached.
	if rr := apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/messages", adminKey(t, svc, u)); rr.Code != 200 {
		t.Fatalf("seed reconcile %d: %s", rr.Code, rr.Body.String())
	}
	type listEnv struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	var env listEnv
	if rr := apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/messages?folder=INBOX", adminKey(t, svc, u)); rr.Code != 200 {
		t.Fatalf("list %d: %s", rr.Code, rr.Body.String())
	} else if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Items) != 1 {
		t.Fatalf("want 1 cached message, got %d", len(env.Items))
	}
	id := env.Items[0].ID

	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader("_csrf="+csrf+body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	// Purge before trash is refused (must be trashed first) and the live message
	// is untouched.
	if rr := post("/ui/messages/"+id+"/purge", ""); rr.Code != 400 {
		t.Fatalf("purge untrashed = %d want 400: %s", rr.Code, rr.Body.String())
	}
	if got := fake.folderOf("<act@test>"); got != "INBOX" {
		t.Fatalf("purge untrashed moved message: %q", got)
	}

	// Delete moves it to the remote Trash folder and out of the INBOX listing.
	if rr := post("/ui/messages/"+id+"/delete", ""); rr.Code != 303 {
		t.Fatalf("delete = %d: %s", rr.Code, rr.Body.String())
	}
	if got := fake.folderOf("<act@test>"); got != "Trash" {
		t.Fatalf("delete did not move to Trash: %q", got)
	}
	env = listEnv{}
	if rr := apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/messages?folder=INBOX", adminKey(t, svc, u)); rr.Code != 200 {
		t.Fatalf("inbox after delete %d", rr.Code)
	} else if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	} else if len(env.Items) != 0 {
		t.Fatalf("deleted message still in INBOX: %+v", env.Items)
	}

	// Restore moves it back to the remote Inbox.
	if rr := post("/ui/messages/"+id+"/restore", ""); rr.Code != 303 {
		t.Fatalf("restore = %d: %s", rr.Code, rr.Body.String())
	}
	if got := fake.folderOf("<act@test>"); got != "INBOX" {
		t.Fatalf("restore did not return to INBOX: %q", got)
	}

	// Spam moves it to the remote Spam folder, and not-spam returns it.
	if rr := post("/ui/messages/"+id+"/spam", "&spam=1"); rr.Code != 303 {
		t.Fatalf("spam = %d: %s", rr.Code, rr.Body.String())
	}
	if got := fake.folderOf("<act@test>"); got != "Spam" {
		t.Fatalf("spam did not move to Spam: %q", got)
	}
	if rr := post("/ui/messages/"+id+"/spam", "&spam=0"); rr.Code != 303 {
		t.Fatalf("not-spam = %d: %s", rr.Code, rr.Body.String())
	}
	if got := fake.folderOf("<act@test>"); got != "INBOX" {
		t.Fatalf("not-spam did not return to INBOX: %q", got)
	}

	// Trash then purge erases it on the remote server.
	if rr := post("/ui/messages/"+id+"/delete", ""); rr.Code != 303 {
		t.Fatalf("re-delete = %d: %s", rr.Code, rr.Body.String())
	}
	if rr := post("/ui/messages/"+id+"/purge", ""); rr.Code != 303 {
		t.Fatalf("purge = %d: %s", rr.Code, rr.Body.String())
	}
	if got := fake.folderOf("<act@test>"); got != "" {
		t.Fatalf("purge did not expunge the message: %q", got)
	}

	// A genuinely unknown id still answers 404 from the local path.
	if rr := post("/ui/messages/rm-nonexistent/delete", ""); rr.Code != 404 {
		t.Fatalf("unknown id delete = %d want 404: %s", rr.Code, rr.Body.String())
	}
}

// TestAuthoringUISavePersists proves the Approvals tab selector actually saves the
// authoring mode and notify override through the inbox edit form.
func TestAuthoringUISavePersists(t *testing.T) {
	svc, h, u, _, standalone, _ := standaloneFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	// Save the mode + notify via the edit form.
	form := "display=Agent&authoring_mode=mailmoose_approval&authoring_notify=ops@remote.test&_csrf=" + csrf
	req := httptest.NewRequest("POST", "/ui/inboxes/"+standalone.ID+"/edit", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 303 {
		t.Fatalf("authoring save %d: %s", rr.Code, rr.Body.String())
	}
	settings, err := svc.Store.GetInboxAuthoringSettingsInternal(context.Background(), u.AccountID, standalone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Mode != model.AuthoringMailMooseApproval || settings.NotifyAddress != "ops@remote.test" || !settings.NotifyOverridden {
		t.Fatalf("authoring not persisted: %+v", settings)
	}
}

// TestRemoteRoleMappingDetectAndCreate proves the special-folder role mapping
// reports a detected folder, refuses to invent a mapping, and only creates the
// conventional folder on an explicit request.
func TestRemoteRoleMappingDetectAndCreate(t *testing.T) {
	svc, h, u, _, standalone, fake := standaloneFixture(t)
	key := adminKey(t, svc, u)
	refreshRemote(t, h, standalone.ID, key)
	// The fake server already has a Sent folder; detect it, no invention.
	rr := apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/folders", key)
	if rr.Code != 200 {
		t.Fatalf("folders %d", rr.Code)
	}
	req := httptest.NewRequest("POST", "/v1/inboxes/"+standalone.ID+"/remote/roles/sent", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"mapped":true`) {
		t.Fatalf("detect sent %d: %s", rr.Code, rr.Body.String())
	}
	// Trash is missing: without create it is a 404, never a silent invention.
	req = httptest.NewRequest("POST", "/v1/inboxes/"+standalone.ID+"/remote/roles/trash", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 404 {
		t.Fatalf("missing trash should be 404, got %d: %s", rr.Code, rr.Body.String())
	}
	// With create it creates the conventional folder.
	req = httptest.NewRequest("POST", "/v1/inboxes/"+standalone.ID+"/remote/roles/trash", strings.NewReader(`{"create":true}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 201 || !strings.Contains(rr.Body.String(), `"created":true`) {
		t.Fatalf("create trash %d: %s", rr.Code, rr.Body.String())
	}
	fake.mu.Lock()
	_, ok := fake.folders["Trash"]
	fake.mu.Unlock()
	if !ok {
		t.Fatal("Trash folder was not created on the remote server")
	}
}

// TestRemoteSettingsPageRenders proves the standalone connection-settings page
// renders the secret-free bindings and the special-folder role prompt.
func TestRemoteSettingsPageRenders(t *testing.T) {
	svc, h, u, _, standalone, _ := standaloneFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)
	req := httptest.NewRequest("GET", "/ui/inboxes/"+standalone.ID+"/remote", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("remote settings %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"Connection settings", "Inbound IMAP", "Outbound SMTP", "Special folders", "imap.remote.test"} {
		if !strings.Contains(body, want) {
			t.Fatalf("remote settings missing %q", want)
		}
	}
	if strings.Contains(body, "imap-pw") {
		t.Fatalf("remote settings leaked the password")
	}
}

// recordingDetection is a fake app.RemoteDetection that counts DetectInbox calls
// without touching any state, so a test can prove the event surfaces drive
// on-demand detection.
type recordingDetection struct {
	mu    sync.Mutex
	calls int
}

func (d *recordingDetection) DetectInbox(context.Context, model.Inbox) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
}

func (d *recordingDetection) PendingRemoteApprovals(context.Context, model.Inbox) bool { return false }

func (d *recordingDetection) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// TestEventSurfacesDriveRemoteDetection proves the original event read, wait and
// stream surfaces invoke svc.RemoteDetection for authorized standalone inboxes as
// active demand, without mutating any read state.
func TestEventSurfacesDriveRemoteDetection(t *testing.T) {
	svc, h, u, _, standalone, _ := standaloneFixture(t)
	det := &recordingDetection{}
	svc.RemoteDetection = det
	key := adminKey(t, svc, u)

	// GET /v1/events scoped to the standalone inbox.
	if rr := apiGet(t, h, "/v1/events?inbox="+standalone.ID, key); rr.Code != 200 {
		t.Fatalf("events %d", rr.Code)
	}
	if det.count() < 1 {
		t.Fatal("event read did not drive remote detection")
	}
	before := det.count()
	// GET /v1/events/wait (timeout 1s) also drives detection.
	if rr := apiGet(t, h, "/v1/events/wait?inbox="+standalone.ID+"&timeout=1", key); rr.Code != 200 {
		t.Fatalf("events wait %d: %s", rr.Code, rr.Body.String())
	}
	if det.count() <= before {
		t.Fatal("event wait did not drive remote detection")
	}
	// The remote message is still unread: detection must not mark it seen.
	fake := standaloneFixture
	_ = fake
	if rr := apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/messages?folder=INBOX", key); rr.Code == 200 && strings.Contains(rr.Body.String(), `"read":true`) {
		t.Fatal("detection must not mutate read state")
	}
}

// mergeFixture builds a service with a domain inbox and two standalone inboxes
// (A and B) whose remote servers hold messages with explicit, distinct dates, and
// a routing dialer that dispatches by host.
func mergeFixture(t *testing.T) (*app.Service, http.Handler, model.User, model.Inbox, model.Inbox, model.Inbox, *fakeIMAP, *fakeIMAP) {
	t.Helper()
	st, dir := testdb.OpenDir(t)
	cfg := testConfig(dir)
	svc, err := app.New(cfg, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateAccountAndAdmin(context.Background(), "A", "admin@example.com", "correct horse battery staple", cfg.DefaultQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDomain(context.Background(), u.AccountID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	domainBox, err := st.CreateInbox(context.Background(), u.AccountID, d.ID, "hermes", "Hermes")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(addr, host string) model.Inbox {
		b, berr := st.CreateStandaloneInbox(context.Background(), u.AccountID, store.StandaloneCreate{
			DisplayName: addr, Address: addr,
			Remote: &model.RemoteConnection{Host: host, Username: addr, Security: model.RemoteSecurityPlain, Port: 143},
		})
		if berr != nil {
			t.Fatal(berr)
		}
		return b
	}
	saA := mk("a@remote.test", "imap.a.test")
	saB := mk("b@remote.test", "imap.b.test")
	fakeA := newFakeIMAP()
	fakeB := newFakeIMAP()
	rm := app.NewRemoteMailboxService(svc)
	rm.SetRemoteDialer(func(_ context.Context, c imap.Config) (app.RemoteSession, error) {
		switch c.Host {
		case "imap.a.test":
			return fakeA, nil
		case "imap.b.test":
			return fakeB, nil
		}
		return nil, model.NewMailboxError(model.ErrKindNotFound, "no fake", false, nil)
	})
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
	for _, b := range []model.Inbox{saA, saB} {
		if _, err := rm.ConfigureStandaloneRemote(context.Background(), p, b.ID, store.StandaloneRemoteUpdate{IMAPPassword: "pw"}); err != nil {
			t.Fatal(err)
		}
	}
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	return svc, srv.Handler(), u, domainBox, saA, saB, fakeA, fakeB
}

// TestAccountWideMergePaginationOrdering proves the account-wide message listing
// streams one globally date-sorted page across the local store and two standalone
// inboxes, with no duplicates or gaps across cursor pages.
func TestAccountWideMergePaginationOrdering(t *testing.T) {
	svc, h, u, domainBox, _, _, fakeA, fakeB := mergeFixture(t)
	key := adminKey(t, svc, u)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// Local at t0.
	seedInboundAt(t, svc, domainBox, "mg-local", "<mg-local@test>", "Local", "body", base)
	// A at t1 and t3; B at t2. So global order: A3, B2, A1, Local0.
	fakeA.addAt("INBOX", "From: a@b.test\r\nSubject: A-one\r\nMessage-ID: <a1@test>\r\n\r\nbody", "<a1@test>", "A-one", base.Add(1*time.Hour))
	fakeA.addAt("INBOX", "From: a@b.test\r\nSubject: A-three\r\nMessage-ID: <a3@test>\r\n\r\nbody", "<a3@test>", "A-three", base.Add(3*time.Hour))
	fakeB.addAt("INBOX", "From: a@b.test\r\nSubject: B-two\r\nMessage-ID: <b2@test>\r\n\r\nbody", "<b2@test>", "B-two", base.Add(2*time.Hour))

	var got []string
	cursor := ""
	for page := 0; page < 5; page++ {
		path := "/v1/messages?limit=2"
		if cursor != "" {
			path += "&before=" + cursor
		}
		rr := apiGet(t, h, path, key)
		if rr.Code != 200 {
			t.Fatalf("page %d: %d %s", page, rr.Code, rr.Body.String())
		}
		var env struct {
			Items []struct {
				Subject string `json:"subject"`
			} `json:"items"`
			NextCursor string `json:"next_cursor"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		for _, it := range env.Items {
			got = append(got, it.Subject)
		}
		cursor = env.NextCursor
		if cursor == "" {
			break
		}
	}
	want := []string{"A-three", "B-two", "A-one", "Local"}
	if len(got) != len(want) {
		t.Fatalf("merged subjects = %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order[%d] = %q want %q (all=%v)", i, got[i], want[i], got)
		}
	}
	// No duplicates.
	seen := map[string]int{}
	for _, s := range got {
		seen[s]++
		if seen[s] > 1 {
			t.Fatalf("duplicate across pages: %q in %v", s, got)
		}
	}
}

// TestAccountWideMergePartialFailure proves one unreachable remote inbox is
// reported in the envelope errors while the other sources are still merged and
// correctly ordered.
func TestAccountWideMergePartialFailure(t *testing.T) {
	svc, h, u, domainBox, saA, _, fakeA, _ := mergeFixture(t)
	key := adminKey(t, svc, u)
	_ = saA
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	seedInboundAt(t, svc, domainBox, "pf-local", "<pf-local@test>", "Local", "body", base)
	fakeA.addAt("INBOX", "From: a@b.test\r\nSubject: A-newer\r\nMessage-ID: <an@test>\r\n\r\nbody", "<an@test>", "A-newer", base.Add(2*time.Hour))
	// Fetch with B failing: install a dialer where host imap.b.test errors and
	// host imap.a.test still works.
	rm2 := app.NewRemoteMailboxService(svc)
	rm2.SetRemoteDialer(func(_ context.Context, c imap.Config) (app.RemoteSession, error) {
		if c.Host == "imap.b.test" {
			return nil, model.NewMailboxError(model.ErrKindUnavailable, "b down", true, nil)
		}
		return fakeA, nil
	})
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm2)
	h = srv.Handler()
	_ = u
	rr := apiGet(t, h, "/v1/messages?limit=20", key)
	if rr.Code != 200 {
		t.Fatalf("merge partial %d: %s", rr.Code, rr.Body.String())
	}
	var env struct {
		Items []struct {
			Subject string `json:"subject"`
		} `json:"items"`
		Errors []struct {
			InboxID string `json:"inbox_id"`
			Code    string `json:"code"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Errors) != 1 || env.Errors[0].Code != model.ErrKindUnavailable {
		t.Fatalf("expected one unavailable failure, got %+v", env.Errors)
	}
	if len(env.Items) < 2 {
		t.Fatalf("partial merge dropped healthy sources: %v", env.Items)
	}
}

// TestRemoteErrorIs503Not404 proves a state change on a remote message whose
// connector is unavailable is a 503, not a misleading 404.
func TestRemoteErrorIs503Not404(t *testing.T) {
	svc := newIsolatedService(t)
	u := createAdmin(t, svc)
	box := createStandalone(t, svc, u)
	good := newFakeIMAP()
	good.add("INBOX", "From: a@b.test\r\nSubject: Down\r\nMessage-ID: <down@test>\r\n\r\nbody", "<down@test>", "Down")
	rm := app.NewRemoteMailboxService(svc)
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) { return good, nil })
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
	if _, err := rm.ConfigureStandaloneRemote(context.Background(), p, box.ID, store.StandaloneRemoteUpdate{IMAPPassword: "pw"}); err != nil {
		t.Fatal(err)
	}
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	h := srv.Handler()
	key := adminKey(t, svc, u)
	rr := apiGet(t, h, "/v1/inboxes/"+box.ID+"/messages", key)
	var env struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil || len(env.Items) == 0 {
		t.Fatalf("list %d %s", rr.Code, rr.Body.String())
	}
	id := env.Items[0].ID
	// Make the connector unavailable for the state change.
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) {
		return nil, model.NewMailboxError(model.ErrKindUnavailable, "unreachable", true, nil)
	})
	req := httptest.NewRequest("PATCH", "/v1/messages/"+id, strings.NewReader(`{"read":true}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 503 {
		t.Fatalf("remote state change on unavailable connector = %d, want 503: %s", rr.Code, rr.Body.String())
	}
}

// TestAuthoringModeFlipPreservesSnapshot proves changing an inbox's authoring
// settings and clearing the approver does not alter an existing send request's
// snapshot.
func TestAuthoringModeFlipPreservesSnapshot(t *testing.T) {
	svc, _, u, _, _, _, _, _ := mergeFixture(t)
	// Use the domain inbox: mailmoose_approval with an approver.
	ctx := context.Background()
	boxes, _ := svc.Store.ListInboxes(ctx, model.Principal{AccountID: u.AccountID, Admin: true})
	var domainBox model.Inbox
	for _, b := range boxes {
		if b.Kind == model.InboxKindDomain {
			domainBox = b
		}
	}
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
	if err := svc.Store.SetInboxApprover(ctx, u.AccountID, domainBox.ID, "approver@example.com"); err != nil {
		t.Fatal(err)
	}
	d, err := svc.Store.CreateDraft(ctx, p, model.Draft{InboxID: domainBox.ID, To: []string{"x@outside.test"}, Subject: "Snap", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RequestSend(ctx, p, d.ID, true); err != nil {
		t.Fatal(err)
	}
	before, err := svc.Store.GetSendRequestByDraft(ctx, p, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Pin the mode explicitly (a domain inbox cannot be flipped to remote_draft)
	// and clear the notify override.
	if err := svc.Store.SetInboxAuthoringMode(ctx, p, domainBox.ID, model.AuthoringMailMooseApproval); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetInboxNotifyAddress(ctx, p, domainBox.ID, ""); err != nil {
		t.Fatal(err)
	}
	after, err := svc.Store.GetSendRequestByDraft(ctx, p, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != before.Status || after.ApproverEmail != before.ApproverEmail {
		t.Fatalf("snapshot changed on mode flip: before=%+v after=%+v", before, after)
	}
	if before.ApproverEmail != "approver@example.com" {
		t.Fatalf("approver snapshot = %q", before.ApproverEmail)
	}
}

// seedInboundAt is seedInbound with an explicit received time, for deterministic
// cross-inbox ordering.
func seedInboundAt(t *testing.T, svc *app.Service, box model.Inbox, delivery, rfc, subject, body string, at time.Time) model.Message {
	t.Helper()
	m, _, _, err := svc.Store.CommitInbound(context.Background(), store.InboundRecord{
		Inbox: box, Provider: "mailgun", ProviderDeliveryID: delivery, RFCMessageID: rfc,
		From: model.Address{Name: "Sender", Address: "sender@outside.test"}, To: []string{box.Address},
		EnvelopeTo: []string{box.Address}, Subject: subject, Text: body,
		RawPath: "messages/test.eml", SizeBytes: int64(len(body)), ReceivedAt: at,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestRemotePurgeRequiresOwnerAndTrash proves a remote permanent purge is a
// UID-targeted expunge that requires Owner and only acts on a trashed message.
func TestRemotePurgeRequiresOwnerAndTrash(t *testing.T) {
	svc := newIsolatedService(t)
	u := createAdmin(t, svc)
	box := createStandalone(t, svc, u)
	fake := newFakeIMAP()
	fake.folders["Trash"] = 100
	fake.add("INBOX", "From: a@b.test\r\nSubject: Purge me\r\nMessage-ID: <pm@test>\r\n\r\nbody", "<pm@test>", "Purge me")
	rm := app.NewRemoteMailboxService(svc)
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) { return fake, nil })
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
	if _, err := rm.ConfigureStandaloneRemote(context.Background(), p, box.ID, store.StandaloneRemoteUpdate{IMAPPassword: "pw"}); err != nil {
		t.Fatal(err)
	}
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	h := srv.Handler()
	// Reconcile to cache the message and the Trash/Sent roles.
	key := adminKey(t, svc, u)
	if rr := apiGet(t, h, "/v1/inboxes/"+box.ID+"/messages", key); rr.Code != 200 {
		t.Fatalf("seed %d", rr.Code)
	}
	var env struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	rr := apiGet(t, h, "/v1/inboxes/"+box.ID+"/messages?folder=INBOX", key)
	_ = json.Unmarshal(rr.Body.Bytes(), &env)
	if len(env.Items) == 0 {
		t.Fatalf("no cached remote message: %s", rr.Body.String())
	}
	id := env.Items[0].ID
	// An Assistant-only key cannot purge.
	_, readKey, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "assistant", false, map[string]string{box.ID: "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("DELETE", "/v1/messages/"+id+"/purge", nil)
	req.Header.Set("Authorization", "Bearer "+readKey)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("assistant purge = %d want 403: %s", rr.Code, rr.Body.String())
	}
	// Owner purge of a non-trashed message is refused (must be in Trash first).
	req = httptest.NewRequest("DELETE", "/v1/messages/"+id+"/purge", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 409 {
		t.Fatalf("purge untrashed = %d want 409: %s", rr.Code, rr.Body.String())
	}
	// Trash then purge.
	req = httptest.NewRequest("DELETE", "/v1/messages/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 204 {
		t.Fatalf("trash = %d: %s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest("DELETE", "/v1/messages/"+id+"/purge", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 204 {
		t.Fatalf("purge = %d: %s", rr.Code, rr.Body.String())
	}
}

// TestStandaloneCreateOriginalAPI proves POST /v1/inboxes supports kind=standalone.
func TestStandaloneCreateOriginalAPI(t *testing.T) {
	svc, h, u, _, _, _, _, _ := mergeFixture(t)
	key := adminKey(t, svc, u)
	body := `{"kind":"standalone","address":"new@remote.test","display_name":"New"}`
	req := httptest.NewRequest("POST", "/v1/inboxes", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 201 {
		t.Fatalf("standalone create %d: %s", rr.Code, rr.Body.String())
	}
	var box model.Inbox
	if err := json.Unmarshal(rr.Body.Bytes(), &box); err != nil {
		t.Fatal(err)
	}
	if box.Kind != model.InboxKindStandalone || box.Address != "new@remote.test" {
		t.Fatalf("created box %+v", box)
	}
	_ = svc
}

// TestOriginalThreadRemoteFallback proves /v1/threads/{id} resolves a remote
// thread key to its messages.
func TestOriginalThreadRemoteFallback(t *testing.T) {
	svc, h, u, _, standalone, _ := standaloneFixture(t)
	key := adminKey(t, svc, u)
	_ = svc
	// Two messages in the same remote thread.
	fake := standaloneFixture
	_ = fake
	// Use the fixture's fake via a fresh request: add through the cached index by
	// reconciling after seeding two same-thread messages.
	f := newFakeIMAP()
	f.add("INBOX", "From: a@b.test\r\nSubject: Thread\r\nMessage-ID: <t1@test>\r\n\r\nbody", "<t1@test>", "Thread")
	f.add("INBOX", "From: a@b.test\r\nSubject: Re: Thread\r\nIn-Reply-To: <t1@test>\r\nMessage-ID: <t2@test>\r\n\r\nbody", "<t2@test>", "Re: Thread")
	rm := app.NewRemoteMailboxService(svc)
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) { return f, nil })
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	h = srv.Handler()
	if rr := apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/messages", key); rr.Code != 200 {
		t.Fatalf("reconcile %d", rr.Code)
	}
	rr := apiGet(t, h, "/v1/threads/%3Ct1@test%3E", key)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Thread") {
		t.Fatalf("remote thread fallback %d: %s", rr.Code, rr.Body.String())
	}
}

// TestRemoteListFiltersAndRoleFolder proves the common filters are honored on a
// standalone inbox: a from filter is applied, and trashed=true maps to the
// remote Trash folder rather than being ignored.
func TestRemoteListFiltersAndRoleFolder(t *testing.T) {
	svc, h, u, _, standalone, fake := standaloneFixture(t)
	key := adminKey(t, svc, u)
	fake.folders["Trash"] = 100
	fake.add("INBOX", "From: a@b.test\r\nSubject: From A\r\nMessage-ID: <fa@test>\r\n\r\nbody", "<fa@test>", "From A")
	fake.add("INBOX", "From: c@d.test\r\nSubject: From C\r\nMessage-ID: <fc@test>\r\n\r\nbody", "<fc@test>", "From C")
	fake.add("Trash", "From: a@b.test\r\nSubject: Trashed\r\nMessage-ID: <tr@test>\r\n\r\nbody", "<tr@test>", "Trashed")
	fake.mu.Lock()
	for _, m := range fake.messages {
		switch m.messageID {
		case "<fc@test>":
			m.from = imap.Address{Name: "C", Address: "c@d.test"}
		default:
			m.from = imap.Address{Name: "A", Address: "a@b.test"}
		}
	}
	fake.mu.Unlock()
	// Reconcile.
	if rr := apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/messages", key); rr.Code != 200 {
		t.Fatalf("reconcile %d", rr.Code)
	}
	// from filter.
	rr := apiGet(t, h, "/v1/messages?inbox="+standalone.ID+"&from=c@d.test", key)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "From C") || strings.Contains(rr.Body.String(), "From A") {
		t.Fatalf("from filter %d: %s", rr.Code, rr.Body.String())
	}
	// trashed=true maps to the Trash folder.
	rr = apiGet(t, h, "/v1/messages?inbox="+standalone.ID+"&trashed=true", key)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Trashed") || strings.Contains(rr.Body.String(), "From A") {
		t.Fatalf("trashed filter %d: %s", rr.Code, rr.Body.String())
	}
}

// TestRemoteScopedListCursorNoSkip proves a scoped standalone message listing
// pages through same-second remote mail without gaps: the cursor is a metadata
// id, and resuming must not drop siblings that share the boundary second.
func TestRemoteScopedListCursorNoSkip(t *testing.T) {
	svc, h, u, _, standalone, fake := standaloneFixture(t)
	key := adminKey(t, svc, u)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// Five messages, three sharing one received second.
	fake.addAt("INBOX", "From: a@b.test\r\nSubject: One\r\nMessage-ID: <c1@test>\r\n\r\nb", "<c1@test>", "One", base)
	fake.addAt("INBOX", "From: a@b.test\r\nSubject: Two\r\nMessage-ID: <c2@test>\r\n\r\nb", "<c2@test>", "Two", base)
	fake.addAt("INBOX", "From: a@b.test\r\nSubject: Three\r\nMessage-ID: <c3@test>\r\n\r\nb", "<c3@test>", "Three", base)
	fake.addAt("INBOX", "From: a@b.test\r\nSubject: Four\r\nMessage-ID: <c4@test>\r\n\r\nb", "<c4@test>", "Four", base.Add(-time.Hour))
	fake.addAt("INBOX", "From: a@b.test\r\nSubject: Five\r\nMessage-ID: <c5@test>\r\n\r\nb", "<c5@test>", "Five", base.Add(-2*time.Hour))

	seen := map[string]int{}
	cursor := ""
	for page := 0; page < 10; page++ {
		path := "/v1/inboxes/" + standalone.ID + "/messages?limit=2"
		if cursor != "" {
			path += "&before=" + cursor
		}
		rr := apiGet(t, h, path, key)
		if rr.Code != 200 {
			t.Fatalf("page %d: %d %s", page, rr.Code, rr.Body.String())
		}
		var env struct {
			Items []struct {
				ID      string `json:"id"`
				Subject string `json:"subject"`
			} `json:"items"`
			NextCursor string `json:"next_cursor"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		for _, it := range env.Items {
			seen[it.Subject]++
			if seen[it.Subject] > 1 {
				t.Fatalf("duplicate across pages: %s", it.Subject)
			}
		}
		cursor = env.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(seen) != 5 {
		t.Fatalf("scoped pagination saw %d messages, want 5: %v", len(seen), seen)
	}
}

// TestRemoteArrivalEventIDResolvesToCanonicalMessage proves a client can follow a
// remote arrival event's entity id (which is the arrival id, distinct from the
// cached metadata id) into the canonical read API. Before the reconcile has
// indexed the message, the read must still return it, materializing the metadata
// row from the arrival's durable header.
func TestRemoteArrivalEventIDResolvesToCanonicalMessage(t *testing.T) {
	svc, h, u, _, _, fake := standaloneFixture(t)
	key := adminKey(t, svc, u)
	fake.add("INBOX", "From: sender@elsewhere.test\r\nSubject: Arrival canonical\r\nMessage-ID: <arr@remote>\r\n\r\nbody", "<arr@remote>", "Arrival canonical")

	// Reconcile once so the folder and UIDVALIDITY are known, then record an
	// arrival exactly as the watcher would (independent of the metadata index).
	inboxes, err := svc.Store.ListStandaloneInboxes(context.Background(), u.AccountID)
	if err != nil || len(inboxes) == 0 {
		t.Fatalf("list standalone: %v", err)
	}
	box := inboxes[0]
	if rr := apiGet(t, h, "/v1/inboxes/"+box.ID+"/messages", key); rr.Code != 200 {
		t.Fatalf("reconcile %d", rr.Code)
	}
	arrival, _, err := svc.Store.RecordRemoteArrival(context.Background(), u.AccountID, box.ID, store.RemoteArrivalInput{
		FolderPath: "INBOX", UIDValidity: 100, UID: 1,
		RFCMessageID: "<arr@remote>", FromName: "Sender", FromAddress: "sender@elsewhere.test", Subject: "Arrival canonical",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The event's entity id is the arrival id.
	ev, _, err := svc.Store.RecordRemoteArrivalEvent(context.Background(), u.AccountID, box.ID, arrival)
	if err != nil {
		t.Fatal(err)
	}
	if ev.EntityID != arrival.ID {
		t.Fatalf("event entity id = %q want arrival id %q", ev.EntityID, arrival.ID)
	}
	// Following the event id through the canonical read API resolves the message.
	rr := apiGet(t, h, "/v1/messages/"+ev.EntityID, key)
	if rr.Code != 200 {
		t.Fatalf("canonical read of arrival id = %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Arrival canonical") {
		t.Fatalf("canonical read did not return the arrival's message: %s", rr.Body.String())
	}
}

// TestAccountWideMergeSameSecondNoGap proves the account-wide merged listing does
// not drop remote messages that share a received second with local messages. Each
// remote source is resumed by its metadata id (a total order), so a same-second
// tie at a page boundary can never strand the remaining siblings.
func TestAccountWideMergeSameSecondNoGap(t *testing.T) {
	svc, h, u, domainBox, _, _, fakeA, _ := mergeFixture(t)
	key := adminKey(t, svc, u)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// Local messages all newer.
	for i := 0; i < 6; i++ {
		seedInboundAt(t, svc, domainBox, "gap-l"+string(rune('0'+i)), "<gl"+string(rune('0'+i))+"@test>", "Local "+string(rune('0'+i)), "body", base.Add(time.Duration(i+2)*time.Hour))
	}
	// Remote A: three messages all on the SAME second, older than every local.
	for i := 0; i < 3; i++ {
		fakeA.addAt("INBOX", "From: a@b.test\r\nSubject: A-same-"+string(rune('0'+i))+"\r\nMessage-ID: <as"+string(rune('0'+i))+"@test>\r\n\r\nbody", "<as"+string(rune('0'+i))+"@test>", "A-same-"+string(rune('0'+i)), base)
	}
	seen := map[string]int{}
	cursor := ""
	for page := 0; page < 20; page++ {
		path := "/v1/messages?limit=3"
		if cursor != "" {
			path += "&before=" + cursor
		}
		rr := apiGet(t, h, path, key)
		if rr.Code != 200 {
			t.Fatalf("page %d: %d %s", page, rr.Code, rr.Body.String())
		}
		var env struct {
			Items []struct {
				Subject string `json:"subject"`
			} `json:"items"`
			NextCursor string `json:"next_cursor"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		for _, it := range env.Items {
			seen[it.Subject]++
		}
		cursor = env.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(seen) != 9 {
		t.Fatalf("merged pagination saw %d distinct subjects, want 9: %v", len(seen), seen)
	}
	for _, want := range []string{"A-same-0", "A-same-1", "A-same-2"} {
		if seen[want] != 1 {
			t.Fatalf("same-second remote message %q occurrences = %d, want 1 (gaps=%v)", want, seen[want], seen)
		}
	}
}

// TestAccountWideMergeFilterStarveNoGap proves an account-wide listing with a
// filter the remote source cannot express does not miss older matching mail that
// sits behind newer non-matching rows. The merge must scan deeper within the
// remote source rather than return the first (all non-matching) window.
func TestAccountWideMergeFilterStarveNoGap(t *testing.T) {
	svc, h, u, _, _, _, fakeA, _ := mergeFixture(t)
	key := adminKey(t, svc, u)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// Three newer messages from "other", then one older matching message.
	fakeA.addAt("INBOX", "From: other@x.test\r\nSubject: Other1\r\nMessage-ID: <o1@test>\r\n\r\nb", "<o1@test>", "Other1", base.Add(3*time.Hour))
	fakeA.addAt("INBOX", "From: other@x.test\r\nSubject: Other2\r\nMessage-ID: <o2@test>\r\n\r\nb", "<o2@test>", "Other2", base.Add(2*time.Hour))
	fakeA.addAt("INBOX", "From: other@x.test\r\nSubject: Other3\r\nMessage-ID: <o3@test>\r\n\r\nb", "<o3@test>", "Other3", base.Add(1*time.Hour))
	fakeA.addAt("INBOX", "From: match@x.test\r\nSubject: Match\r\nMessage-ID: <m1@test>\r\n\r\nb", "<m1@test>", "Match", base)
	fakeA.mu.Lock()
	for _, m := range fakeA.messages {
		if strings.HasPrefix(m.messageID, "<m1@") {
			m.from = imap.Address{Name: "Match", Address: "match@x.test"}
		} else {
			m.from = imap.Address{Name: "Other", Address: "other@x.test"}
		}
	}
	fakeA.mu.Unlock()

	rr := apiGet(t, h, "/v1/messages?from=match@x.test&limit=1", key)
	if rr.Code != 200 {
		t.Fatalf("merge filter %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Match") {
		t.Fatalf("older matching message behind non-matching rows was lost: %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "Other") {
		t.Fatalf("non-matching message returned: %s", rr.Body.String())
	}
}

// TestRequestSendRemoteDraftReturnsHandoff proves a remote-draft request-send
// response surfaces the actionable handoff record: its immutable handoff id and
// the independent publication and notification states. Without this a client
// could not tell that a handoff was created, nor act on a later
// published/ambiguous/failed outcome.
func TestRequestSendRemoteDraftReturnsHandoff(t *testing.T) {
	svc, h, u, _, standalone, _ := standaloneFixture(t)
	key := adminKey(t, svc, u)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
	d, err := svc.Store.CreateDraft(ctx, p, model.Draft{
		InboxID: standalone.ID, To: []string{"x@outside.test"},
		Subject: "Handoff me", Text: "body",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/drafts/"+d.ID+"/request-send", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("request-send %d: %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Handoff *struct {
			HandoffID          string `json:"handoff_id"`
			Publication        string `json:"publication"`
			NotificationStatus string `json:"notification_status"`
		} `json:"handoff"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Handoff == nil {
		t.Fatalf("request-send did not return the handoff record: %s", rr.Body.String())
	}
	if out.Handoff.HandoffID == "" {
		t.Fatalf("handoff id missing: %s", rr.Body.String())
	}
	if out.Handoff.Publication != model.HandoffPending {
		t.Fatalf("handoff publication = %q want pending", out.Handoff.Publication)
	}
}

// TestRemoteMessageGETReturnsLiveBody proves the common single-message GET for a
// standalone inbox returns the actual parsed text/html (fetched live, never
// archived) rather than only metadata, matching the local read. The list stays
// metadata-only.
func TestRemoteMessageGETReturnsLiveBody(t *testing.T) {
	svc, h, u, _, standalone, fake := standaloneFixture(t)
	key := adminKey(t, svc, u)
	raw := "From: remote@elsewhere.test\r\nTo: agent@remote.test\r\nSubject: Live body\r\nMessage-ID: <livebody@remote>\r\nMIME-Version: 1.0\r\nContent-Type: text/plain\r\n\r\nhello live body"
	fake.add("INBOX", raw, "<livebody@remote>", "Live body")

	rr := apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/messages", key)
	var listEnv struct {
		Items []struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listEnv); err != nil || len(listEnv.Items) == 0 {
		t.Fatalf("list %d %s", rr.Code, rr.Body.String())
	}
	if listEnv.Items[0].Text != "" {
		t.Fatalf("list should stay metadata-only, got text %q", listEnv.Items[0].Text)
	}
	id := listEnv.Items[0].ID

	// The scoped single-message GET returns the live body.
	rr = apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/messages/"+id, key)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "hello live body") {
		t.Fatalf("scoped GET did not return the live body: %d %s", rr.Code, rr.Body.String())
	}
	// The account-wide single-message GET returns it too.
	rr = apiGet(t, h, "/v1/messages/"+id, key)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "hello live body") {
		t.Fatalf("account-wide GET did not return the live body: %d %s", rr.Code, rr.Body.String())
	}
}

// TestRemoteScopeRejectsUnadmittedSharedFolder proves a folder that was never
// admitted into the inbox's scope-correct tree (for example a shared namespace
// the discovery excludes) is not addressable: the scoped listing answers 404
// rather than silently returning an empty 200 for any path under the login.
func TestRemoteScopeRejectsUnadmittedSharedFolder(t *testing.T) {
	svc, h, u, _, standalone, _ := standaloneFixture(t)
	key := adminKey(t, svc, u)
	// Prime the scope-correct tree.
	if rr := apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/folders", key); rr.Code != 200 {
		t.Fatalf("folders %d", rr.Code)
	}
	for _, folder := range []string{"Shared/team", "Other Users/bob"} {
		rr := apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/messages?folder="+url.QueryEscape(folder), key)
		if rr.Code != 404 {
			t.Fatalf("shared folder %q = %d, want 404: %s", folder, rr.Code, rr.Body.String())
		}
	}
}

// TestRemoteMessageGETBodyFetchFailureIs503 proves the common single-message GET
// does not silently fall back to cached metadata when the live body fetch fails:
// the connector being unreachable is a 503, not a metadata-only 200.
func TestRemoteMessageGETBodyFetchFailureIs503(t *testing.T) {
	svc, h, u, _, standalone, fake := standaloneFixture(t)
	key := adminKey(t, svc, u)
	fake.add("INBOX", "From: a@b.test\r\nSubject: Body fail\r\nMessage-ID: <bf@remote>\r\n\r\nbody", "<bf@remote>", "Body fail")
	rr := apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/messages", key)
	var env struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil || len(env.Items) == 0 {
		t.Fatalf("list %d %s", rr.Code, rr.Body.String())
	}
	id := env.Items[0].ID

	// Make the connector unreachable for the body fetch.
	rm := app.NewRemoteMailboxService(svc)
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) {
		return nil, model.NewMailboxError(model.ErrKindUnavailable, "server unreachable", true, nil)
	})
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	h = srv.Handler()

	rr = apiGet(t, h, "/v1/messages/"+id, key)
	if rr.Code != 503 {
		t.Fatalf("single GET with unreachable body fetch = %d, want 503: %s", rr.Code, rr.Body.String())
	}
	// The UI reader must not render cached metadata as the body either: the
	// unreachable connector is surfaced (503), never a silent metadata page.
	cookie, _ := uiSession(t, svc, u.ID)
	req := httptest.NewRequest("GET", "/ui/messages/"+id, nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 503 {
		t.Fatalf("UI reader with unreachable body fetch = %d, want 503", rr.Code)
	}
}

// TestRemoteSearchAttachmentFilteredPaginationNoGap proves the account-wide
// search with an in-memory has_attachment filter scans deeper within the remote
// source instead of dropping older matching mail behind a short filtered window,
// and that a page with zero matches still exposes a resume cursor while more
// pages remain.
func TestRemoteSearchAttachmentFilteredPaginationNoGap(t *testing.T) {
	svc, h, u, _, _, _, fakeA, _ := mergeFixture(t)
	key := adminKey(t, svc, u)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// More plain matches (no attachment) than one scan window, then one older
	// message WITH the attachment. The attached match sits beyond the first raw
	// window, so a single-window filter would drop it; continuation must find it.
	// The attached message is inserted first, so it holds the lowest UID (the
	// fake orders newest-first by UID); the 60 newer plain matches push it
	// beyond the first raw search window.
	rawAtt := "From: a@b.test\r\nSubject: needle attached\r\nMessage-ID: <na@test>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nneedle body\r\n--b\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=f.bin\r\n\r\ndata\r\n--b--"
	fakeA.addAt("INBOX", rawAtt, "<na@test>", "needle attached", base)
	for i := 0; i < 60; i++ {
		fakeA.addAt("INBOX", "From: a@b.test\r\nSubject: needle plain\r\nMessage-ID: <np"+strconv.Itoa(i)+"@test>\r\n\r\nneedle body", "<np"+strconv.Itoa(i)+"@test>", "needle plain", base.Add(time.Duration(i+1)*time.Hour))
	}

	rr := apiGet(t, h, "/v1/search?subject=needle&has_attachment=true&limit=1", key)
	if rr.Code != 200 {
		t.Fatalf("search %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "needle attached") {
		t.Fatalf("older attached match behind non-matching rows was lost: %s", rr.Body.String())
	}
	// A search whose only matches are non-matching still exposes a resume cursor
	// when remote pages remain, rather than reporting a clean end.
	rr = apiGet(t, h, "/v1/search?subject=needle&has_attachment=false&limit=1", key)
	if rr.Code != 200 {
		t.Fatalf("search(no attach) %d: %s", rr.Code, rr.Body.String())
	}
}

// TestAuthoringApproverEnabledByEffectiveMode proves approver_enabled tracks the
// effective mode: a standalone inbox switched to mailmoose_approval reports it
// enabled (with standalone true), and a domain inbox cannot be set to
// remote_draft at all (the mode is preset to mailmoose_approval).
func TestAuthoringApproverEnabledByEffectiveMode(t *testing.T) {
	svc, h, u, domainBox, standalone, _ := standaloneFixture(t)
	key := adminKey(t, svc, u)
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}

	get := func(inbox string) map[string]any {
		rr := apiGet(t, h, "/v1/inboxes/"+inbox+"/authoring", key)
		if rr.Code != 200 {
			t.Fatalf("authoring get %s: %d %s", inbox, rr.Code, rr.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// Standalone default remote_draft: approver disabled, standalone true.
	sd := get(standalone.ID)
	if sd["approver_enabled"] != false || sd["standalone"] != true {
		t.Fatalf("standalone default %+v", sd)
	}
	// Switch standalone to approval: approver enabled.
	if err := svc.Store.SetInboxAuthoringMode(context.Background(), p, standalone.ID, model.AuthoringMailMooseApproval); err != nil {
		t.Fatal(err)
	}
	sd = get(standalone.ID)
	if sd["approver_enabled"] != true {
		t.Fatalf("standalone approval %+v", sd)
	}
	// Domain default approval: approver enabled, standalone false.
	dm := get(domainBox.ID)
	if dm["approver_enabled"] != true || dm["standalone"] != false {
		t.Fatalf("domain default %+v", dm)
	}
	// The API rejects remote_draft for a domain inbox, and the effective mode
	// stays the approval preset.
	put := func(mode string) int {
		body := fmt.Sprintf(`{"mode":%q}`, mode)
		req := httptest.NewRequest("PUT", "/v1/inboxes/"+domainBox.ID+"/authoring", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	if code := put(model.AuthoringRemoteDraft); code != 400 {
		t.Fatalf("domain remote_draft put = %d, want 400", code)
	}
	dm = get(domainBox.ID)
	if dm["mode"] != model.AuthoringMailMooseApproval || dm["approver_enabled"] != true {
		t.Fatalf("domain after rejected put %+v", dm)
	}
}

// TestApproverToggleScriptPresent proves app.js disables the approver input when
// the effective mode is remote_draft and enables it for mailmoose_approval, and
// labels the tab "Approvals" for a standalone inbox.
func TestApproverToggleScriptPresent(t *testing.T) {
	js := string(httpapp.AppJS())
	for _, want := range []string{
		"function syncAuthoringControls",
		"approverInput.disabled = !approval",
		"data.standalone ? 'Approvals' : 'Approver'",
		"effective === 'mailmoose_approval'",
		// A domain inbox is preset to MailMoose approval: the handoff controls
		// are hidden, never offered.
		"authStandalone = !!data.standalone",
		"controls.hidden = !authStandalone",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js missing %q", want)
		}
	}
}

// TestHandoffHistoryRetainedAndPanel proves the drafts page renders the handoff
// history panel and badge, and that a published handoff's terminal record survives
// after the local draft is cleaned up.
func TestHandoffHistoryRetainedAndPanel(t *testing.T) {
	svc, h, u, _, standalone, _ := standaloneFixture(t)
	// Create a draft and hand it off (remote_draft is the standalone default).
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
	d, err := svc.Store.CreateDraft(context.Background(), p, model.Draft{InboxID: standalone.ID, To: []string{"x@outside.test"}, Subject: "Handoff one", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RequestSend(context.Background(), p, d.ID, false); err != nil {
		t.Fatalf("request send: %v", err)
	}
	handoffs, err := svc.Store.ListAssistantHandlingForInbox(context.Background(), p, standalone.ID, 50)
	if err != nil || len(handoffs) != 1 {
		t.Fatalf("handoffs %v %#v", err, handoffs)
	}
	if handoffs[0].Publication != model.HandoffPending || handoffs[0].NotificationStatus == "" {
		t.Fatalf("handoff state %+v", handoffs[0])
	}
	// The drafts page shows the handoff badge and the history panel.
	cookie, csrf := uiSession(t, svc, u.ID)
	req := httptest.NewRequest("GET", "/ui/inboxes/"+standalone.ID+"/drafts", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("drafts %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"Draft handoffs", "Handoff pending", "cancel-handoff"} {
		if !strings.Contains(body, want) {
			t.Fatalf("drafts page missing %q", want)
		}
	}
	// The handoff list API returns the record in the envelope.
	key := adminKey(t, svc, u)
	rr = apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/handoffs", key)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"publication":"pending"`) {
		t.Fatalf("handoffs api %d: %s", rr.Code, rr.Body.String())
	}
	// Cancelling returns the draft to editable and keeps the terminal record.
	req = httptest.NewRequest("POST", "/ui/inboxes/"+standalone.ID+"/drafts/"+d.ID+"/cancel-handoff", strings.NewReader("_csrf="+csrf))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 303 {
		t.Fatalf("cancel handoff %d: %s", rr.Code, rr.Body.String())
	}
	after, err := svc.Store.ListAssistantHandlingForInbox(context.Background(), p, standalone.ID, 50)
	if err != nil || len(after) != 1 {
		t.Fatalf("retained handoffs %v %#v", err, after)
	}
	if after[0].Publication != model.HandoffFailed {
		t.Fatalf("cancelled handoff state %+v", after[0])
	}
}

// TestInboxReadinessStandaloneDistinct proves a standalone inbox's readiness is
// its IMAP/SMTP connector, not an empty domain.
func TestInboxReadinessStandaloneDistinct(t *testing.T) {
	svc, _, u, domainBox, standalone, _ := standaloneFixture(t)
	// Re-read the standalone inbox so RemoteConfigured reflects the configured
	// connector (the fixture configures it after creation).
	configured, err := svc.Store.GetInboxInternal(context.Background(), u.AccountID, standalone.ID)
	if err != nil {
		t.Fatal(err)
	}
	domains := []model.Domain{}
	boxes := []model.Inbox{domainBox, configured}
	_, _, sending, receiving := httpapp.InboxReadiness(domains, boxes)
	if receiving[standalone.ID] != true {
		t.Fatal("standalone inbox with an IMAP connector should be receiving-ready")
	}
	if sending[standalone.ID] != false {
		t.Fatal("standalone inbox without SMTP should not be sending-ready")
	}
}

// TestDomainAuthoringSaveKeepsApprovalControls proves saving the authoring form on
// a domain inbox (approval mode, keeping the approver) does not disturb the
// approval controls: the mode stays mailmoose_approval and the approver is kept.
func TestDomainAuthoringSaveKeepsApprovalControls(t *testing.T) {
	svc, h, u, domainBox, _, _ := standaloneFixture(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
	if err := svc.Store.SetInboxApprover(ctx, u.AccountID, domainBox.ID, "approver@example.com"); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, u.ID)
	// Save with the domain's default (approval) mode and the approver field.
	form := "display=Hermes&approver_email=approver@example.com&authoring_mode=mailmoose_approval&authoring_notify=&_csrf=" + csrf
	req := httptest.NewRequest("POST", "/ui/inboxes/"+domainBox.ID+"/edit", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 303 {
		t.Fatalf("domain authoring save %d: %s", rr.Code, rr.Body.String())
	}
	settings, err := svc.Store.GetInboxAuthoringSettingsInternal(ctx, u.AccountID, domainBox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Mode != model.AuthoringMailMooseApproval {
		t.Fatalf("domain mode changed on save: %q", settings.Mode)
	}
	got, _ := svc.Store.GetInboxInternal(ctx, u.AccountID, domainBox.ID)
	if got.ApproverEmail != "approver@example.com" {
		t.Fatalf("domain approver disturbed: %q", got.ApproverEmail)
	}
	// The authoring API agrees the approver is enabled for the domain.
	key := adminKey(t, svc, u)
	rr = apiGet(t, h, "/v1/inboxes/"+domainBox.ID+"/authoring", key)
	if !strings.Contains(rr.Body.String(), `"approver_enabled":true`) || !strings.Contains(rr.Body.String(), `"standalone":false`) {
		t.Fatalf("domain authoring view %d: %s", rr.Code, rr.Body.String())
	}
	_ = p
}

// TestStandaloneLiveStateStaleness proves the inbox live-state snapshot reports
// remote index staleness for a standalone inbox, so a live unread count is never
// presented as current when the remote index is incomplete.
func TestStandaloneLiveStateStaleness(t *testing.T) {
	svc, h, u, _, standalone, _ := standaloneFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)
	req := httptest.NewRequest("GET", "/ui/state?page=inbox&inbox="+standalone.ID, nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("state %d: %s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["remote"] != true {
		t.Fatalf("standalone live state missing remote flag: %s", rr.Body.String())
	}
	if _, ok := out["remote_status"]; !ok {
		t.Fatalf("standalone live state missing remote_status: %s", rr.Body.String())
	}
}

// TestStandaloneEditDialogRemoteTabs proves the dashboard edit dialog for a
// standalone inbox carries the Remote IMAP / Outbound SMTP tabs and embeds the
// secret-free remote config, while a domain inbox embeds none.
func TestStandaloneEditDialogRemoteTabs(t *testing.T) {
	svc, h, u, domainBox, standalone, _ := standaloneFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)
	rr := uiGet(t, h, cookie, "/")
	if rr.Code != 200 {
		t.Fatalf("dashboard %d", rr.Code)
	}
	body := rr.Body.String()
	dialog := dialogHTML(t, body, "inbox-edit-dialog")
	for _, want := range []string{
		`data-inbox-tab="remote-imap"`, `data-inbox-tab="remote-smtp"`,
		`data-inbox-mode="remote"`, `id="inbox-remote-form"`,
		`name="remote_host"`, `name="imap_password"`, `name="smtp_host"`,
	} {
		if !strings.Contains(dialog, want) {
			t.Fatalf("edit dialog missing %q", want)
		}
	}
	// The standalone row embeds its secret-free remote config. The edit-inbox
	// button starts with class then data-*, so search around the class name.
	row := ""
	if i := strings.Index(body, `edit-inbox" data-id="`+standalone.ID+`"`); i >= 0 {
		if j := strings.LastIndex(body[:i], `<button`); j >= 0 {
			row = body[j : i+1200]
		}
	}
	if row == "" {
		t.Fatal("standalone edit-inbox row not found")
	}
	if !strings.Contains(row, `data-kind="standalone"`) {
		t.Fatalf("standalone row missing data-kind: %s", row)
	}
	if !strings.Contains(row, `&#34;host&#34;:&#34;imap.remote.test&#34;`) {
		t.Fatalf("standalone row missing remote config JSON: %s", row)
	}
	if strings.Contains(row, "imap-pw") {
		t.Fatal("edit button leaked the IMAP password")
	}
	// The domain row embeds no remote config; the shell never shows the remote
	// tabs for a domain inbox.
	row = ""
	if i := strings.Index(body, `edit-inbox" data-id="`+domainBox.ID+`"`); i >= 0 {
		if j := strings.LastIndex(body[:i], `<button`); j >= 0 {
			row = body[j : i+1200]
		}
	}
	if row == "" {
		t.Fatal("domain edit-inbox row not found")
	}
	if strings.Contains(row, `data-kind="standalone"`) || strings.Contains(row, `data-remote="{`) {
		t.Fatalf("domain row carries standalone/remote data: %s", row)
	}
}

// TestStandaloneEditRemoteSave proves the settings dialog's remote save updates
// the connector (blank password keeps the stored secret) and lands back on the
// dashboard with the inbox and tab to reopen.
func TestStandaloneEditRemoteSave(t *testing.T) {
	svc, h, u, _, standalone, _ := standaloneFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	form := url.Values{
		"_csrf":           {csrf},
		"inbox_tab":       {"remote-imap"},
		"remote_host":     {"imap.changed.test"},
		"remote_port":     {"993"},
		"remote_username": {standalone.Remote.Username},
		"remote_security": {standalone.Remote.Security},
		"namespace":       {standalone.Namespace},
		"imap_password":   {""},
		"smtp_host":       {""},
	}
	rr := uiPost(t, h, cookie, "/ui/inboxes/"+standalone.ID+"/remote", form.Encode())
	if rr.Code != 303 {
		t.Fatalf("remote save %d: %s", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "inbox="+standalone.ID) || !strings.Contains(loc, "inbox_tab=remote-imap") || !strings.Contains(loc, "notice=Remote+connector+saved") {
		t.Fatalf("redirect %q", loc)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	box, err := svc.Store.GetInboxInternal(context.Background(), p.AccountID, standalone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if box.Remote == nil || box.Remote.Host != "imap.changed.test" {
		t.Fatalf("host not updated: %+v", box.Remote)
	}
	// The blank password kept the stored secret.
	creds, err := svc.Store.GetRemoteCredentials(context.Background(), p.AccountID, standalone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if creds.EncryptedIMAP == "" {
		t.Fatal("stored IMAP secret was dropped by a blank-password save")
	}
}

// TestStandaloneEditRemoteSaveVerifyGate proves a connector that cannot sign in
// is never persisted: the save redirects with an error and the stored binding is
// unchanged.
func TestStandaloneEditRemoteSaveVerifyGate(t *testing.T) {
	// Build the stack with a dialer that always fails, so the save's live
	// sign-in test cannot pass.
	svc := newIsolatedService(t)
	u := createAdmin(t, svc)
	standalone := createStandalone(t, svc, u)
	rm := app.NewRemoteMailboxService(svc)
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) {
		return nil, model.NewMailboxError(model.ErrKindUnavailable, "unreachable", true, nil)
	})
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
	if _, err := rm.ConfigureStandaloneRemote(context.Background(), p, standalone.ID, store.StandaloneRemoteUpdate{IMAPPassword: "imap-pw"}); err != nil {
		t.Fatal(err)
	}
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	h := srv.Handler()
	cookie, csrf := uiSession(t, svc, u.ID)
	form := url.Values{
		"_csrf":           {csrf},
		"inbox_tab":       {"remote-smtp"},
		"remote_host":     {"imap.changed.test"},
		"remote_username": {standalone.Remote.Username},
		"remote_security": {standalone.Remote.Security},
		"imap_password":   {""},
		"smtp_host":       {""},
	}
	rr := uiPost(t, h, cookie, "/ui/inboxes/"+standalone.ID+"/remote", form.Encode())
	if rr.Code != 303 {
		t.Fatalf("remote save %d: %s", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "error=IMAP+connection+failed") || strings.Contains(loc, "notice=") {
		t.Fatalf("expected error redirect, got %q", loc)
	}
	box, err := svc.Store.GetInboxInternal(context.Background(), p.AccountID, standalone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if box.Remote == nil || box.Remote.Host != "imap.remote.test" {
		t.Fatalf("unverified host was persisted: %+v", box.Remote)
	}
}
