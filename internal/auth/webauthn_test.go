package auth

import (
	"testing"

	"github.com/go-webauthn/webauthn/webauthn"
)

// TestChallengeStoreIsOneUseAndKindScoped guards the ceremony token contract:
// a token can be consumed exactly once, and a registration challenge cannot be
// replayed as a login challenge.
func TestChallengeStoreIsOneUseAndKindScoped(t *testing.T) {
	s := NewChallengeStore()
	token, ok := s.Put(challengeRegister, []byte("user-1"), webauthn.SessionData{Challenge: "abc"})
	if !ok || token == "" {
		t.Fatal("Put returned no token")
	}
	// A token minted for registration must not be accepted as a login token.
	if _, _, ok := s.Take(token, challengeLogin); ok {
		t.Fatal("registration challenge accepted as login")
	}
	// But it is consumed even by the wrong-kind attempt (one-use on lookup).
	if _, _, ok := s.Take(token, challengeRegister); ok {
		t.Fatal("challenge survived a mismatched take")
	}
}

func TestChallengeStoreTakeReturnsSession(t *testing.T) {
	s := NewChallengeStore()
	token, ok := s.Put(challengeLogin, nil, webauthn.SessionData{Challenge: "xyz"})
	if !ok {
		t.Fatal("Put failed")
	}
	userID, session, ok := s.Take(token, challengeLogin)
	if !ok || userID != nil || session.Challenge != "xyz" {
		t.Fatalf("Take = %v %q %v", userID, session.Challenge, ok)
	}
	if _, _, ok := s.Take(token, challengeLogin); ok {
		t.Fatal("challenge used twice")
	}
}
