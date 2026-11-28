//go:build !windows

package main

import (
	"os"
	"syscall"
)

// configFileOwnerIDs returns the user and group IDs that own fi. Used by the
// atomic config replacement (#340) to decide whether the replacement file
// already has the right ownership.
func configFileOwnerIDs(fi os.FileInfo) (uid, gid int, ok bool) {
	st, k := fi.Sys().(*syscall.Stat_t)
	if !k {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
