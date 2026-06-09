package allstak

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

// Hardened creation/validation of the SDK's on-disk state directories (the
// offline spool and the session-state dir).
//
// Threat model: on a shared multi-user host an attacker can pre-create a
// predictable world-tmp directory (mode 0777) or replace it with a symlink
// before the service starts. os.MkdirAll happily succeeds on an existing dir
// regardless of owner or mode, and a follow-up write through a symlink is an
// arbitrary-file overwrite with the service user's privileges. To close this
// class:
//
//   - state dirs are created 0700 and chmod'ed back to 0700 if a pre-existing
//     dir is looser;
//   - the dir is Lstat'ed and REFUSED if it is a symlink (or not a directory);
//   - on Unix the dir's owner must be the current uid, otherwise it is
//     refused (an attacker-owned pre-created dir fails here);
//   - the default world-tmp fallback paths embed the uid
//     (e.g. /tmp/allstak-spool-501) so two users never share a state dir.
//
// All failures are surfaced as errors; callers stay fail-open by degrading to
// in-memory/no-op behavior — a refused dir must never crash the host.

// ensureSecureDir creates dir (0700) if needed and validates that it is a
// real, caller-owned, 0700 directory. It returns an error — and the caller
// must NOT use the directory — when dir is a symlink, not a directory, owned
// by another user (Unix), or cannot be created/tightened.
func ensureSecureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("allstak: state dir %s is a symlink — refusing to use it", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("allstak: state path %s is not a directory", dir)
	}
	if err := checkDirOwnership(fi); err != nil {
		return fmt.Errorf("allstak: state dir %s: %w", dir, err)
	}
	// MkdirAll does not tighten a pre-existing looser dir — chmod it to 0700
	// and verify, so a pre-created 0777 dir cannot stay world-writable.
	if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("allstak: state dir %s has mode %o and cannot be tightened: %w", dir, fi.Mode().Perm(), err)
		}
	}
	return nil
}

// errNotOwned is returned (wrapped) when a state dir belongs to another user.
var errNotOwned = errors.New("owned by another user — refusing to use it")

// uidSuffix returns "-<uid>" on platforms with real uids, or "" where
// os.Getuid is meaningless (Windows returns -1). Appended to the world-tmp
// fallback dir names so state paths are unique per user.
func uidSuffix() string {
	if uid := os.Getuid(); uid >= 0 {
		return "-" + strconv.Itoa(uid)
	}
	return ""
}
