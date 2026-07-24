package main

import (
	"context"
	"io"
	"os"
	"os/exec"
)

type srCommandRunner interface {
	Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error
	RunWithEnv(ctx context.Context, name string, args []string, env []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error
	Output(ctx context.Context, name string, args []string) ([]byte, error)
}

type execSRCommandRunner struct{}

func (execSRCommandRunner) Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	return execSRCommandRunner{}.RunWithEnv(ctx, name, args, nil, stdin, stdout, stderr)
}

func (execSRCommandRunner) RunWithEnv(ctx context.Context, name string, args []string, env []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	return cmd.Run()
}

func (execSRCommandRunner) Output(ctx context.Context, name string, args []string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

func (r srRunner) commandRunner() srCommandRunner {
	if r.cmd != nil {
		return r.cmd
	}
	return execSRCommandRunner{}
}
