//go:build !unix

package main

import (
	"fmt"
	"os"
)

// Non-Unix fallback verifies both path identity and the opened regular file.
func openWSHistoryFile(path string, flags int) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("WebSocket history must be a regular file, not a symlink")
	}
	f, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		f.Close()
		return nil, fmt.Errorf("WebSocket history changed while opening")
	}
	return f, nil
}

func lockWSHistoryFile(path string) (func() error, error) {
	return nil, fmt.Errorf("persistent WebSocket history needs an exclusive file lock, unsupported on this platform")
}
