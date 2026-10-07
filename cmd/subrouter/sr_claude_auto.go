package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/fsutil"
)

// Team administrators can provision this environment variable once for every
// developer. An individual user can instead run "sr claude mode auto" once.
const claudePoolLaunchModeEnv = "SUBROUTER_CLAUDE_LAUNCH_MODE"

type claudePoolLaunchMode string

const (
	claudePoolLaunchChoose claudePoolLaunchMode = "choose"
	claudePoolLaunchAuto   claudePoolLaunchMode = "auto"
)

type claudePoolLaunchPreference struct {
	Mode claudePoolLaunchMode `json:"mode"`
}

func parseClaudePoolLaunchMode(raw string) (claudePoolLaunchMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "auto":
		return claudePoolLaunchAuto, nil
	case "choose":
		return claudePoolLaunchChoose, nil
	default:
		return "", fmt.Errorf("invalid Claude launch mode %q: expected auto or choose", raw)
	}
}

// The launch preference is local to each developer. No subscription credential
// or account ID is stored here; the server selects the serving account.
func (r srRunner) claudePoolLaunchPreferencePath() string {
	return filepath.Join(r.store.StoreDir(), "claude-launch-mode.json")
}

func (r srRunner) claudePoolLaunchMode() (claudePoolLaunchMode, error) {
	if raw := strings.TrimSpace(os.Getenv(claudePoolLaunchModeEnv)); raw != "" {
		mode, err := parseClaudePoolLaunchMode(raw)
		if err != nil {
			return "", fmt.Errorf("%s: %w", claudePoolLaunchModeEnv, err)
		}
		return mode, nil
	}
	data, err := os.ReadFile(r.claudePoolLaunchPreferencePath())
	if errors.Is(err, os.ErrNotExist) {
		return claudePoolLaunchChoose, nil // compatible with previous releases
	}
	if err != nil {
		return "", fmt.Errorf("read Claude launch preference: %w", err)
	}
	var saved claudePoolLaunchPreference
	if err := json.Unmarshal(data, &saved); err != nil {
		return "", fmt.Errorf("read Claude launch preference: %w", err)
	}
	mode, err := parseClaudePoolLaunchMode(string(saved.Mode))
	if err != nil {
		return "", fmt.Errorf("read Claude launch preference: %w", err)
	}
	return mode, nil
}

func (r srRunner) saveClaudePoolLaunchMode(mode claudePoolLaunchMode) error {
	if _, err := parseClaudePoolLaunchMode(string(mode)); err != nil {
		return err
	}
	path := r.claudePoolLaunchPreferencePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create Claude launch preference directory: %w", err)
	}
	body, err := json.MarshalIndent(claudePoolLaunchPreference{Mode: mode}, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if err := fsutil.ReplaceFile(path, body, 0o600); err != nil {
		return fmt.Errorf("save Claude launch preference: %w", err)
	}
	return nil
}

func (r srRunner) claudePoolModeCommand(args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("usage: sr claude mode [auto|choose]")
	}
	if len(args) == 0 {
		mode, err := r.claudePoolLaunchMode()
		if err != nil {
			return err
		}
		fmt.Fprintf(r.out, "Claude pooled launch mode: %s\n", mode)
		return nil
	}
	mode, err := parseClaudePoolLaunchMode(args[0])
	if err != nil {
		return err
	}
	if err := r.saveClaudePoolLaunchMode(mode); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Claude pooled launch mode saved: %s\n", mode)
	if strings.TrimSpace(os.Getenv(claudePoolLaunchModeEnv)) != "" {
		fmt.Fprintf(r.out, "Note: %s currently overrides the saved preference.\n", claudePoolLaunchModeEnv)
	}
	return nil
}

// Pooled auto mode applies to implicit Claude Code launches, including
// streaming/non-interactive calls and resume. Every explicit "proxy", "run",
// login, profile management or help command keeps its original semantics.
func claudeImplicitPoolLaunch(args []string) bool {
	if len(args) == 0 {
		return true
	}
	if args[0] == "-h" || args[0] == "--help" {
		return false
	}
	return strings.HasPrefix(args[0], "-")
}

func (r srRunner) claudePooledAutoLaunch(ctx context.Context, args []string) (bool, error) {
	if !claudeImplicitPoolLaunch(args) {
		return false, nil
	}
	mode, err := r.claudePoolLaunchMode()
	if err != nil {
		return true, err
	}
	if mode != claudePoolLaunchAuto {
		return false, nil
	}
	// No interactive picker and no preferred account. The selected server
	// remains responsible for choosing accounts, sticky sessions and failover.
	return true, r.proxyClaudeSelectedRemote(ctx, args, claudeProxyLaunchOptions{})
}
