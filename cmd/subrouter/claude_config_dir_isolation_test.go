package main

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/agents/claude"
)

func isolateCallerClaudeConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(home, ".subrouter"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_CODE_REMOTE_MEMORY_DIR", "")
	t.Setenv(claudeProxyUserSettingsEnv, "")
	return home
}

func TestClaudeProxyConfigDirIsSeparatePerCallerConfigDir(t *testing.T) {
	home := isolateCallerClaudeConfig(t)
	storeDir := claude.DefaultStore().Dir

	// The default ~/.claude caller keeps its existing proxy directory, so an
	// upgrade does not orphan proxy state.
	sum := sha256.Sum256([]byte("team\x00account:acct"))
	legacy := filepath.Join(storeDir, "claude-proxy", fmt.Sprintf("%x", sum[:12]))
	if got := claudeProxyConfigDir(storeDir, "team", "acct"); got != legacy {
		t.Fatalf("default caller proxy dir = %q, want unchanged %q", got, legacy)
	}

	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "work-claude"))
	work := claudeProxyConfigDir(storeDir, "team", "acct")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "play-claude"))
	play := claudeProxyConfigDir(storeDir, "team", "acct")
	if work == legacy || play == legacy || work == play {
		t.Fatalf("callers with different config dirs share a proxy dir: default=%q work=%q play=%q", legacy, work, play)
	}
}

func TestClaudeProxyUsesCallerConfigDirForUserSettingsAndTrust(t *testing.T) {
	home := isolateCallerClaudeConfig(t)
	storeDir := claude.DefaultStore().Dir
	custom := filepath.Join(home, "work-claude")
	t.Setenv("CLAUDE_CONFIG_DIR", custom)
	if got, want := claudeProxyUserSettingsPath(storeDir), filepath.Join(custom, "settings.json"); got != want {
		t.Fatalf("merged user settings = %q, want %q", got, want)
	}
	if got, want := claudeUserConfigPath(), filepath.Join(custom, ".claude.json"); got != want {
		t.Fatalf("trust source = %q, want %q", got, want)
	}

	// Inside a pooled launch CLAUDE_CONFIG_DIR is the proxy directory. Trust
	// must still sync with the caller's config, not with the proxy itself.
	proxyDir := claudeProxyConfigDir(storeDir, "team", "")
	t.Setenv("CLAUDE_CONFIG_DIR", proxyDir)
	t.Setenv("CLAUDE_CODE_REMOTE_MEMORY_DIR", custom)
	if got, want := claudeUserConfigPath(), filepath.Join(custom, ".claude.json"); got != want {
		t.Fatalf("nested trust source = %q, want %q", got, want)
	}
	t.Setenv("CLAUDE_CODE_REMOTE_MEMORY_DIR", "")
	if got, want := claudeUserConfigPath(), filepath.Join(home, ".claude.json"); got != want {
		t.Fatalf("nested default trust source = %q, want %q", got, want)
	}
}
