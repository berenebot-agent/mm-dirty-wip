// Package privdrop covers the deployment case where a Gatehouse process is
// started as root — for example the default image (no Dockerfile USER, so the
// container boots as root) or `user: "0:0"` in Compose — so a Docker-created
// bind-mount directory can be fixed up without host-side chown commands. It
// hands the runtime directory to the image's non-root runtime user and then
// drops privileges before the database is opened or any request is served. The
// application passes its data directory; the MX edge passes its staging
// directory, which is its only writable path.
//
// When the process is already non-root — the opt-in hardened posture with a
// strict `user:` in compose — it is a no-op, so the read-only / caps-dropped
// compose never needs setuid or chown capabilities.
package privdrop

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// DefaultUID and DefaultGID match the image's runtime user (uid/gid 65532).
const (
	DefaultUID = 65532
	DefaultGID = 65532
)

// DropToRuntimeUser chowns dir (recursively) to the runtime UID/GID and
// switches the running process to that user. It reports whether a drop
// actually happened and the UID/GID that was applied (whether dropped or
// no-op, so callers can log the runtime identity without re-reading the
// environment). Once dropped, privileges cannot be regained.
func DropToRuntimeUser(dir string) (dropped bool, uid int, gid int, err error) {
	uid, err = envID("GATEHOUSE_RUN_UID", DefaultUID)
	if err != nil {
		return false, 0, 0, err
	}
	gid, err = envID("GATEHOUSE_RUN_GID", DefaultGID)
	if err != nil {
		return false, 0, 0, err
	}
	if os.Getuid() != 0 {
		return false, uid, gid, nil
	}
	if err := ChownDataDir(dir, uid, gid); err != nil {
		return false, uid, gid, err
	}
	if err := DropTo(uid, gid); err != nil {
		return false, uid, gid, err
	}
	return true, uid, gid, nil
}

// ChownDataDir creates dir if absent, chowns it recursively to uid/gid, and
// forces the top-level directory to 0700. The mode is set explicitly because a
// bind-mounted directory keeps whatever mode the host gave it (commonly 0755),
// which would let the embedded edge's uid traverse /data even though it does
// not own it. 0700 on the root blocks traversal for any other uid; individual
// files inside remain 0600 from the app. Callers must already be root.
func ChownDataDir(dir string, uid, gid int) error {
	// The bind mount may be a fresh, empty root-owned directory; create it (as
	// root) before the walk so a missing directory does not fail the chown.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create runtime directory: %w", err)
	}
	if err := chownRecursive(dir, uid, gid); err != nil {
		return fmt.Errorf("chown runtime directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure runtime directory: %w", err)
	}
	return nil
}

// DropTo permanently sheds privileges to uid/gid (supplementary groups, then
// group, then user). Callers must already be root. Once dropped, privileges
// cannot be regained.
func DropTo(uid, gid int) error {
	// Supplementary groups, then group, then user: each call permanently
	// sheds the privileges the next one depends on, so the order matters.
	if err := syscall.Setgroups([]int{gid}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("setgid %d: %w", gid, err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("setuid %d: %w", uid, err)
	}
	if actualUID := os.Getuid(); actualUID != uid {
		return fmt.Errorf("setuid %d succeeded but process UID is %d", uid, actualUID)
	}
	return nil
}

// ResolvedIdentity returns the UID and GID the running process would be
// configured with if DropToRuntimeUser were invoked now. It honours
// GATEHOUSE_RUN_UID / GATEHOUSE_RUN_GID and falls back to DefaultUID/DefaultGID.
// It does not perform any privilege change.
func ResolvedIdentity() (uid, gid int, err error) {
	uid, err = envID("GATEHOUSE_RUN_UID", DefaultUID)
	if err != nil {
		return 0, 0, err
	}
	gid, err = envID("GATEHOUSE_RUN_GID", DefaultGID)
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

func envID(name string, fallback int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("%s must be a positive non-zero integer; cannot drop privileges to root (0), got %q", name, raw)
	}
	return v, nil
}

// chownRecursive uses Lchown to avoid following symlinks, which could point
// outside the data directory.
func chownRecursive(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, uid, gid)
	})
}
