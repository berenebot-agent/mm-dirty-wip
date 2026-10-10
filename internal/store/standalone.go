package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/dellarb/mailmoose/internal/idgen"
	"github.com/dellarb/mailmoose/internal/model"
)

var (
	// ErrStandaloneRequired is returned when an operation that only makes sense
	// for a standalone inbox is attempted on a domain inbox.
	ErrStandaloneRequired = errors.New("operation requires a standalone inbox")
	// ErrKindImmutable is returned when an attempt is made to change an inbox's
	// kind after creation.
	ErrKindImmutable = errors.New("inbox kind is immutable")
)

// StandaloneCreate describes a standalone inbox to create. Address is the
// operator-supplied, self-contained address; Namespace is the selected remote
// root to sync. Every other field is the optional remote binding description.
type StandaloneCreate struct {
	DisplayName string
	Address     string
	Namespace   string
	Remote      *model.RemoteConnection
}

// ValidateStandaloneAddress normalizes and validates a standalone inbox address.
// It must be a bare, fully-qualified email address with no display name.
func ValidateStandaloneAddress(raw string) (string, error) {
	addr := strings.ToLower(strings.TrimSpace(raw))
	if addr == "" || len(addr) > 254 || strings.ContainsAny(addr, " <>\t\r\n") {
		return "", fmt.Errorf("enter a full email address")
	}
	parsed, err := mail.ParseAddress(addr)
	if err != nil || !strings.EqualFold(parsed.Address, addr) {
		return "", fmt.Errorf("enter a full email address")
	}
	at := strings.LastIndexByte(addr, '@')
	if at <= 0 || at == len(addr)-1 || !strings.Contains(addr[at+1:], ".") {
		return "", fmt.Errorf("enter a full email address")
	}
	return addr, nil
}

// validateSecurityMode normalizes a remote transport security mode. TLS
// (implicit, the default), STARTTLS and plain are accepted; plaintext is an
// explicit operator choice, never an automatic downgrade. Whether a deployment
// actually permits plain is decided by the runtime policy layer, not here.
func validateSecurityMode(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return model.RemoteSecurityDefault, nil
	case model.RemoteSecurityTLS, model.RemoteSecurityStartTLS, model.RemoteSecurityPlain:
		return strings.ToLower(strings.TrimSpace(raw)), nil
	default:
		return "", fmt.Errorf("remote security must be %q, %q or %q", model.RemoteSecurityTLS, model.RemoteSecurityStartTLS, model.RemoteSecurityPlain)
	}
}

// defaultIMAPPort returns the conventional port for an IMAP security mode.
func defaultIMAPPort(security string) int {
	switch security {
	case model.RemoteSecurityStartTLS, model.RemoteSecurityPlain:
		return model.RemoteDefaultIMAPStartPort
	default:
		return model.RemoteDefaultIMAPPort
	}
}

// defaultSMTPPort returns the conventional port for an SMTP security mode.
func defaultSMTPPort(security string) int {
	switch security {
	case model.RemoteSecurityStartTLS:
		return model.RemoteDefaultSMTPStartPort
	case model.RemoteSecurityPlain:
		return model.RemoteDefaultSMTPPlainPort
	default:
		return model.RemoteDefaultSMTPPort
	}
}

