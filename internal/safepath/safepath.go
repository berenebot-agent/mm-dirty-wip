// Package safepath resolves a stored relative path beneath a data root and
// rejects any value that could escape it.
//
// Every on-disk location MailMoose stores is server-generated and kept under
// DataDir, and the relative form is what is persisted. The path is therefore
// never attacker-controlled today. Join is a defence-in-depth guard so a future
// path that does store a client-supplied value cannot turn a read, open or
// remove into arbitrary file access.
package safepath

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Join resolves rel beneath root, returning the cleaned path. It rejects a NUL
// byte, a backslash (a path separator on some platforms, and never needed in a
// stored relative path), an absolute path, and any value that would resolve to
// or above root. On rejection it returns an error and no path.
func Join(root, rel string) (string, error) {
	if strings.IndexByte(rel, 0) >= 0 {
		return "", fmt.Errorf("safepath: path contains NUL")
	}
	if strings.ContainsRune(rel, '\\') {
		return "", fmt.Errorf("safepath: path contains backslash")
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." {
		return "", fmt.Errorf("safepath: empty path")
	}
	if filepath.IsAbs(clean) {
		return "", fmt.Errorf("safepath: absolute path rejected")
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("safepath: path escapes root")
	}
	joined := filepath.Join(root, clean)
	// Re-check after joining: a symlinked root or a platform quirk must not
	// silently place the result outside root.
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	joinedAbs, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}
	back, err := filepath.Rel(rootAbs, joinedAbs)
	if err != nil || filepath.IsAbs(back) || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("safepath: path escapes root")
	}
	return joined, nil
}
