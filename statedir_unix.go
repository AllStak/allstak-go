//go:build unix

package allstak

import (
	"os"
	"syscall"
)

// checkDirOwnership verifies that the directory described by fi is owned by
// the current uid. A dir owned by another user (e.g. an attacker pre-created
// /tmp/allstak-spool) is refused.
func checkDirOwnership(fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil // no ownership info available — nothing to check
	}
	if int(st.Uid) != os.Getuid() {
		return errNotOwned
	}
	return nil
}
