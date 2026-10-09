package app

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/netutil"
)

// MX receiver modes as exposed through the UI/API. An empty mode means no
// receiver is configured.
const (
	MXModeNone     = ""
	MXModeIncluded = "included"
	MXModeRemote   = "remote"
)

// ErrMXInvalidInput is returned when a save request cannot be satisfied as
// specified. It is distinct from a permission or CAS error so the UI can point
// at the offending field.
var ErrMXInvalidInput = errors.New("invalid MX receiver settings")

// mxSettingsAAD binds the installation MX receiver's stored secret and config
// blob to the installation row. There is exactly one row, so a fixed scope id
// is sufficient.
const mxSettingsAAD = "mx_settings"

// mxTLSMaxPEMBytes bounds the combined STARTTLS certificate+key PEM size. The
// pair is delivered to the included child inside one control-channel frame,
// whose payload is capped at 64 KiB (dialmx/control.MaxFrameBytes); this leaves
// headroom for the surrounding Settings JSON so a persisted pair can never be
// too large to activate. It is a local literal so the app package does not
// depend on the control transport.
const mxTLSMaxPEMBytes = 60 << 10

// MXReceiverSettings is the installation-wide MX receiver configuration as shown
// to a system administrator. The bearer credential is never returned: BearerKey
// is always empty and KeyConfigured reports whether one is stored. CA is a
// non-secret PEM bundle of private receiver roots. Revision is the
// optimistic-concurrency token a save must echo back.
type MXReceiverSettings struct {
	Mode          string `json:"mode"`
	URL           string `json:"url"`
	BearerKey     string `json:"bearer_key"`
	KeyConfigured bool   `json:"key_configured"`
	CA            string `json:"ca"`

	// Included-edge SMTP settings. They are omitted from JSON when nil/zero in
	// remote mode (the pointers and omitempty) so a GET→PUT round-trip never
	// re-sends a mode-inapplicable field.
	Hostname        string `json:"hostname,omitempty"`
	MaxMessageBytes int64  `json:"max_message_bytes,omitempty"`
	MaxStagingBytes int64  `json:"max_staging_bytes,omitempty"`
	MaxRecipients   int    `json:"max_recipients,omitempty"`
	MaxConnections  int    `json:"max_connections,omitempty"`
	// RequireTLS refuses plaintext SMTP sessions; it needs a certificate.
	RequireTLS *bool `json:"require_tls,omitempty"`
	// Verification toggles for the included edge's SPF/DKIM/DMARC evidence.
	// They are nil when not applicable (remote mode) and default true when
	// included with no explicit choice, so the edge always verifies unless an
	// operator explicitly disables a check.
	VerifySPF   *bool `json:"verify_spf,omitempty"`
	VerifyDKIM  *bool `json:"verify_dkim,omitempty"`
	VerifyDMARC *bool `json:"verify_dmarc,omitempty"`
	// DNSResolver is an optional resolver address (host:port); the timeouts are
	// in seconds. Zero means "use the child default".
	DNSResolver         string `json:"dns_resolver,omitempty"`
	DNSTimeoutSeconds   int    `json:"dns_timeout_seconds,omitempty"`
	ReadTimeoutSeconds  int    `json:"read_timeout_seconds,omitempty"`
	WriteTimeoutSeconds int    `json:"write_timeout_seconds,omitempty"`
	DataTimeoutSeconds  int    `json:"data_timeout_seconds,omitempty"`
	// SMTPTLSCert is the public STARTTLS certificate (PEM). It is safe to
	// return; the private key is never echoed and SMTPTLSKeyConfigured reports
	// whether one is stored.
	SMTPTLSCert          string    `json:"smtp_tls_cert,omitempty"`
	SMTPTLSKey           string    `json:"smtp_tls_key,omitempty"`
	SMTPTLSKeyConfigured bool      `json:"smtp_tls_key_configured"`
	Revision             int64     `json:"revision"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// MXReceiverInput is the save request. Revision must match the currently stored
// revision, or be zero to create the configuration when none exists. In
// included mode BearerKey is ignored (the core generates and retains one); in
// remote mode an empty BearerKey retains any existing credential and a non-empty
// one replaces it. Clearing a remote credential is done by switching mode or
// clearing the whole configuration.
type MXReceiverInput struct {
	Mode            string `json:"mode"`
	URL             string `json:"url"`
	BearerKey       string `json:"bearer_key"`
	CA              string `json:"ca"`
	Hostname        string `json:"hostname"`
	MaxMessageBytes int64  `json:"max_message_bytes"`
	MaxStagingBytes int64  `json:"max_staging_bytes"`
	MaxRecipients   int    `json:"max_recipients"`
	MaxConnections  int    `json:"max_connections"`
	// RequireTLS refuses plaintext SMTP sessions. nil means "leave at the
	// default" (off), so an older caller cannot accidentally require TLS.
	RequireTLS *bool `json:"require_tls"`
	// Verification toggles. A nil pointer means "leave at the default" (true),
	// so an older caller that does not send them cannot accidentally disable
	// verification.
	VerifySPF   *bool `json:"verify_spf"`
	VerifyDKIM  *bool `json:"verify_dkim"`
	VerifyDMARC *bool `json:"verify_dmarc"`
	// DNSResolver and the timeouts (seconds) override the child defaults.
	DNSResolver         string `json:"dns_resolver"`
	DNSTimeoutSeconds   int    `json:"dns_timeout_seconds"`
	ReadTimeoutSeconds  int    `json:"read_timeout_seconds"`
	WriteTimeoutSeconds int    `json:"write_timeout_seconds"`
	DataTimeoutSeconds  int    `json:"data_timeout_seconds"`
	// SMTPTLSCert/SMTPTLSKey are an optional STARTTLS certificate and key as PEM
	// text. A blank key retains the stored key (so re-saving without re-pasting
	// the secret does not clear it); a non-blank key replaces it; clearing both
	// removes the pair.
	SMTPTLSCert string `json:"smtp_tls_cert"`
	SMTPTLSKey  string `json:"smtp_tls_key"`
	Revision    int64  `json:"revision"`
}

// mxStoredConfig is the encrypted auxiliary configuration blob. It never holds
// the bearer credential, which has its own encrypted column so a status read
// can report key presence without touching the config.
type mxStoredConfig struct {
	CA              string `json:"ca,omitempty"`
	Hostname        string `json:"hostname,omitempty"`
	MaxMessageBytes int64  `json:"max_message_bytes,omitempty"`
	MaxStagingBytes int64  `json:"max_staging_bytes,omitempty"`
	MaxRecipients   int    `json:"max_recipients,omitempty"`
	MaxConnections  int    `json:"max_connections,omitempty"`
	// RequireTLS and the verification toggles default off/on respectively when
	// absent, so a config written before the fields existed keeps working.
	RequireTLS  *bool `json:"require_tls,omitempty"`
	VerifySPF   *bool `json:"verify_spf,omitempty"`
	VerifyDKIM  *bool `json:"verify_dkim,omitempty"`
	VerifyDMARC *bool `json:"verify_dmarc,omitempty"`
	// DNSResolver and timeouts (seconds).
	DNSResolver         string `json:"dns_resolver,omitempty"`
	DNSTimeoutSeconds   int    `json:"dns_timeout_seconds,omitempty"`
	ReadTimeoutSeconds  int    `json:"read_timeout_seconds,omitempty"`
	WriteTimeoutSeconds int    `json:"write_timeout_seconds,omitempty"`
	DataTimeoutSeconds  int    `json:"data_timeout_seconds,omitempty"`
	// SMTPTLSCert/SMTPTLSKey are the encrypted STARTTLS pair. The whole blob is
	// encrypted, so the private key is at rest protected by the application key.
	SMTPTLSCert string `json:"smtp_tls_cert,omitempty"`
	SMTPTLSKey  string `json:"smtp_tls_key,omitempty"`
}

// GetMXReceiverSettings returns the redacted configuration. An unconfigured
// installation returns a zero-value settings with Mode empty and revision zero.
func (s *Service) GetMXReceiverSettings(ctx context.Context) (MXReceiverSettings, error) {
	m, err := s.Store.GetMXSettings(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return MXReceiverSettings{}, nil
	}
	if err != nil {
		return MXReceiverSettings{}, err
	}
	return s.redactMXSettings(m)
}

// MXReceiverSettingsForRuntime returns the persisted configuration including the
// decrypted bearer credential and CA. It exists for the in-process runtime
// reconciliation, which owns the receiver connections and is the only component
// that may see the secret. UI/API callers must use GetMXReceiverSettings, which
// redacts it.
func (s *Service) MXReceiverSettingsForRuntime(ctx context.Context) (MXReceiverSettings, error) {
	m, err := s.Store.GetMXSettings(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return MXReceiverSettings{}, nil
	}
	if err != nil {
		return MXReceiverSettings{}, err
	}
	out, err := s.redactMXSettings(m)
	if err != nil {
		return MXReceiverSettings{}, err
	}
	if m.EncryptedSecret != "" {
		secret, derr := s.DecryptSecretAAD(mxSettingsAAD, m.EncryptedSecret)
		if derr != nil {
			return MXReceiverSettings{}, derr
		}
		out.BearerKey = string(secret)
		out.KeyConfigured = true
	}
	// The private STARTTLS key is decrypted only for the runtime; the public
	// read path leaves it blank.
	if out.SMTPTLSKeyConfigured {
		blob, berr := s.decryptMXStoredConfig(m)
		if berr != nil {
			return MXReceiverSettings{}, berr
		}
		out.SMTPTLSKey = blob.SMTPTLSKey
	}
	return out, nil
}

// SaveMXReceiverSettings validates the principal, validates the input, retains
// or generates the credential as the mode requires, and CAS-writes the singleton
// configuration. It wakes the runtime reconciliation after a successful commit.
func (s *Service) SaveMXReceiverSettings(ctx context.Context, p model.Principal, in MXReceiverInput) (MXReceiverSettings, error) {
	if !p.SystemAdmin {
		return MXReceiverSettings{}, store.ErrForbidden
	}
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode != MXModeIncluded && mode != MXModeRemote {
		return MXReceiverSettings{}, fmt.Errorf("%w: mode must be %q or %q", ErrMXInvalidInput, MXModeIncluded, MXModeRemote)
	}
	receiverURL := strings.TrimRight(strings.TrimSpace(in.URL), "/")

	// Resolve the credential and the retained STARTTLS key against the stored
	// state in one read.
	existing, gerr := s.Store.GetMXSettings(ctx)
	if gerr != nil && !errors.Is(gerr, store.ErrNotFound) {
		return MXReceiverSettings{}, gerr
	}
	hasExisting := gerr == nil
	includedToIncluded := hasExisting && existing.Mode == MXModeIncluded
	existingBlob, berr := s.decryptMXStoredConfig(existing)
	if berr != nil {
		return MXReceiverSettings{}, berr
	}

	// A blank input key retains the stored one (only when the stored config is
	// also included, since the key is only meaningful there); a non-blank key
	// replaces it; supplying a cert with no key at all is rejected below.
	tlsCert := strings.TrimSpace(in.SMTPTLSCert)
	keySupplied := strings.TrimSpace(in.SMTPTLSKey) != ""
	tlsKey := in.SMTPTLSKey
	if !keySupplied && includedToIncluded {
		tlsKey = existingBlob.SMTPTLSKey
	}

	blob := mxStoredConfig{
		CA:                  strings.TrimSpace(in.CA),
		Hostname:            strings.TrimSpace(in.Hostname),
		MaxMessageBytes:     in.MaxMessageBytes,
		MaxStagingBytes:     in.MaxStagingBytes,
		MaxRecipients:       in.MaxRecipients,
		MaxConnections:      in.MaxConnections,
		RequireTLS:          in.RequireTLS,
		VerifySPF:           in.VerifySPF,
		VerifyDKIM:          in.VerifyDKIM,
		VerifyDMARC:         in.VerifyDMARC,
		DNSResolver:         strings.TrimSpace(in.DNSResolver),
		DNSTimeoutSeconds:   in.DNSTimeoutSeconds,
		ReadTimeoutSeconds:  in.ReadTimeoutSeconds,
		WriteTimeoutSeconds: in.WriteTimeoutSeconds,
		DataTimeoutSeconds:  in.DataTimeoutSeconds,
		SMTPTLSCert:         tlsCert,
		SMTPTLSKey:          strings.TrimSpace(tlsKey),
	}
	// A cleared certificate must not leave a retained key behind. A caller-
	// supplied key with no certificate is kept so the pair validation rejects it
	// rather than silently discarding the key.
	if tlsCert == "" && !keySupplied {
		blob.SMTPTLSKey = ""
	}
	if mode == MXModeRemote {
		if receiverURL == "" {
			return MXReceiverSettings{}, fmt.Errorf("%w: a remote receiver requires a URL", ErrMXInvalidInput)
		}
		if err := validateMXReceiverURL(receiverURL, s.Config.RequirePublicOutbound()); err != nil {
			return MXReceiverSettings{}, err
		}
		// Reject included-only fields in remote mode rather than store values
		// that the runtime would ignore, so the operator is never misled.
		if err := rejectIncludedOnlyFields(blob); err != nil {
			return MXReceiverSettings{}, err
		}
	} else {
		// Included mode dials a fixed loopback endpoint; a supplied URL would be
		// silently ignored, so reject it rather than store misleading state.
		if receiverURL != "" {
			return MXReceiverSettings{}, fmt.Errorf("%w: included mode does not use a URL", ErrMXInvalidInput)
		}
		if err := validateMXIncludedSettings(blob, s.Config.MaxMessageBytes); err != nil {
			return MXReceiverSettings{}, err
		}
	}
	// The CA bundle is only used by remote mode, but validate it whenever it is
	// present so a malformed bundle is caught at save, not at runtime where it
	// would silently fail every connection.
	if blob.CA != "" {
		if err := validateCAPEM(blob.CA); err != nil {
			return MXReceiverSettings{}, err
		}
	}

	var encryptedSecret string
	if mode == MXModeIncluded {
		// The included child always authenticates with a core-generated key. A
		// caller cannot supply one. An existing key is retained only when the
		// stored configuration is also included, so a save that only changes
		// limits does not rotate the credential; switching in from remote never
		// reuses the operator's remote key.
		if includedToIncluded && existing.EncryptedSecret != "" {
			encryptedSecret = existing.EncryptedSecret
		} else {
			secret, serr := newMXBearerKey()
			if serr != nil {
				return MXReceiverSettings{}, serr
			}
			enc, eerr := s.EncryptSecretAAD(mxSettingsAAD, []byte(secret))
			if eerr != nil {
				return MXReceiverSettings{}, eerr
			}
			encryptedSecret = enc
		}
	} else {
		// Remote mode: a supplied key replaces the stored one. A blank key
		// retains the stored credential only when the stored configuration is
		// also remote; switching in from included requires the operator to
		// supply the remote key, because the generated included key is not a
		// valid remote credential.
		switch {
		case strings.TrimSpace(in.BearerKey) != "":
			enc, eerr := s.EncryptSecretAAD(mxSettingsAAD, []byte(strings.TrimSpace(in.BearerKey)))
			if eerr != nil {
				return MXReceiverSettings{}, eerr
			}
			encryptedSecret = enc
		case hasExisting && existing.Mode == MXModeRemote && existing.EncryptedSecret != "":
			encryptedSecret = existing.EncryptedSecret
		default:
			return MXReceiverSettings{}, fmt.Errorf("%w: supplying a remote receiver requires a bearer key", ErrMXInvalidInput)
		}
	}
	raw, merr := json.Marshal(blob)
	if merr != nil {
		return MXReceiverSettings{}, merr
	}
	encryptedConfig, eerr := s.EncryptSecretAAD(mxSettingsAAD, raw)
	if eerr != nil {
		return MXReceiverSettings{}, eerr
	}

	saved, serr := s.Store.SaveMXSettingsCAS(ctx, mode, receiverURL, encryptedSecret, encryptedConfig, store.ConfigVersion{ID: store.MXSettingsID, Revision: in.Revision})
	if serr != nil {
		return MXReceiverSettings{}, serr
	}
	s.wakeMXRuntime()
	return s.redactMXSettings(saved)
}

// ClearMXReceiverSettings removes the configured receiver while preserving the
// "initialized" marker so the one-time environment import never re-fires. It
// validates the principal and the CAS revision, and wakes the runtime.
func (s *Service) ClearMXReceiverSettings(ctx context.Context, p model.Principal, revision int64) error {
	if !p.SystemAdmin {
		return store.ErrForbidden
	}
	if err := s.Store.ClearMXSettingsCAS(ctx, store.ConfigVersion{ID: store.MXSettingsID, Revision: revision}); err != nil {
		return err
	}
	s.wakeMXRuntime()
	return nil
}

// MXReceiverStatus is the observable state of the configured receiver, combining
// the persisted configuration with the live runtime state when a runtime is
// attached. It is safe to call from the system-administrator UI/API.
type MXReceiverStatus struct {
	Mode              string `json:"mode"`
	Configured        bool   `json:"configured"`
	Revision          int64  `json:"revision"`
	State             string `json:"state"`
	Detail            string `json:"detail,omitempty"`
	IncludedSupported bool   `json:"included_supported"`
	SMTPAddr          string `json:"smtp_addr,omitempty"`
	SessionAddr       string `json:"session_addr,omitempty"`
	ActiveConnections int64  `json:"active_connections"`
}

// MXReceiverConfigured reports whether an installation receiver is configured.
// It exposes the ingest gate's check to the UI so a domain editor can tell the
// operator that no receiver exists yet. It never reveals anything secret.
func (s *Service) MXReceiverConfigured(ctx context.Context) (bool, error) {
	return s.mxReceiverConfigured(ctx)
}

// mxReceiverConfigured reports whether a receiver is configured for the
// mx-routed ingest path. It reads the persisted singleton and treats only a
// non-empty mode as configured, so both included and remote modes enable ingest
// and a cleared/absent configuration disables it.
func (s *Service) mxReceiverConfigured(ctx context.Context) (bool, error) {
	m, err := s.Store.GetMXSettings(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return m.Mode != MXModeNone, nil
}

// MXReceiverStatus reports the configured receiver and, when the process has
// wired a runtime, its live state. The runtime is consulted even when no
// configuration is persisted, because it is the only component that knows
// whether the included receiver shape is available in this process; reporting
// that as false on a fresh install would wrongly hide the included option in the
// UI. Without a runtime it reports the persisted configuration with an "unknown"
// runtime state so a read-only consumer never mistakes absence of a controller
// for a healthy receiver.
func (s *Service) MXReceiverStatus(ctx context.Context) (MXReceiverStatus, error) {
	m, err := s.Store.GetMXSettings(ctx)
	configured := false
	mode := ""
	var revision int64
	switch {
	case errors.Is(err, store.ErrNotFound):
		// No persisted configuration: fall through with the zero values and let
		// the runtime report its shape availability.
	case err != nil:
		return MXReceiverStatus{}, err
	default:
		mode = m.Mode
		configured = m.Mode != MXModeNone
		revision = m.Revision
	}
	st := MXReceiverStatus{Mode: mode, Configured: configured, Revision: revision, State: mxStateUnknown}
	if s.MXRuntime != nil {
		// The runtime owns the live shape: SMTP/session addresses, the included
		// shape's availability and any listener failure. The persisted fields
		// always win, because the runtime may not yet have applied the latest
		// revision.
		live := s.MXRuntime.Status(ctx)
		live.Mode = mode
		live.Configured = configured
		live.Revision = revision
		if live.State == "" {
			live.State = mxStateUnknown
		}
		if !configured {
			live.State = mxStateDisabled
		}
		st = live
	}
	if !configured {
		st.State = mxStateDisabled
	}
	return st, nil
}

// Runtime MX state vocabulary for MXReceiverStatus.State. It mirrors the
// included child's control states and adds the states only the core can observe
// ("connecting", "unavailable", "unknown"). It is exported so the UI can label
// states without duplicating literals.
const (
	MXStateDisabled    = "disabled"
	MXStateUnknown     = "unknown"
	MXStateStandby     = "standby"
	MXStateActive      = "active"
	MXStateConnecting  = "connecting"
	MXStateDraining    = "draining"
	MXStateFailed      = "failed"
	MXStateUnavailable = "unavailable"
)

// unexported aliases keep the package's own reads terse.
const (
	mxStateDisabled = MXStateDisabled
	mxStateUnknown  = MXStateUnknown
)

// MXReceiverRuntime is the network-hook surface the app calls after a settings
// change. It is implemented by cmd/server, which owns the embedded child process
// and the remote dialer. The app never dials or spawns anything itself.
type MXReceiverRuntime interface {
	// Wake requests an out-of-band reconciliation pass after a settings change.
	Wake()
	// Status reports the live receiver state.
	Status(ctx context.Context) MXReceiverStatus
}

// wakeMXRuntime nudges the attached runtime, if any, after a settings change.
func (s *Service) wakeMXRuntime() {
	if s.MXRuntime != nil {
		s.MXRuntime.Wake()
	}
}

func (s *Service) redactMXSettings(m store.MXSettings) (MXReceiverSettings, error) {
	out := MXReceiverSettings{
		Mode:          m.Mode,
		URL:           m.ReceiverURL,
		KeyConfigured: m.EncryptedSecret != "",
		Revision:      m.Revision,
		UpdatedAt:     parseStoreTime(m.UpdatedAt),
	}
	blob, err := s.decryptMXStoredConfig(m)
	if err != nil {
		return MXReceiverSettings{}, err
	}
	out.CA = blob.CA
	out.Hostname = blob.Hostname
	out.MaxMessageBytes = blob.MaxMessageBytes
	out.MaxStagingBytes = blob.MaxStagingBytes
	out.MaxRecipients = blob.MaxRecipients
	out.MaxConnections = blob.MaxConnections
	out.RequireTLS = copyBoolPtr(blob.RequireTLS)
	// Preserve the operator's explicit tri-state; nil means "default", which the
	// runtime resolves to on. The stored pointers are copied, not the resolved
	// booleans, so a GET followed by a PUT does not turn an unset toggle into an
	// explicit one.
	out.VerifySPF = copyBoolPtr(blob.VerifySPF)
	out.VerifyDKIM = copyBoolPtr(blob.VerifyDKIM)
	out.VerifyDMARC = copyBoolPtr(blob.VerifyDMARC)
	out.DNSResolver = blob.DNSResolver
	out.DNSTimeoutSeconds = blob.DNSTimeoutSeconds
	out.ReadTimeoutSeconds = blob.ReadTimeoutSeconds
	out.WriteTimeoutSeconds = blob.WriteTimeoutSeconds
	out.DataTimeoutSeconds = blob.DataTimeoutSeconds
	out.SMTPTLSCert = blob.SMTPTLSCert
	// The private key is never echoed on the public read. The runtime accessor
	// fills it after this call.
	out.SMTPTLSKeyConfigured = blob.SMTPTLSKey != ""
	return out, nil
}

// decryptMXStoredConfig returns the decrypted auxiliary configuration, or a zero
// value when none has been stored.
func (s *Service) decryptMXStoredConfig(m store.MXSettings) (mxStoredConfig, error) {
	if m.EncryptedConfig == "" {
		return mxStoredConfig{}, nil
	}
	raw, err := s.DecryptSecretAAD(mxSettingsAAD, m.EncryptedConfig)
	if err != nil {
		return mxStoredConfig{}, err
	}
	var blob mxStoredConfig
	if err := json.Unmarshal(raw, &blob); err != nil {
		return mxStoredConfig{}, err
	}
	return blob, nil
}

// VerifySPFEnabled reports the effective SPF verification setting: an explicit
// value wins, and an unset value (nil) defaults to on.
func (s MXReceiverSettings) VerifySPFEnabled() bool { return boolDefault(s.VerifySPF, true) }

// VerifyDKIMEnabled reports the effective DKIM verification setting.
func (s MXReceiverSettings) VerifyDKIMEnabled() bool { return boolDefault(s.VerifyDKIM, true) }

// VerifyDMARCEnabled reports the effective DMARC verification setting.
func (s MXReceiverSettings) VerifyDMARCEnabled() bool { return boolDefault(s.VerifyDMARC, true) }

func copyBoolPtr(v *bool) *bool {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

// boolDefault resolves a tri-state toggle: nil means "the default", which for
// verification is on.
func boolDefault(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

// rejectIncludedOnlyFields fails a remote save that carries fields only the
// included edge uses, so the operator is not misled by values the runtime
// ignores.
func rejectIncludedOnlyFields(blob mxStoredConfig) error {
	if blob.Hostname != "" {
		return fmt.Errorf("%w: hostname applies only to the included receiver", ErrMXInvalidInput)
	}
	if blob.MaxMessageBytes != 0 || blob.MaxStagingBytes != 0 || blob.MaxRecipients != 0 || blob.MaxConnections != 0 {
		return fmt.Errorf("%w: SMTP limits apply only to the included receiver", ErrMXInvalidInput)
	}
	if blob.RequireTLS != nil || blob.VerifySPF != nil || blob.VerifyDKIM != nil || blob.VerifyDMARC != nil {
		return fmt.Errorf("%w: SMTP TLS and verification toggles apply only to the included receiver", ErrMXInvalidInput)
	}
	if blob.DNSResolver != "" || blob.DNSTimeoutSeconds != 0 || blob.ReadTimeoutSeconds != 0 || blob.WriteTimeoutSeconds != 0 || blob.DataTimeoutSeconds != 0 {
		return fmt.Errorf("%w: SMTP DNS and timeout settings apply only to the included receiver", ErrMXInvalidInput)
	}
	if blob.SMTPTLSCert != "" || blob.SMTPTLSKey != "" {
		return fmt.Errorf("%w: the SMTP TLS certificate applies only to the included receiver", ErrMXInvalidInput)
	}
	return nil
}

// validateCAPEM checks that a CA bundle contains at least one certificate, so a
// malformed bundle is rejected at save rather than failing every connection.
func validateCAPEM(pemData string) error {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(pemData)) {
		return fmt.Errorf("%w: CA bundle contains no certificates", ErrMXInvalidInput)
	}
	return nil
}

// validateMXReceiverURL accepts an HTTP or HTTPS origin for the installation
// Remote MX receiver. Cleartext and private/LAN hosts are allowed by default
// (self-hosting: the receiver is commonly another container on the same host or
// LAN). When the operator confines outbound to the public internet
// (requirePublic), the URL must be https and public-routable, matching the
// per-account Remote MX receiver policy.
func validateMXReceiverURL(raw string, requirePublic bool) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("%w: receiver URL must be an HTTP or HTTPS origin", ErrMXInvalidInput)
	}
	if !requirePublic {
		return nil
	}
	if u.Scheme != "https" {
		return fmt.Errorf("%w: a public receiver must use https", ErrMXInvalidInput)
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !netutil.PublicIP(ip) {
		return fmt.Errorf("%w: receiver host is not public-routable (set ALLOW_PRIVATE_OUTBOUND=true to allow this)", ErrMXInvalidInput)
	}
	return nil
}

// validateMXIncludedSettings validates the included edge's optional overrides.
// Zero means "use the child default"; a provided value must be meaningful so the
// operator never sets a limit that is silently clamped or ignored.
func validateMXIncludedSettings(blob mxStoredConfig, coreMaxMessageBytes int64) error {
	if blob.MaxMessageBytes != 0 && blob.MaxMessageBytes < 1<<20 {
		return fmt.Errorf("%w: max message size must be at least 1 MiB", ErrMXInvalidInput)
	}
	// The included edge must never accept a message the core ingest path would
	// reject. When the core cap is known, refuse a larger receiver limit rather
	// than advertise a size the core then refuses. A zero core cap (tests, or an
	// unset config) means no bound here.
	if coreMaxMessageBytes > 0 && blob.MaxMessageBytes > coreMaxMessageBytes {
		return fmt.Errorf("%w: max message size must not exceed the core limit of %d bytes", ErrMXInvalidInput, coreMaxMessageBytes)
	}
	if blob.MaxStagingBytes != 0 && blob.MaxStagingBytes < blob.MaxMessageBytes {
		return fmt.Errorf("%w: max staging size must be at least the max message size", ErrMXInvalidInput)
	}
	if blob.MaxRecipients < 0 || blob.MaxConnections < 0 {
		return fmt.Errorf("%w: SMTP limits must not be negative", ErrMXInvalidInput)
	}
	if blob.DNSTimeoutSeconds < 0 || blob.ReadTimeoutSeconds < 0 || blob.WriteTimeoutSeconds < 0 || blob.DataTimeoutSeconds < 0 {
		return fmt.Errorf("%w: SMTP timeouts must not be negative", ErrMXInvalidInput)
	}
	if blob.Hostname != "" {
		if err := validateMXHostname(blob.Hostname); err != nil {
			return err
		}
	}
	if err := validateMXTLSPair(blob); err != nil {
		return err
	}
	return nil
}

// validateMXTLSPair enforces the STARTTLS pair rules: cert and key must be
// supplied together, the pair must parse as a certificate, and RequireTLS needs
// a certificate to offer. A configured pair is validated here so a malformed
// certificate is rejected at save rather than at activation.
func validateMXTLSPair(blob mxStoredConfig) error {
	certSet := strings.TrimSpace(blob.SMTPTLSCert) != ""
	keySet := strings.TrimSpace(blob.SMTPTLSKey) != ""
	if certSet != keySet {
		return fmt.Errorf("%w: SMTP TLS certificate and key must be supplied together", ErrMXInvalidInput)
	}
	if requireTLS(blob.RequireTLS) && !certSet {
		return fmt.Errorf("%w: requiring TLS needs an SMTP TLS certificate and key", ErrMXInvalidInput)
	}
	if certSet {
		if len(blob.SMTPTLSCert)+len(blob.SMTPTLSKey) > mxTLSMaxPEMBytes {
			return fmt.Errorf("%w: SMTP TLS certificate and key are too large to deliver to the receiver", ErrMXInvalidInput)
		}
		if _, err := tls.X509KeyPair([]byte(blob.SMTPTLSCert), []byte(blob.SMTPTLSKey)); err != nil {
			return fmt.Errorf("%w: SMTP TLS certificate/key pair is invalid", ErrMXInvalidInput)
		}
	}
	return nil
}

// requireTLSOrFalse resolves the tri-state RequireTLS toggle: nil means off.
func requireTLS(v *bool) bool { return boolDefault(v, false) }

// RequireTLSEnabled reports the effective RequireTLS setting (nil = off).
func (s MXReceiverSettings) RequireTLSEnabled() bool { return requireTLS(s.RequireTLS) }

// validateMXHostname accepts a DNS hostname (optionally one trailing dot) of at
// most 253 characters. It rejects schemes, ports, spaces and other values that
// would produce an invalid SMTP greeting rather than being silently corrected.
func validateMXHostname(host string) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil
	}
	if len(host) > 253 {
		return fmt.Errorf("%w: hostname is too long", ErrMXInvalidInput)
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" || strings.ContainsAny(host, " \t/:@") {
		return fmt.Errorf("%w: hostname must be a bare DNS name", ErrMXInvalidInput)
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("%w: hostname has an empty or over-long label", ErrMXInvalidInput)
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return fmt.Errorf("%w: hostname contains an invalid character", ErrMXInvalidInput)
			}
		}
	}
	return nil
}

func newMXBearerKey() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// parseStoreTime parses the RFC3339Nano text the store persists, returning the
// zero time for an empty or malformed value.
func parseStoreTime(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, v)
	return t
}
