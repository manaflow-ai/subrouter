package proxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

func TestDeadClaudeRefreshChainSurvivesDaemonRestart(t *testing.T) {
	dir := t.TempDir()
	account := accounts.Account{
		ID: "account-a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth,
		Token: "sensitive-access-token", CredentialVersion: "sensitive-refresh-version",
	}
	first := &AccountRef{claudeStore: agentclaude.Store{Dir: dir}}
	first.noteCredResult(account, errors.New("Claude OAuth refresh failed: invalid_grant"))
	marker := first.claudeTerminalRefreshMarkerPath(account)
	info, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("durable rejection was not written: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("marker permissions = %v, want 0600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "invalid_grant\n" ||
		strings.Contains(string(raw), account.Token) || strings.Contains(string(raw), account.CredentialVersion) {
		t.Fatalf("unsafe persisted error marker %q", raw)
	}

	// The restarted controller has an empty in-memory failure cache but
	// must make zero provider refresh calls for the unchanged rejected grant.
	restarted := &AccountRef{claudeStore: agentclaude.Store{Dir: dir}}
	if _, blocked := restarted.terminalCredFailure(account); !blocked {
		t.Fatal("rejected refresh grant became eligible after a daemon restart")
	}
	if _, err := restarted.Refresh(context.Background(), account); err == nil ||
		!strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("refresh did not consult persisted rejection: %v", err)
	}

	// A successful interactive re-login installs a new chain; its identity
	// must not inherit a marker from the previous credential generation.
	repaired := account
	repaired.Token = "repaired-access"
	repaired.CredentialVersion = "repaired-refresh"
	if _, blocked := restarted.terminalCredFailure(repaired); blocked {
		t.Fatal("fresh OAuth grant incorrectly inherits stale failure marker")
	}
	if marker == restarted.claudeTerminalRefreshMarkerPath(repaired) {
		t.Fatal("credential version did not change durable marker identity")
	}
	if got, err := filepath.Glob(filepath.Join(dir, "terminal-refresh", "*.status")); err != nil || len(got) != 1 {
		t.Fatalf("durable markers = %q err=%v, want only one rejected chain", got, err)
	}
}
