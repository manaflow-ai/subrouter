package claude

import (
	"path/filepath"
	"testing"
)

// isolateClaudeConfigEnv gives a test its own home and Subrouter state root
// and clears every variable that selects a Claude config directory.
func isolateClaudeConfigEnv(t *testing.T) (home, stateDir string) {
	t.Helper()
	home = t.TempDir()
	stateDir = filepath.Join(home, ".subrouter")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SUBROUTER_STATE_DIR", stateDir)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_CODE_REMOTE_MEMORY_DIR", "")
	return home, stateDir
}

func TestDefaultStoreSharesTheCallersClaudeConfigDir(t *testing.T) {
	home, _ := isolateClaudeConfigEnv(t)
	if got, want := DefaultStore().SharedStateDir, filepath.Join(home, ".claude"); got != want {
		t.Fatalf("default shared dir = %q, want %q", got, want)
	}

	custom := filepath.Join(home, "work-claude")
	t.Setenv("CLAUDE_CONFIG_DIR", custom)
	for name, store := range map[string]Store{
		"interactive": DefaultStore(),
		"read-only":   DefaultStoreForReadOnlyInspection(),
	} {
		if store.SharedStateDir != custom {
			t.Fatalf("%s store shared dir = %q, want caller CLAUDE_CONFIG_DIR %q", name, store.SharedStateDir, custom)
		}
		if got, want := store.Dir, filepath.Join(home, ".subrouter", "codex"); got != want {
			t.Fatalf("%s store account dir = %q, want %q (credentials must not move)", name, got, want)
		}
	}
}

func TestDefaultStoreIgnoresSubrouterManagedClaudeConfigDir(t *testing.T) {
	home, stateDir := isolateClaudeConfigEnv(t)
	proxyDir := filepath.Join(stateDir, "codex", "claude-proxy", "0123456789ab")
	legacyProfile := filepath.Join(home, ".codex-accounts", "claude", "_p1")
	custom := filepath.Join(home, "work-claude")

	for _, managed := range []string{proxyDir, legacyProfile} {
		// A shell inside a pooled or profile launch inherits the managed
		// directory. It is never the user's own config.
		t.Setenv("CLAUDE_CONFIG_DIR", managed)
		t.Setenv("CLAUDE_CODE_REMOTE_MEMORY_DIR", "")
		if got, want := DefaultStore().SharedStateDir, filepath.Join(home, ".claude"); got != want {
			t.Fatalf("CLAUDE_CONFIG_DIR=%s: shared dir = %q, want %q", managed, got, want)
		}
		// A pooled launch also exports the shared root it attached to, so a
		// nested sr keeps the caller's custom directory.
		t.Setenv("CLAUDE_CODE_REMOTE_MEMORY_DIR", custom)
		if got := DefaultStore().SharedStateDir; got != custom {
			t.Fatalf("CLAUDE_CONFIG_DIR=%s: shared dir = %q, want inherited %q", managed, got, custom)
		}
		t.Setenv("CLAUDE_CODE_REMOTE_MEMORY_DIR", filepath.Join(stateDir, "codex", "claude-proxy", "other"))
		if got, want := DefaultStore().SharedStateDir, filepath.Join(home, ".claude"); got != want {
			t.Fatalf("managed memory dir accepted as shared dir: %q, want %q", got, want)
		}
	}
}
