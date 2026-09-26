package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

const srDaemonHelp = `sr daemon - Manage this machine's local proxy

Usage:
  sr daemon start
  sr daemon stop
  sr daemon restart
  sr daemon status
  sr daemon logs
  sr daemon bind-state <absolute-state-dir> --local-data-socket PATH [--if-current-absent]
                       [--if-current-sha256 SHA --if-current-mode MODE]
  sr daemon unbind-state
`

func runDaemonCommand(
	ctx context.Context,
	args []string,
	store accounts.CodexStore,
	out io.Writer,
	errOut io.Writer,
) error {
	if len(args) == 0 {
		args = []string{"status"}
	}
	switch args[0] {
	case "start", "up":
		return runServerLifecycle("start", out)
	case "stop", "down":
		return runServerLifecycle("stop", out)
	case "restart":
		return runServerLifecycle("restart", out)
	case "status":
		return runServerHealth(ctx, out)
	case "logs":
		return followDaemonLogs(ctx, out, errOut)
	case "bind-state":
		if len(args) < 2 {
			return fmt.Errorf("usage: sr daemon bind-state <absolute-state-dir> --local-data-socket PATH [--if-current-absent | --if-current-sha256 SHA --if-current-mode MODE]")
		}
		expectation, err := parseLocalServingStoreExpectation(args[2:])
		if err != nil {
			return err
		}
		if expectation.LocalDataSocket == "" {
			return errors.New("bind-state requires --local-data-socket")
		}
		return bindLocalServingStoreIfCurrent(ctx, args[1], store, out, expectation)
	case "unbind-state":
		if len(args) != 1 {
			return fmt.Errorf("usage: sr daemon unbind-state")
		}
		return unbindLocalServingStore(store, out)
	case "help", "-h", "--help":
		fmt.Fprint(out, srDaemonHelp)
		return nil
	default:
		return fmt.Errorf("unknown daemon command %q\n%s", args[0], srDaemonHelp)
	}
}

func followDaemonLogs(ctx context.Context, out io.Writer, errOut io.Writer) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		command = exec.CommandContext(
			ctx,
			"tail",
			"-n",
			"100",
			"-F",
			filepath.Join(home, "Library", "Logs", "subrouter.log"),
			filepath.Join(home, "Library", "Logs", "subrouter.err.log"),
		)
	case "linux":
		command = exec.CommandContext(
			ctx,
			"journalctl",
			"--user",
			"--unit",
			defaultSystemdServiceName,
			"--lines",
			"100",
			"--follow",
		)
	default:
		return fmt.Errorf("daemon logs are not supported on %s", runtime.GOOS)
	}
	command.Stdout = out
	command.Stderr = errOut
	return command.Run()
}
