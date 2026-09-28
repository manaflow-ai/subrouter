//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// pidAlive reports whether a process with this ID exists. EPERM means it
// exists under another user, which still counts.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// detachSessionProcess starts cmd in its own session, so it outlives the
// status line command and is not signalled with the agent's process group.
func detachSessionProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
