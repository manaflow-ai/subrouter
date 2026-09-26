package main

import (
	"flag"
	"fmt"
	"strings"
)

// parseFlagsAnywhere parses flags that appear before or after positional
// arguments and returns the positionals in order. The standard flag package
// stops at the first positional, which made `sr reset <email> --dry-run`
// silently redeem a real credit. Everything after a literal "--" stays
// positional.
//
// Do not use this for commands that pass trailing arguments through to a
// child process (sr claude, sr codex, sr kimi/qwen launchers, supervise, ...):
// those must leave the child's flags alone.
func parseFlagsAnywhere(flags *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := flags.Parse(args); err != nil {
			return nil, err
		}
		rest := flags.Args()
		consumed := len(args) - len(rest)
		if consumed > 0 && args[consumed-1] == "--" {
			return append(positional, rest...), nil
		}
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// parseFlagsNoPositionals parses flags for a command that takes none, and
// rejects stray positionals instead of silently ignoring every flag after
// them.
func parseFlagsNoPositionals(flags *flag.FlagSet, args []string) error {
	positional, err := parseFlagsAnywhere(flags, args)
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(positional, " "))
	}
	return nil
}

// parseFlagsOneName parses flags anywhere around exactly one positional name,
// returning usage as the error when the name is missing or repeated.
func parseFlagsOneName(flags *flag.FlagSet, args []string, usage error) (string, error) {
	positional, err := parseFlagsAnywhere(flags, args)
	if err != nil {
		return "", err
	}
	if len(positional) > 1 {
		return "", fmt.Errorf("unexpected arguments: %s\n%w", strings.Join(positional[1:], " "), usage)
	}
	if len(positional) == 0 {
		return "", usage
	}
	return positional[0], nil
}
