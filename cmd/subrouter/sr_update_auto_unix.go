//go:build !windows

package main

import "syscall"

// detachedProcessAttributes starts the background update in its own session,
// so it outlives the command that started it and never reads its terminal.
func detachedProcessAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