// validateRemoteConnection validates the non-secret remote description. The
// transport security mode defaults to TLS; STARTTLS and plain are accepted as
// explicit operator choices, and plaintext is never selected automatically.
func validateRemoteConnection(rc *model.RemoteConnection) (*model.RemoteConnection, error) {
	if rc == nil {
		return nil, nil
	}
	out := *rc
	out.Host = strings.TrimSpace(strings.ToLower(out.Host))
	out.Username = strings.TrimSpace(out.Username)
	if out.Host == "" {
		return nil, fmt.Errorf("remote host is required")
	}
	if strings.ContainsAny(out.Host, " /\\\t\r\n") {
		return nil, fmt.Errorf("invalid remote host")
	}
	security, err := validateSecurityMode(out.Security)
	if err != nil {
		return nil, err
	}
	out.Security = security
	if out.Username == "" {
		return nil, fmt.Errorf("remote username is required")
	}
	if out.Port == 0 {
		out.Port = defaultIMAPPort(out.Security)
	}
	if out.Port < 1 || out.Port > 65535 {
		return nil, fmt.Errorf("invalid remote port")
	}
	if out.SMTP != nil {
		s := *out.SMTP
		s.Host = strings.TrimSpace(strings.ToLower(s.Host))
		s.Username = strings.TrimSpace(s.Username)
		if s.Host == "" {
			return nil, fmt.Errorf("SMTP host is required when SMTP is enabled")
		}
		if strings.ContainsAny(s.Host, " /\\\t\r\n") {
			return nil, fmt.Errorf("invalid SMTP host")
		}
		sec, err := validateSecurityMode(s.Security)
		if err != nil {
			return nil, err
		}
		s.Security = sec
		if s.Username == "" {
			s.Username = out.Username
		}
		if s.Port == 0 {
			s.Port = defaultSMTPPort(s.Security)
		}
		if s.Port < 1 || s.Port > 65535 {
			return nil, fmt.Errorf("invalid SMTP port")
		}
		out.SMTP = &s
	}
	return &out, nil
}

// CreateStandaloneInbox creates a standalone inbox: an independent mailbox with
// its own address and no managed domain. Domain_id is the empty string, so the
// account's domain joins drop it and the standalone address is the identity.
func (s *Store) CreateStandaloneInbox(ctx context.Context, accountID string, in StandaloneCreate) (model.Inbox, error) {
	address, err := ValidateStandaloneAddress(in.Address)
	if err != nil {
		return model.Inbox{}, err
	}
	namespace := strings.TrimSpace(in.Namespace)
	if namespace == "" {
		namespace = model.NamespaceDefault
	}
	remote, err := validateRemoteConnection(in.Remote)
	if err != nil {
		return model.Inbox{}, err
	}
	var exists int
	if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM inboxes WHERE account_id=? AND kind='standalone' AND address=? COLLATE NOCASE`, accountID, address).Scan(&exists); err != nil {
		return model.Inbox{}, err
	}
	if exists != 0 {
		return model.Inbox{}, fmt.Errorf("%w: address is already a standalone inbox in this account", ErrConflict)
	}
	// A standalone address must not collide with an address that already routes
	// inbound in this account's managed domains, or delivery would be ambiguous.
	var managedCollision int
	if err := s.read.QueryRowContext(ctx, `SELECT
			(SELECT count(*) FROM inboxes i JOIN domains d ON d.id=i.domain_id WHERE i.account_id=? AND (i.local_part||'@'||d.name)=?)
			+(SELECT count(*) FROM inbox_aliases a JOIN domains d ON d.id=a.domain_id WHERE a.account_id=? AND (a.local_part||'@'||d.name)=?)`,
		accountID, address, accountID, address).Scan(&managedCollision); err != nil {
		return model.Inbox{}, err
	}
	if managedCollision != 0 {
		return model.Inbox{}, fmt.Errorf("%w: address already belongs to a managed mailbox or alias", ErrConflict)
	}
	id := idgen.New("in")
	now := nowText()
	var rhost, ruser, rsec, shost, suser, ssec string
	var rport, sport int
	if remote != nil {
		rhost, rport, ruser, rsec = remote.Host, remote.Port, remote.Username, remote.Security
		if remote.SMTP != nil {
			shost, sport, suser, ssec = remote.SMTP.Host, remote.SMTP.Port, remote.SMTP.Username, remote.SMTP.Security
		}
	}
	if _, err = s.write.ExecContext(ctx, `INSERT INTO inboxes(id,account_id,kind,domain_id,local_part,address,display_name,namespace,remote_host,remote_port,remote_username,remote_security,smtp_host,smtp_port,smtp_username,smtp_security,delivery_trigger,storage_used_bytes,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, accountID, model.InboxKindStandalone, nil, "", address, strings.TrimSpace(in.DisplayName), namespace, rhost, rport, ruser, rsec, shost, sport, suser, ssec, DeliveryTriggerDefault, 0, now); err != nil {
		return model.Inbox{}, err
	}
	if _, err = s.write.ExecContext(ctx, `INSERT INTO inbox_remote_credentials(inbox_id,account_id,revision,created_at,updated_at) VALUES(?,?,0,?,?)`, id, accountID, now, now); err != nil {
		return model.Inbox{}, err
	}
	return s.GetInboxInternal(ctx, accountID, id)
}

