package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
	"github.com/dellarb/mailmoose/internal/transport/netutil"
)

// ErrAccountMXInvalidInput is returned when an account Remote MX save cannot be
// satisfied as specified. It mirrors ErrMXInvalidInput so the UI can point at
// the offending field.
var ErrAccountMXInvalidInput = errors.New("invalid Remote MX receiver settings")

// AccountMXReceiver is the redacted per-account Remote MX receiver configuration
// as shown to an account admin. The bearer credential is never returned:
// BearerKey is always empty and KeyConfigured reports whether one is stored. CA
// is a non-secret PEM bundle of private receiver roots. AllowPrivate is the
// per-receiver opt-in that lets the dialer reach a loopback/LAN receiver.
// Revision is the optimistic-concurrency token a save must echo back.
type AccountMXReceiver struct {
	URL           string `json:"url"`
	BearerKey     string `json:"bearer_key"`
	KeyConfigured bool   `json:"key_configured"`
	CA            string `json:"ca"`
	AllowPrivate  bool   `json:"allow_private"`
	Revision      int64  `json:"revision"`
}

// AccountMXReceiverInput is the save request. Revision must match the currently
// stored revision, or be zero to create the configuration when none exists. An
// empty BearerKey retains any existing credential and a non-empty one replaces
// it.
type AccountMXReceiverInput struct {
	URL          string `json:"url"`
	BearerKey    string `json:"bearer_key"`
	CA           string `json:"ca"`
	AllowPrivate bool   `json:"allow_private"`
	Revision     int64  `json:"revision"`
}

// accountMXStoredConfig is the encrypted auxiliary configuration blob. It never
// holds the bearer credential, which has its own encrypted column so a status
// read can report key presence without touching the config.
type accountMXStoredConfig struct {
	CA           string `json:"ca,omitempty"`
	AllowPrivate bool   `json:"allow_private,omitempty"`
}

// GetAccountMXReceiver returns the redacted configuration. An unconfigured
// account returns a zero-value settings with revision zero.
func (s *Service) GetAccountMXReceiver(ctx context.Context, accountID string) (AccountMXReceiver, error) {
	m, err := s.Store.GetAccountMXReceiver(ctx, accountID)
	if errors.Is(err, store.ErrNotFound) {
		return AccountMXReceiver{}, nil
	}
	if err != nil {
		return AccountMXReceiver{}, err
	}
	return s.redactAccountMXReceiver(m)
}

// AccountMXReceiverSettingsForRuntime returns the persisted configuration
// including the decrypted bearer credential. It exists for the in-process
// runtime reconciliation, which owns the receiver connections and is the only
// component that may see the secret. UI/API callers must use
// GetAccountMXReceiver, which redacts it.
func (s *Service) AccountMXReceiverSettingsForRuntime(ctx context.Context, accountID string) (AccountMXReceiver, error) {
	m, err := s.Store.GetAccountMXReceiver(ctx, accountID)
	if errors.Is(err, store.ErrNotFound) {
		return AccountMXReceiver{}, nil
	}
	if err != nil {
		return AccountMXReceiver{}, err
	}
	out, err := s.redactAccountMXReceiver(m)
	if err != nil {
		return AccountMXReceiver{}, err
	}
	if m.EncryptedSecret != "" {
		secret, derr := s.DecryptSecret(m.EncryptedSecret)
		if derr != nil {
			return AccountMXReceiver{}, derr
		}
		out.BearerKey = string(secret)
		out.KeyConfigured = true
	}
	return out, nil
}

