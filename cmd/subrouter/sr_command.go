package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
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
		cmd.Env = mergeCommandEnv(os.Environ(), env)
	}
	return cmd.Run()
}

func mergeCommandEnv(base, overrides []string) []string {
	overridden := make(map[string]struct{}, len(overrides))
	for _, value := range overrides {
		if key, _, ok := strings.Cut(value, "="); ok {
			overridden[commandEnvKey(key)] = struct{}{}
		}
	}
	merged := make([]string, 0, len(base)+len(overrides))
	for _, value := range base {
		key, _, ok := strings.Cut(value, "=")
		if ok {
			if _, replace := overridden[commandEnvKey(key)]; replace {
				continue
			}
		}
		merged = append(merged, value)
	}
	return append(merged, overrides...)
}

func commandEnvKey(key string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(key)
	}
	return key
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
