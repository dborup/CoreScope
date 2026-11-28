//go:build windows

package main

import "os"

// configFileOwnerIDs reports no owner on Windows, which has no Unix user or
// group IDs; the server is only deployed on Linux. The atomic config
// replacement then skips its ownership step and relies on the preserved
// permission bits alone.
func configFileOwnerIDs(os.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }
