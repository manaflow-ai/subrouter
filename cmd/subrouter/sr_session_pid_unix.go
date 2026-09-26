//go:build !windows

package main

import "syscall"

// pidAlive reports whether a process with this ID exists. EPERM means it
// exists under another user, which still counts.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
