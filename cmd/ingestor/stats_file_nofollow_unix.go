//go:build !windows

package main

import "syscall"

// oNoFollow is syscall.O_NOFOLLOW on platforms that define it (all non-Windows targets).
// On Windows this constant does not exist; see stats_file_nofollow_windows.go.
const oNoFollow = syscall.O_NOFOLLOW

// oNonBlock is syscall.O_NONBLOCK: opening a FIFO for writing without it
// waits for a reader, forever if none comes (#161).
const oNonBlock = syscall.O_NONBLOCK
