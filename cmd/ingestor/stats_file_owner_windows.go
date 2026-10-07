//go:build windows

package main

import "os"

// fileOwnerUID reports no owner on Windows, which has no Unix user IDs; the
// ingestor is only deployed on Linux.
func fileOwnerUID(os.FileInfo) (int, bool) { return 0, false }

// fileLinkCount reports no link count on Windows, where FileInfo does not
// carry one.
func fileLinkCount(os.FileInfo) (uint64, bool) { return 0, false }