// SetInboxKind rejects any attempt to change the immutable kind column.
func (s *Store) SetInboxKind(ctx context.Context, accountID, inboxID, kind string) error {
	var current string
	err := s.read.QueryRowContext(ctx, `SELECT kind FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).Scan(&current)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if current == kind {
		return nil
	}
	return ErrKindImmutable
}

// SaveRemoteCredentials stores the encrypted IMAP (and optional SMTP) secrets of
// a standalone inbox. Both blobs are ciphertext produced by the caller under the
// account/inbox AAD; the store never sees plaintext credentials.
func (s *Store) SaveRemoteCredentials(ctx context.Context, accountID, inboxID, encryptedIMAP, encryptedSMTP string, expected ConfigVersion) error {
	var kind string
	err := s.read.QueryRowContext(ctx, `SELECT kind FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).Scan(&kind)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if kind != model.InboxKindStandalone {
		return ErrStandaloneRequired
	}
	now := nowText()
	res, err := s.write.ExecContext(ctx, `UPDATE inbox_remote_credentials SET encrypted_imap=?,encrypted_smtp=?,revision=revision+1,updated_at=? WHERE inbox_id=? AND account_id=? AND revision=?`,
		encryptedIMAP, encryptedSMTP, now, inboxID, accountID, expected.Revision)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConflict
	}
	// Mark the inbox configured only when an IMAP secret is present.
	configured := 0
	if encryptedIMAP != "" {
		configured = 1
	}
	if _, err = s.write.ExecContext(ctx, `UPDATE inboxes SET remote_configured=? WHERE id=? AND account_id=?`, configured, inboxID, accountID); err != nil {
		return err
	}
	return nil
}

// RemoteCredentials is the encrypted credential material of a standalone inbox.
// It is only ever returned on the internal store surface, never serialized.
type RemoteCredentials struct {
	InboxID       string
	AccountID     string
	EncryptedIMAP string
	EncryptedSMTP string
	Revision      int64
	CreatedAt     string
	UpdatedAt     string
}

// GetRemoteCredentials returns the encrypted credentials of a standalone inbox.
func (s *Store) GetRemoteCredentials(ctx context.Context, accountID, inboxID string) (RemoteCredentials, error) {
	var c RemoteCredentials
	err := s.read.QueryRowContext(ctx, `SELECT inbox_id,account_id,encrypted_imap,encrypted_smtp,revision,created_at,updated_at FROM inbox_remote_credentials WHERE inbox_id=? AND account_id=?`, inboxID, accountID).
		Scan(&c.InboxID, &c.AccountID, &c.EncryptedIMAP, &c.EncryptedSMTP, &c.Revision, &c.CreatedAt, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return c, ErrNotFound
	}
	return c, err
}

