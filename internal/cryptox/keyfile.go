package cryptox

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// KeyFileName is the name of the key file an operator does not have to supply:
// it is generated into the data directory on first start.
const KeyFileName = "encryption_key"

// LoadOrCreateKeyFile returns the encryption key stored at path, generating and
// persisting a fresh random one when no file exists. generated reports that
// this call created the file, so the caller can tell the operator to back it
// up.
//
// An existing file is never overwritten and never regenerated: a key that no
// longer matches the stored ciphertext is unrecoverable, so a file that cannot
// be parsed is an error rather than a reason to mint a new one. The hard-link
// create makes concurrent starts converge on the first writer's key, and makes
// a reader of path never observe a partially written file.
//
// shortcut: hard links need a filesystem that supports them (overlayfs, ext4
// and NFS do); on a link-less filesystem generation fails instead of racing.
func LoadOrCreateKeyFile(path string) (key string, generated bool, err error) {
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		k := strings.TrimSpace(string(raw))
		if k == "" {
			return "", false, fmt.Errorf("encryption key file %s is empty; restore it from backup, or delete it to generate a new one", path)
		}
		if _, derr := DeriveKey(k); derr != nil {
			return "", false, fmt.Errorf("encryption key file %s is not a valid key: %w", path, derr)
		}
		return k, false, nil
	case !errors.Is(err, fs.ErrNotExist):
		return "", false, fmt.Errorf("read encryption key file %s: %w", path, err)
	}

	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", false, fmt.Errorf("generate encryption key: %w", err)
	}
	k := hex.EncodeToString(buf[:])

	tmp, err := os.CreateTemp(filepath.Dir(path), KeyFileName+".tmp")
	if err != nil {
		return "", false, fmt.Errorf("create encryption key file in %s: %w", filepath.Dir(path), err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(k + "\n"); err != nil {
		tmp.Close()
		return "", false, fmt.Errorf("write encryption key file %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return "", false, fmt.Errorf("write encryption key file %s: %w", path, err)
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			// Another start created the key first; its key is the instance's key.
			return LoadOrCreateKeyFile(path)
		}
		return "", false, fmt.Errorf("create encryption key file %s: %w", path, err)
	}
	return k, true, nil
}
