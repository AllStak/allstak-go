//go:build !unix

package allstak

import "os"

// checkDirOwnership is a no-op on platforms without Unix uid semantics
// (Windows). The symlink and mode checks in ensureSecureDir still apply.
func checkDirOwnership(_ os.FileInfo) error {
	return nil
}
