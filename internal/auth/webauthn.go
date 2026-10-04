// Package auth's WebAuthn support implements passkey registration and login.
// It is deliberately thin: the protocol cryptography (CBOR, attestation, COSE
// keys, origin/RP checks) lives in github.com/go-webauthn/webauthn, and this
// file only wires it to MailMoose's user store and a short-lived, in-memory
// challenge store.
package auth

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// WebAuthnConfig configures the relying party. RPID is the effective domain
// (the registrable domain, without scheme or port); RPOrigins are the exact
// origins permitted to complete a ceremony.
type WebAuthnConfig struct {
	RPDisplayName string
	RPID          string
	RPOrigins     []string
}

// WebAuthnUser is the credential-bearing view of a MailMoose user the ceremony
// layer needs. WebAuthnID must be stable and unique; the store uses the user's
// row id encoded to bytes.
type WebAuthnUser struct {
	ID          []byte
	Name        string
	DisplayName string
	Credentials []webauthn.Credential
}

func (u *WebAuthnUser) WebAuthnID() []byte                         { return u.ID }
func (u *WebAuthnUser) WebAuthnName() string                       { return u.Name }
func (u *WebAuthnUser) WebAuthnDisplayName() string                { return u.DisplayName }
func (u *WebAuthnUser) WebAuthnCredentials() []webauthn.Credential { return u.Credentials }

// challengeTTL bounds how long a begun ceremony may be completed. It matches
// the OAuth flow lifetime used elsewhere in the product.
const challengeTTL = 10 * time.Minute

// maxPendingChallenges caps the in-memory challenge store. A ceremony that is
// abandoned before completion is swept on TTL expiry; the cap is a safety valve
// against unbounded growth under abuse.
const maxPendingChallenges = 4096

type pendingChallenge struct {
	kind    challengeKind
	userID  []byte
	session webauthn.SessionData
	expires time.Time
}

type challengeKind int

const (
	challengeRegister challengeKind = iota
	challengeLogin
)

// ChallengeStore holds one-use WebAuthn ceremony state in memory. A restart
// expires every ceremony, which is safer than persisting browser authentication
// state. Tokens are looked up by a hash of the raw token so an accidental map
// dump does not reveal a live challenge handle.
type ChallengeStore struct {
	mu      sync.Mutex
	entries map[[32]byte]pendingChallenge
}

func NewChallengeStore() *ChallengeStore {
	return &ChallengeStore{entries: make(map[[32]byte]pendingChallenge)}
}

// Put stores a ceremony and returns the opaque token the client must echo back
// on the finish step.
func (s *ChallengeStore) Put(kind challengeKind, userID []byte, session webauthn.SessionData) (string, bool) {
	token, err := RandomToken(32)
	if err != nil {
		return "", false
	}
	key := sha256.Sum256([]byte(token))
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.entries {
		if !now.Before(v.expires) {
			delete(s.entries, k)
		}
	}
	if len(s.entries) >= maxPendingChallenges {
		return "", false
	}
	s.entries[key] = pendingChallenge{kind: kind, userID: userID, session: session, expires: now.Add(challengeTTL)}
	return token, true
}

