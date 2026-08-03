//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"time"
)

func runDeployTestCommand(command *exec.Cmd) ([]byte, error) {
	command.Cancel = func() error {
		return killDeployTestProcess(command)
	}
	command.WaitDelay = time.Second
	return command.CombinedOutput()
}

func killDeployTestProcess(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}
	err := command.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
