//go:build windows

package main

import (
	"os"
	"os/exec"
)

func processAlivePlatform(process *os.Process) bool {
	// The golden gate refuses to run on Windows. This implementation exists so
	// repository release cross-compiles still verify the command package.
	return process != nil
}

func configureProcessGroup(_ *exec.Cmd) {}

func interruptProcessGroup(command *exec.Cmd) {
	if command != nil && command.Process != nil {
		_ = command.Process.Signal(os.Interrupt)
	}
}

func terminateProcessGroup(command *exec.Cmd) {
	if command != nil && command.Process != nil {
		_ = command.Process.Signal(os.Interrupt)
	}
}

func killProcessGroup(command *exec.Cmd) {
	if command != nil && command.Process != nil {
		_ = command.Process.Kill()
	}
}