// Take consumes a ceremony by its opaque token. It is one-use: a second call
// with the same token misses. The kind is checked so a registration challenge
// can never be replayed as a login challenge.
func (s *ChallengeStore) Take(token string, kind challengeKind) (userID []byte, session webauthn.SessionData, ok bool) {
	if token == "" {
		return nil, webauthn.SessionData{}, false
	}
	key := sha256.Sum256([]byte(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, found := s.entries[key]
	delete(s.entries, key)
	if !found || entry.kind != kind || !time.Now().Before(entry.expires) {
		return nil, webauthn.SessionData{}, false
	}
	return entry.userID, entry.session, true
}

// WebAuthnService is the ceremony entry point. It wraps the protocol library
// and the challenge store; persistence and user lookup stay in the store layer.
type WebAuthnService struct {
	wa         *webauthn.WebAuthn
	challenges *ChallengeStore
}

// NewWebAuthnService builds the service from relying-party configuration.
func NewWebAuthnService(cfg WebAuthnConfig) (*WebAuthnService, error) {
	if cfg.RPID == "" {
		return nil, errors.New("webauthn: RPID is required")
	}
	if len(cfg.RPOrigins) == 0 {
		return nil, errors.New("webauthn: at least one RP origin is required")
	}
	if cfg.RPDisplayName == "" {
		cfg.RPDisplayName = "MailMoose"
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPDisplayName: cfg.RPDisplayName,
		RPID:          cfg.RPID,
		RPOrigins:     cfg.RPOrigins,
		// Prefer user verification (biometric/PIN) where the authenticator
		// supports it, without rejecting security keys that only assert user
		// presence. This drives both registration and login defaults.
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			UserVerification: protocol.VerificationPreferred,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("webauthn: %w", err)
	}
	return &WebAuthnService{wa: wa, challenges: NewChallengeStore()}, nil
}

// RegistrationOptions is the JSON payload handed to navigator.credentials.create.
type RegistrationOptions struct {
	ChallengeToken string                       `json:"challenge_token"`
	Options        *protocol.CredentialCreation `json:"options"`
}

// BeginRegistration starts a registration ceremony for an already-authenticated
// user. Existing credentials are excluded so the same authenticator is not
// registered twice.
func (s *WebAuthnService) BeginRegistration(user *WebAuthnUser) (RegistrationOptions, error) {
	creation, session, err := s.wa.BeginRegistration(user,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithExclusions(webauthn.Credentials(user.WebAuthnCredentials()).CredentialDescriptors()),
	)
	if err != nil {
		return RegistrationOptions{}, err
	}
	token, ok := s.challenges.Put(challengeRegister, user.WebAuthnID(), *session)
	if !ok {
		return RegistrationOptions{}, errors.New("webauthn: too many pending ceremonies")
	}
	return RegistrationOptions{ChallengeToken: token, Options: creation}, nil
}

// RegisteredCredential is the result of a successful registration, in the
// terms the store persists.
type RegisteredCredential struct {
	Credential      webauthn.Credential
	AttestationType string
	AAGUID          []byte
	BackupEligible  bool
	BackupState     bool
}

// FinishRegistration validates the authenticator's response and returns the
// credential to persist. It consumes the challenge token.
func (s *WebAuthnService) FinishRegistration(user *WebAuthnUser, challengeToken string, r *http.Request) (RegisteredCredential, error) {
	userID, session, ok := s.challenges.Take(challengeToken, challengeRegister)
	if !ok {
		return RegisteredCredential{}, errors.New("webauthn: registration challenge expired or unknown")
	}
	// The stored challenge belongs to exactly one user; refuse a token minted
	// for a different account.
	if string(userID) != string(user.WebAuthnID()) {
		return RegisteredCredential{}, errors.New("webauthn: registration challenge user mismatch")
	}
	cred, err := s.wa.FinishRegistration(user, session, r)
	if err != nil {
		return RegisteredCredential{}, err
	}
	return RegisteredCredential{
		Credential:      *cred,
		AttestationType: cred.AttestationType,
		AAGUID:          cred.Authenticator.AAGUID,
		BackupEligible:  cred.Flags.BackupEligible,
		BackupState:     cred.Flags.BackupState,
	}, nil
}

// LoginOptions is the JSON payload handed to navigator.credentials.get for a
// usernameless (discoverable credential) login.
type LoginOptions struct {
	ChallengeToken string                        `json:"challenge_token"`
	Options        *protocol.CredentialAssertion `json:"options"`
}

// BeginDiscoverableLogin starts a usernameless login. The browser selects one
// of the resident passkeys and the finish step resolves the user from the
// credential id, so no email is required up front.
func (s *WebAuthnService) BeginDiscoverableLogin() (LoginOptions, error) {
	assertion, session, err := s.wa.BeginDiscoverableLogin(
		webauthn.WithUserVerification(protocol.VerificationPreferred),
	)
	if err != nil {
		return LoginOptions{}, err
	}
	token, ok := s.challenges.Put(challengeLogin, nil, *session)
	if !ok {
		return LoginOptions{}, errors.New("webauthn: too many pending ceremonies")
	}
	return LoginOptions{ChallengeToken: token, Options: assertion}, nil
}

// DiscoveredCredential is the outcome of a login ceremony: the credential the
// browser presented. The caller resolves it to a user via the store.
type DiscoveredCredential struct {
	Credential webauthn.Credential
	UserHandle []byte
}

// FinishDiscoverableLogin validates the assertion. The supplied handler is
// invoked with the presented credential id and user handle so the caller can
// load the matching store row and construct the user to verify against.
func (s *WebAuthnService) FinishDiscoverableLogin(challengeToken string, r *http.Request, resolve func(credentialID, userHandle []byte) (*WebAuthnUser, error)) (DiscoveredCredential, error) {
	_, session, ok := s.challenges.Take(challengeToken, challengeLogin)
	if !ok {
		return DiscoveredCredential{}, errors.New("webauthn: login challenge expired or unknown")
	}
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		u, err := resolve(rawID, userHandle)
		if err != nil {
			return nil, err
		}
		return u, nil
	}
	user, cred, err := s.wa.FinishPasskeyLogin(handler, session, r)
	if err != nil {
		return DiscoveredCredential{}, err
	}
	return DiscoveredCredential{Credential: *cred, UserHandle: user.WebAuthnID()}, nil
}
