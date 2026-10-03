//go:build windows

package main

import "os"

// fileOwnerUID reports no owner on Windows, which has no Unix user IDs; the
// ingestor is only deployed on Linux.
func fileOwnerUID(os.FileInfo) (int, bool) { return 0, false }
