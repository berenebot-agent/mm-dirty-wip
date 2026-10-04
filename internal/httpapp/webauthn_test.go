package httpapp

import (
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
)

// TestToWebAuthnCredentialsRestoresBackupFlags guards the login-path bug where
// the reconstructed credential omitted BackupEligible/BackupState, causing the
// library to reject every synced (backup-eligible) passkey with "Backup Eligible
// flag inconsistency".
func TestToWebAuthnCredentialsRestoresBackupFlags(t *testing.T) {
	creds := []model.WebAuthnCredential{{
		CredentialID:   []byte{1, 2, 3, 4},
		PublicKey:      []byte{5, 6, 7},
		SignCount:      9,
		BackupEligible: true,
		BackupState:    true,
	}}
	out := toWebAuthnCredentials(creds)
	if len(out) != 1 {
		t.Fatalf("got %d credentials", len(out))
	}
	if !out[0].Flags.BackupEligible || !out[0].Flags.BackupState {
		t.Fatalf("backup flags dropped: %#v", out[0].Flags)
	}
	if out[0].Authenticator.SignCount != 9 {
		t.Fatalf("sign count = %d, want 9", out[0].Authenticator.SignCount)
	}
}
