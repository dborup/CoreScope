//go:build unix

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// NOFOLLOW rejects symlinks atomically; NONBLOCK prevents a replaced FIFO
// from hanging startup. No caller writes until the regular-file check passes.
func openWSHistoryFile(path string, flags int) (*os.File, error) {
	fd, err := unix.Open(path, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("WebSocket history must be a regular file")
	}
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		f.Close()
		return nil, err
	}
	if info.Nlink != 1 {
		f.Close()
		return nil, fmt.Errorf("WebSocket history must not be hardlinked")
	}
	// New/rewritten journals are always created at exactly 0600 (CreateTemp's
	// default). A restored backup can legitimately come back as 0644 (tar/cp
	// under umask 022); only reject group/other write access, which would let
	// another local user tamper with the journal.
	if st.Mode().Perm()&0022 != 0 {
		f.Close()
		return nil, fmt.Errorf("WebSocket history must not be group- or other-writable")
	}
	return f, nil
}

func lockWSHistoryFile(path string) (func() error, error) {
	f, err := openWSHistoryFile(path, os.O_RDWR|os.O_CREATE)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another server owns this history file: %w", err)
	}
	return f.Close, nil
}