// SaveAccountMXReceiver validates the principal, validates the input, retains or
// replaces the credential, enforces the global one-account-per-receiver rule,
// and CAS-writes the account's configuration. It wakes the runtime
// reconciliation after a successful commit.
func (s *Service) SaveAccountMXReceiver(ctx context.Context, p model.Principal, in AccountMXReceiverInput) (AccountMXReceiver, error) {
	if !p.Admin {
		return AccountMXReceiver{}, store.ErrForbidden
	}
	receiverURL := strings.TrimRight(strings.TrimSpace(in.URL), "/")
	if receiverURL == "" {
		return AccountMXReceiver{}, fmt.Errorf("%w: a Remote MX receiver requires a URL", ErrAccountMXInvalidInput)
	}
	allowPrivate := in.AllowPrivate
	if err := validateAccountMXReceiverURL(receiverURL, allowPrivate, s.Config.RequirePublicOutbound()); err != nil {
		return AccountMXReceiver{}, err
	}
	blob := accountMXStoredConfig{CA: strings.TrimSpace(in.CA), AllowPrivate: allowPrivate}
	if blob.CA != "" {
		if err := validateCAPEM(blob.CA); err != nil {
			return AccountMXReceiver{}, err
		}
	}

	existing, gerr := s.Store.GetAccountMXReceiver(ctx, p.AccountID)
	if gerr != nil && !errors.Is(gerr, store.ErrNotFound) {
		return AccountMXReceiver{}, gerr
	}
	hasExisting := gerr == nil

	// One physical single-mode receiver belongs to exactly one account: a second
	// account may not register the same receiver URL. (The bearer key cannot be
	// compared without decrypting every other account's secret, so URL identity is
	// the enforced invariant; a wrong key simply never authenticates.)
	others, uerr := s.Store.ListAccountMXReceiverURLs(ctx, p.AccountID)
	if uerr != nil {
		return AccountMXReceiver{}, uerr
	}
	for _, u := range others {
		if strings.EqualFold(strings.TrimRight(u, "/"), receiverURL) {
			return AccountMXReceiver{}, fmt.Errorf("%w: this receiver is already registered to another account", ErrAccountMXInvalidInput)
		}
	}

	var encryptedSecret string
	switch {
	case strings.TrimSpace(in.BearerKey) != "":
		enc, eerr := s.EncryptSecret([]byte(strings.TrimSpace(in.BearerKey)))
		if eerr != nil {
			return AccountMXReceiver{}, eerr
		}
		encryptedSecret = enc
	case hasExisting && existing.EncryptedSecret != "":
		encryptedSecret = existing.EncryptedSecret
	default:
		return AccountMXReceiver{}, fmt.Errorf("%w: a Remote MX receiver requires a bearer key", ErrAccountMXInvalidInput)
	}
	raw, merr := json.Marshal(blob)
	if merr != nil {
		return AccountMXReceiver{}, merr
	}
	encryptedConfig, eerr := s.EncryptSecret(raw)
	if eerr != nil {
		return AccountMXReceiver{}, eerr
	}
	// The caller's revision is the optimistic-concurrency token: zero creates
	// (and fails if a row exists), a non-zero value must match the stored row.
	saved, serr := s.Store.SaveAccountMXReceiverCAS(ctx, p.AccountID, receiverURL, encryptedSecret, encryptedConfig, store.ConfigVersion{Revision: in.Revision})
	if serr != nil {
		return AccountMXReceiver{}, serr
	}
	s.wakeRemoteMXRuntime()
	return s.redactAccountMXReceiver(saved)
}

// ClearAccountMXReceiver removes the account's configured receiver while keeping
// the row so the runtime observes the change. It validates the principal and the
// CAS revision, refuses when a domain still routes to the receiver (fail closed:
// the caller must re-point those domains first), and wakes the runtime.
func (s *Service) ClearAccountMXReceiver(ctx context.Context, p model.Principal, revision int64) error {
	if !p.Admin {
		return store.ErrForbidden
	}
	inUse, err := s.Store.RemoteMXReceiverInUse(ctx, p.AccountID)
	if err != nil {
		return err
	}
	if inUse {
		return fmt.Errorf("%w: one or more domains still receive through this receiver; change them first", ErrAccountMXInvalidInput)
	}
	if err := s.Store.ClearAccountMXReceiverCAS(ctx, p.AccountID, store.ConfigVersion{Revision: revision}); err != nil {
		return err
	}
	s.wakeRemoteMXRuntime()
	return nil
}

func (s *Service) redactAccountMXReceiver(m store.AccountMXReceiver) (AccountMXReceiver, error) {
	out := AccountMXReceiver{
		URL:           m.ReceiverURL,
		KeyConfigured: m.EncryptedSecret != "",
		Revision:      m.Revision,
	}
	blob, err := s.decryptAccountMXStoredConfig(m)
	if err != nil {
		return AccountMXReceiver{}, err
	}
	out.CA = blob.CA
	out.AllowPrivate = blob.AllowPrivate
	return out, nil
}

func (s *Service) decryptAccountMXStoredConfig(m store.AccountMXReceiver) (accountMXStoredConfig, error) {
	if m.EncryptedConfig == "" {
		return accountMXStoredConfig{}, nil
	}
	raw, err := s.DecryptSecret(m.EncryptedConfig)
	if err != nil {
		return accountMXStoredConfig{}, err
	}
	var blob accountMXStoredConfig
	if err := json.Unmarshal(raw, &blob); err != nil {
		return accountMXStoredConfig{}, err
	}
	return blob, nil
}

// validateAccountMXReceiverURL accepts an HTTP or HTTPS origin. When the
// receiver is not marked private, the destination is held to the public-only
// policy, matching the installation Remote receiver. A private opt-in permits a
// loopback/LAN destination, but only when the operator's global policy allows
// private outbound (requirePublic=false); when the operator confines outbound to
// the public internet, an account cannot re-enable private destinations.
func validateAccountMXReceiverURL(raw string, allowPrivate, requirePublic bool) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("%w: receiver URL must be an HTTP or HTTPS origin", ErrAccountMXInvalidInput)
	}
	if requirePublic {
		allowPrivate = false
	}
	if allowPrivate {
		return nil
	}
	if u.Scheme != "https" {
		return fmt.Errorf("%w: a public receiver must use https", ErrAccountMXInvalidInput)
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !netutil.PublicIP(ip) {
		return fmt.Errorf("%w: receiver host is not public-routable (mark it private to allow this)", ErrAccountMXInvalidInput)
	}
	return nil
}