// ListStandaloneInboxes returns only the account's standalone inboxes.
func (s *Store) ListStandaloneInboxes(ctx context.Context, accountID string) ([]model.Inbox, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+fullInboxSelectCols+` FROM inboxes i LEFT JOIN domains d ON d.id=i.domain_id WHERE i.account_id=? AND i.kind='standalone' ORDER BY i.address`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Inbox{}
	for rows.Next() {
		i, _, scanErr := scanFullInbox(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// folderColumns is the projection of every inbox folder read.
const folderColumns = `id,account_id,inbox_id,path,name,parent_path,role,selectable,message_count,unread_count,created_at,updated_at`

func scanFolder(row interface{ Scan(...any) error }) (model.Folder, error) {
	var f model.Folder
	var selectable int
	var created, updated string
	if err := row.Scan(&f.ID, &f.AccountID, &f.InboxID, &f.Path, &f.Name, &f.ParentPath, &f.Role, &selectable, &f.MessageCount, &f.UnreadCount, &created, &updated); err != nil {
		return f, err
	}
	f.Selectable = selectable != 0
	f.CreatedAt, f.UpdatedAt = parseTime(created), parseTime(updated)
	return f, nil
}

// FolderInput is one folder to upsert during a sync reconcile.
type FolderInput struct {
	Path       string
	Name       string
	ParentPath string
	Role       string
	Selectable bool
}

// FolderRoleForName infers a folder role from its well-known remote name. It is
// deliberately conservative: unknown names are plain folders.
func FolderRoleForName(name string) string {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "INBOX":
		return model.FolderRoleInbox
	case "SENT", "SENT ITEMS", "SENT MESSAGES":
		return model.FolderRoleSent
	case "DRAFTS":
		return model.FolderRoleDrafts
	case "TRASH", "DELETED", "DELETED ITEMS", "DELETED MESSAGES":
		return model.FolderRoleTrash
	case "JUNK", "SPAM", "JUNK E-MAIL":
		return model.FolderRoleSpam
	case "ARCHIVE", "ALL MAIL", "ALL":
		return model.FolderRoleArchive
	case "OUTBOX":
		return model.FolderRoleOutbox
	default:
		return model.FolderRoleFolder
	}
}

// UpsertFolders reconciles the folder set of a standalone inbox to exactly the
// supplied paths: new paths are inserted, changed paths are updated, and paths
// absent from the input are removed. It is the folder half of a sync reconcile
// and runs in one transaction.
func (s *Store) UpsertFolders(ctx context.Context, accountID, inboxID string, folders []FolderInput) ([]model.Folder, error) {
	var kind string
	if err := s.read.QueryRowContext(ctx, `SELECT kind FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).Scan(&kind); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if kind != model.InboxKindStandalone {
		return nil, ErrStandaloneRequired
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := nowText()
	keep := map[string]bool{}
	for _, f := range folders {
		path := strings.TrimSpace(f.Path)
		if path == "" {
			continue
		}
		keep[path] = true
		name := strings.TrimSpace(f.Name)
		if name == "" {
			name = path
			if idx := strings.LastIndexAny(path, "/."); idx >= 0 && idx+1 < len(path) {
				name = path[idx+1:]
			}
		}
		role := f.Role
		if role == "" {
			role = FolderRoleForName(name)
		}
		var existingID string
		err := tx.QueryRowContext(ctx, `SELECT id FROM inbox_folders WHERE inbox_id=? AND path=?`, inboxID, path).Scan(&existingID)
		switch {
		case err == sql.ErrNoRows:
			id := idgen.New("fld")
			if _, err = tx.ExecContext(ctx, `INSERT INTO inbox_folders(id,account_id,inbox_id,path,name,parent_path,role,selectable,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
				id, accountID, inboxID, path, name, strings.TrimSpace(f.ParentPath), role, boolInt(f.Selectable), now, now); err != nil {
				return nil, err
			}
		case err != nil:
			return nil, err
		default:
			if _, err = tx.ExecContext(ctx, `UPDATE inbox_folders SET name=?,parent_path=?,role=?,selectable=?,updated_at=? WHERE id=?`, name, strings.TrimSpace(f.ParentPath), role, boolInt(f.Selectable), now, existingID); err != nil {
				return nil, err
			}
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,path FROM inbox_folders WHERE inbox_id=?`, inboxID)
	if err != nil {
		return nil, err
	}
	var toDelete []string
	for rows.Next() {
		var id, path string
		if err = rows.Scan(&id, &path); err != nil {
			rows.Close()
			return nil, err
		}
		if !keep[path] {
			toDelete = append(toDelete, id)
		}
	}
	rows.Close()
	for _, id := range toDelete {
		if _, err = tx.ExecContext(ctx, `DELETE FROM inbox_folders WHERE id=?`, id); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return s.ListFolders(ctx, accountID, inboxID)
}

// ListFolders returns a standalone inbox's folders in hierarchical order.
func (s *Store) ListFolders(ctx context.Context, accountID, inboxID string) ([]model.Folder, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+folderColumns+` FROM inbox_folders WHERE account_id=? AND inbox_id=? ORDER BY path`, accountID, inboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Folder{}
	for rows.Next() {
		f, scanErr := scanFolder(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetFolder resolves one folder by its path.
func (s *Store) GetFolder(ctx context.Context, accountID, inboxID, path string) (model.Folder, error) {
	f, err := scanFolder(s.read.QueryRowContext(ctx, `SELECT `+folderColumns+` FROM inbox_folders WHERE account_id=? AND inbox_id=? AND path=?`, accountID, inboxID, path))
	if err == sql.ErrNoRows {
		return f, ErrNotFound
	}
	return f, err
}

// RemoteMessage is the header/thread metadata of a message that lives on a
// remote server. The body and attachments are never archived: they are fetched
// live from the remote by UID when needed.
type RemoteMessage struct {
	ID           string
	AccountID    string
	InboxID      string
	FolderPath   string
	UIDValidity  uint32
	UID          uint32
	RFCMessageID string
	InReplyTo    string
	References   []string
	ThreadKey    string
	FromName     string
	FromAddress  string
	To           []string
	CC           []string
	Subject      string
	Snippet      string
	SizeBytes    int64
	HasAttach    bool
	Read         bool
	Flagged      bool
	ReceivedAt   *string
	SentAt       *string
	CreatedAt    string
	UpdatedAt    string
}

// RemoteMessageInput is one remote message header to upsert during a sync.
type RemoteMessageInput struct {
	FolderPath   string
	UIDValidity  uint32
	UID          uint32
	RFCMessageID string
	InReplyTo    string
	References   []string
	ThreadKey    string
	FromName     string
	FromAddress  string
	To           []string
	CC           []string
	Subject      string
	Snippet      string
	SizeBytes    int64
	HasAttach    bool
	Read         bool
	Flagged      bool
	ReceivedAt   *string
	SentAt       *string
}

// UpsertRemoteMessage records or updates one remote message's header metadata.
// It does not touch any locally stored body: only the metadata row changes.
func (s *Store) UpsertRemoteMessage(ctx context.Context, accountID, inboxID string, in RemoteMessageInput) (RemoteMessage, error) {
	var kind string
	if err := s.read.QueryRowContext(ctx, `SELECT kind FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).Scan(&kind); err != nil {
		if err == sql.ErrNoRows {
			return RemoteMessage{}, ErrNotFound
		}
		return RemoteMessage{}, err
	}
	if kind != model.InboxKindStandalone {
		return RemoteMessage{}, ErrStandaloneRequired
	}
	if strings.TrimSpace(in.FolderPath) == "" || in.UID == 0 {
		return RemoteMessage{}, fmt.Errorf("remote message requires a folder path and uid")
	}
	now := nowText()
	threadKey := strings.TrimSpace(in.ThreadKey)
	if threadKey == "" {
		threadKey = in.RFCMessageID
	}
	id := idgen.New("rm")
	_, err := s.write.ExecContext(ctx, `INSERT INTO inbox_remote_messages(id,account_id,inbox_id,folder_path,remote_uid_validity,remote_uid,rfc_message_id,in_reply_to,references_json,thread_key,from_name,from_address,to_json,cc_json,subject,snippet,size_bytes,has_attachments,is_read,is_flagged,received_at,sent_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(inbox_id,folder_path,remote_uid_validity,remote_uid) DO UPDATE SET
			rfc_message_id=excluded.rfc_message_id,in_reply_to=excluded.in_reply_to,references_json=excluded.references_json,thread_key=excluded.thread_key,
			from_name=excluded.from_name,from_address=excluded.from_address,to_json=excluded.to_json,cc_json=excluded.cc_json,subject=excluded.subject,
			snippet=excluded.snippet,size_bytes=excluded.size_bytes,has_attachments=excluded.has_attachments,is_read=excluded.is_read,is_flagged=excluded.is_flagged,
			received_at=excluded.received_at,sent_at=excluded.sent_at,updated_at=excluded.updated_at`,
		id, accountID, inboxID, in.FolderPath, in.UIDValidity, in.UID, in.RFCMessageID, in.InReplyTo, jsonString(in.References), threadKey,
		in.FromName, in.FromAddress, jsonString(in.To), jsonString(in.CC), in.Subject, in.Snippet, in.SizeBytes, boolInt(in.HasAttach), boolInt(in.Read), boolInt(in.Flagged),
		nullStringPtr(in.ReceivedAt), nullStringPtr(in.SentAt), now, now)
	if err != nil {
		return RemoteMessage{}, err
	}
	return s.GetRemoteMessageByUID(ctx, accountID, inboxID, in.FolderPath, in.UIDValidity, in.UID)
}

func (s *Store) GetRemoteMessageByUID(ctx context.Context, accountID, inboxID, folderPath string, uidValidity, uid uint32) (RemoteMessage, error) {
	return s.scanRemoteMessage(s.read.QueryRowContext(ctx, `SELECT `+remoteMessageColumns+` FROM inbox_remote_messages WHERE account_id=? AND inbox_id=? AND folder_path=? AND remote_uid_validity=? AND remote_uid=?`, accountID, inboxID, folderPath, uidValidity, uid))
}

// GetRemoteMessage resolves a remote message by its opaque metadata id.
func (s *Store) GetRemoteMessage(ctx context.Context, accountID, inboxID, id string) (RemoteMessage, error) {
	return s.scanRemoteMessage(s.read.QueryRowContext(ctx, `SELECT `+remoteMessageColumns+` FROM inbox_remote_messages WHERE account_id=? AND inbox_id=? AND id=?`, accountID, inboxID, id))
}

const remoteMessageColumns = `id,account_id,inbox_id,folder_path,remote_uid_validity,remote_uid,rfc_message_id,in_reply_to,references_json,thread_key,from_name,from_address,to_json,cc_json,subject,snippet,size_bytes,has_attachments,is_read,is_flagged,received_at,sent_at,created_at,updated_at`

func (s *Store) scanRemoteMessage(row interface{ Scan(...any) error }) (RemoteMessage, error) {
	var m RemoteMessage
	var refs, to, cc string
	var hasAttach, read, flagged int
	var received, sent sql.NullString
	err := row.Scan(&m.ID, &m.AccountID, &m.InboxID, &m.FolderPath, &m.UIDValidity, &m.UID, &m.RFCMessageID, &m.InReplyTo, &refs, &m.ThreadKey, &m.FromName, &m.FromAddress, &to, &cc, &m.Subject, &m.Snippet, &m.SizeBytes, &hasAttach, &read, &flagged, &received, &sent, &m.CreatedAt, &m.UpdatedAt)
	if err == sql.ErrNoRows {
		return m, ErrNotFound
	}
	if err != nil {
		return m, err
	}
	m.References = decodeStrings(refs)
	m.To = decodeStrings(to)
	m.CC = decodeStrings(cc)
	m.HasAttach = hasAttach != 0
	m.Read = read != 0
	m.Flagged = flagged != 0
	if received.Valid {
		m.ReceivedAt = &received.String
	}
	if sent.Valid {
		m.SentAt = &sent.String
	}
	return m, nil
}

// ListRemoteMessages returns a folder's remote message metadata, newest first.
func (s *Store) ListRemoteMessages(ctx context.Context, accountID, inboxID, folderPath string) ([]RemoteMessage, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+remoteMessageColumns+` FROM inbox_remote_messages WHERE account_id=? AND inbox_id=? AND folder_path=? ORDER BY COALESCE(received_at,'') DESC, remote_uid DESC`, accountID, inboxID, folderPath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RemoteMessage{}
	for rows.Next() {
		m, scanErr := s.scanRemoteMessage(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteRemoteMessage removes a remote message's metadata row. The body lives on
// the remote server and is untouched.
func (s *Store) DeleteRemoteMessage(ctx context.Context, accountID, inboxID, id string) error {
	res, err := s.write.ExecContext(ctx, `DELETE FROM inbox_remote_messages WHERE account_id=? AND inbox_id=? AND id=?`, accountID, inboxID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func nullStringPtr(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}
