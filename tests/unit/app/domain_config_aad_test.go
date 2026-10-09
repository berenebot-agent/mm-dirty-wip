package app_test

import (
	"context"
	"testing"
)

// TestDomainConfigAADBindsToDomain proves an encrypted domain sending config is
// bound to its owning domain: relabelling the ciphertext to another domain (a
// cross-row copy) fails to decrypt.
func TestDomainConfigAADBindsToDomain(t *testing.T) {
	svc, u, domA, _ := testService(t)
	ctx := context.Background()
	domB, err := svc.Store.CreateDomain(ctx, u.AccountID, "second.example.com")
	if err != nil {
		t.Fatal(err)
	}
	seedSending(t, svc, u.AccountID, domA.ID, "brevo", map[string]any{"api_key": "secret-a"})

	savedA, err := svc.Store.GetDomainSendingConfig(ctx, u.AccountID, domA.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The genuine config decrypts under its own domain.
	if v, err := svc.DecryptDomainSendingConfig(savedA); err != nil || v["api_key"] != "secret-a" {
		t.Fatalf("own-domain decrypt = %v err=%v", v, err)
	}
	// The same ciphertext relabelled to another domain must not.
	swapped := savedA
	swapped.DomainID = domB.ID
	if _, err := svc.DecryptDomainSendingConfig(swapped); err == nil {
		t.Fatal("ciphertext decrypted after being relabelled to another domain")
	}
	// And an AAD-bound blob must not decrypt through the legacy AAD-less path.
	if _, err := svc.DecryptSecret(savedA.EncryptedConfig); err == nil {
		t.Fatal("AAD-bound config decrypted via the legacy AAD-less path")
	}
}