// AccountMXReceiverRuntime is the network-hook surface the app calls after an
// account Remote MX change. It is implemented by cmd/server.
type AccountMXReceiverRuntime interface {
	WakeRemoteMX()
	RemoteMXStatus(ctx context.Context, accountID string) AccountMXReceiverStatus
}

// AccountMXReceiverStatus is the observable live state of one account receiver.
type AccountMXReceiverStatus struct {
	Configured bool   `json:"configured"`
	Revision   int64  `json:"revision"`
	State      string `json:"state"`
	Detail     string `json:"detail,omitempty"`
}

func (s *Service) wakeRemoteMXRuntime() {
	if s.RemoteMXRuntime != nil {
		s.RemoteMXRuntime.WakeRemoteMX()
	}
}

// RemoteMXProvider is the registered receiving provider name for an
// account-owned Remote MX receiver.
const RemoteMXProvider = "remotemx"

// remoteMXBackend adapts one account's Remote MX receiver to the mxdial manager.
// Because the account receiver runs in Dial MX single mode, the receiver
// authorizes any domain on the one authenticated connection and needs no domain
// registration: Domains returns nil, exactly like the installation Remote
// backend. Resolve/Ingest route through the remotemx provider so the core keeps
// per-recipient account/domain authorization.
type remoteMXBackend struct{ service *Service }

func (s *Service) RemoteMXBackend() mxdial.Backend { return remoteMXBackend{service: s} }

func (b remoteMXBackend) Domains(context.Context) ([]mxdial.Domain, error) { return nil, nil }

func (b remoteMXBackend) Resolve(ctx context.Context, domain string, recipients []string) (mxwire.ResolveResponse, error) {
	response := mxwire.ResolveResponse{Version: mxwire.V2Protocol, MachineCode: mxwire.CodeOK}
	if len(recipients) == 0 {
		return mxwire.ResolveResponse{MachineCode: mxwire.CodeUnknownRecipient}, nil
	}
	for _, recipient := range recipients {
		if !strings.EqualFold(domain, domainOf(recipient)) {
			return mxwire.ResolveResponse{MachineCode: mxwire.CodeUnauthorized}, nil
		}
	}
	results := b.service.resolveMXRecipients(ctx, RemoteMXProvider, recipients)
	for _, result := range results {
		response.Results = append(response.Results, mxwire.ResolveRecipient{Recipient: result.Recipient, Accept: result.Accept, Domain: result.Domain, Code: string(result.Code), Temporary: result.Temporary})
	}
	return response, nil
}

func (b remoteMXBackend) Ingest(ctx context.Context, domains []string, meta mxwire.IngestMetadata, rawPath string) (mxwire.IngestResponse, error) {
	if len(domains) == 0 || len(meta.Recipients) == 0 {
		return mxwire.IngestResponse{MachineCode: mxwire.CodeUnauthorized}, nil
	}
	for _, recipient := range meta.Recipients {
		if !containsFold(domains, domainOf(recipient)) {
			return mxwire.IngestResponse{MachineCode: mxwire.CodeUnauthorized}, nil
		}
	}
	input := MXIngestInput{Recipients: meta.Recipients, EnvelopeFrom: meta.EnvelopeFrom, RawPath: rawPath, Size: meta.Size, ContentDigest: meta.ContentDigest, AuthResults: meta.AuthResults, TrustedAuth: true, ProviderMessageID: meta.ProviderMessageID}
	result, err := b.service.IngestRemoteMX(ctx, input)
	if err != nil {
		return mxwire.IngestResponse{}, err
	}
	response := mxwire.IngestResponse{Version: mxwire.V2Protocol, MachineCode: mxwire.CodeOK, MessageID: result.MessageID, PerRecipient: result.PerRecipient}
	for _, r := range result.PerRecipient {
		if r.MachineCode != mxwire.CodeOK && r.MachineCode != mxwire.CodeDuplicate {
			response.MachineCode = r.MachineCode
		}
	}
	return response, nil
}

// IngestRemoteMX persists a Remote MX message for the whole accepted recipient
// set through the shared staged mailbox pipeline.
func (s *Service) IngestRemoteMX(ctx context.Context, in MXIngestInput) (MXIngestResult, error) {
	return s.ingestMX(ctx, RemoteMXProvider, in)
}

// ResolveRemoteMXRecipients maps each envelope recipient to a routing decision
// for the Remote MX provider.
func (s *Service) ResolveRemoteMXRecipients(ctx context.Context, recipients []string) []MXResolveResult {
	return s.resolveMXRecipients(ctx, RemoteMXProvider, recipients)
}
