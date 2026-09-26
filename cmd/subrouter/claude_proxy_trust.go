package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/fsutil"
)

// Pooled Claude launches run in their own config directory, so Claude's
// per-folder trust ("Do you trust the files in this folder?") granted in the
// user's normal Claude never carried over, and every proxy launch asked
// again. These flags are shared between the user's real .claude.json and the
// proxy's: a flag the user accepted on either side is copied to the other.
// Only true values move, so no side ever gains trust the user did not grant,
// and nothing is ever revoked.
var claudeSharedProjectFlags = []string{
	"hasTrustDialogAccepted",
	"hasCompletedProjectOnboarding",
	"hasClaudeMdExternalIncludesApproved",
	"hasClaudeMdExternalIncludesWarningShown",
}

// claudeConfigLockWait bounds how long sr waits for Claude's own lock on a
// config file before leaving it alone for this launch.
const claudeConfigLockWait = time.Second

// claudeUserConfigPath is the .claude.json the user's normal Claude reads:
// $CLAUDE_CONFIG_DIR/.claude.json when set, else ~/.claude.json.
func claudeUserConfigPath() string {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return filepath.Join(dir, ".claude.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude.json")
}

// syncClaudeProjectTrust shares accepted project flags between the user's
// config and a proxy config directory, in both directions. Failures are
// returned for logging only; a launch never depends on this.
func syncClaudeProjectTrust(userConfigPath, proxyConfigDir string) error {
	if strings.TrimSpace(userConfigPath) == "" || strings.TrimSpace(proxyConfigDir) == "" {
		return nil
	}
	proxyConfigPath := filepath.Join(proxyConfigDir, ".claude.json")
	if sameClaudeConfigFile(userConfigPath, proxyConfigPath) {
		return nil
	}
	userProjects, err := readClaudeAcceptedProjectFlags(userConfigPath)
	if err != nil {
		return err
	}
	proxyProjects, err := readClaudeAcceptedProjectFlags(proxyConfigPath)
	if err != nil {
		return err
	}
	return errors.Join(
		mergeClaudeAcceptedProjectFlags(proxyConfigPath, userProjects, true),
		mergeClaudeAcceptedProjectFlags(userConfigPath, proxyProjects, false),
	)
}

func sameClaudeConfigFile(a, b string) bool {
	left, errLeft := os.Stat(a)
	right, errRight := os.Stat(b)
	if errLeft == nil && errRight == nil {
		return os.SameFile(left, right)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// readClaudeAcceptedProjectFlags returns, per project path, the shared flags
// that are true. A missing file has no accepted flags.
func readClaudeAcceptedProjectFlags(path string) (map[string][]string, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var config struct {
		Projects map[string]map[string]json.RawMessage `json:"projects"`
	}
	if err := json.Unmarshal(body, &config); err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for project, fields := range config.Projects {
		for _, flag := range claudeSharedProjectFlags {
			if bytes.Equal(bytes.TrimSpace(fields[flag]), []byte("true")) {
				out[project] = append(out[project], flag)
			}
		}
	}
	return out, nil
}

// mergeClaudeAcceptedProjectFlags sets each accepted flag in the config at
// path, preserving every other field. createFile allows creating a missing
// config; the user's own config is never created by sr.
func mergeClaudeAcceptedProjectFlags(path string, accepted map[string][]string, createFile bool) error {
	if len(accepted) == 0 {
		return nil
	}
	if !waitForClaudeConfigLock(path) {
		return nil
	}
	info, statErr := os.Stat(path)
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if !createFile {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		body = []byte("{}")
	} else if err != nil {
		return err
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(body, &config); err != nil {
		return err
	}
	if config == nil {
		config = map[string]json.RawMessage{}
	}
	projects := map[string]map[string]json.RawMessage{}
	if raw, ok := config["projects"]; ok {
		if err := json.Unmarshal(raw, &projects); err != nil {
			return err
		}
	}
	changed := false
	for project, flags := range accepted {
		fields := projects[project]
		if fields == nil {
			// Claude fills the rest of a project entry with its defaults.
			fields = map[string]json.RawMessage{}
		}
		for _, flag := range flags {
			if !bytes.Equal(bytes.TrimSpace(fields[flag]), []byte("true")) {
				fields[flag] = json.RawMessage("true")
				changed = true
			}
		}
		projects[project] = fields
	}
	if !changed {
		return nil
	}
	encodedProjects, err := json.Marshal(projects)
	if err != nil {
		return err
	}
	config["projects"] = encodedProjects
	out, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if statErr == nil {
		mode = info.Mode().Perm()
	}
	return fsutil.WriteFileAtomic(path, append(out, '\n'), mode)
}

// waitForClaudeConfigLock waits briefly while Claude holds its config lock
// (a <file>.lock entry). It reports false when the lock stays held, so sr
// skips the write rather than race Claude.
func waitForClaudeConfigLock(path string) bool {
	deadline := time.Now().Add(claudeConfigLockWait)
	for {
		info, err := os.Stat(path + ".lock")
		if err != nil || time.Since(info.ModTime()) > time.Minute {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}
