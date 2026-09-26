package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// naked runs a coding agent directly, preserving every argument and removing
// subrouter routing variables so users can select any installed agent.
func naked(args []string) error {
	agent := "codex"
	if len(args) > 0 {
		switch args[0] {
		case "claude", "codex", "opencode", "pi", "qwen", "kimi", "agy", "gemini":
			agent, args = args[0], args[1:]
		}
	}
	path, err := exec.LookPath(agent)
	if err != nil {
		return fmt.Errorf("%s is not installed or is not on PATH; install it before running `sr naked %s`", agent, agent)
	}
	cmd := exec.Command(path, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	env := os.Environ()
	filtered := env[:0]
	for _, item := range env {
		if strings.HasPrefix(item, "SUBROUTER_") || strings.HasPrefix(item, "CODEROUTER_") {
			continue
		}
		filtered = append(filtered, item)
	}
	cmd.Env = filtered
	return cmd.Run()
}
