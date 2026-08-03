//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func processAlivePlatform(process *os.Process) bool {
	if process == nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

func configureProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func interruptProcessGroup(command *exec.Cmd) {
	if command != nil && command.Process != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGINT)
	}
}

func killProcessGroup(command *exec.Cmd) {
	if command != nil && command.Process != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
}
