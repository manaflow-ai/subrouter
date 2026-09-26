package proxy

import (
	"path/filepath"
	"testing"

	"github.com/manaflow-ai/subrouter/account"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestSchedulerSessionCountsScopesProvidersAndKeepsPiInCodex(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		agent, sessionID, accountID string
	}{
		{"codex", "codex-1", "same@example.com"},
		{"pi", "pi-1", "same@example.com"},
		{"claude", "claude-1", "same@example.com"},
		{"qwen-anthropic", "qwen-1", "team"},
	} {
		if _, err := store.Put(item.agent, item.sessionID, item.accountID, ""); err != nil {
			t.Fatal(err)
		}
	}
	counts := SchedulerSessionCounts(store)
	if got := counts[selectacct.ScoreKey(account.ProviderCodex, "same@example.com")]; got != 2 {
		t.Fatalf("Codex sessions = %d, want Codex plus Pi = 2", got)
	}
	if got := counts[selectacct.ScoreKey(account.ProviderClaude, "same@example.com")]; got != 1 {
		t.Fatalf("Claude sessions = %d, want 1", got)
	}
	if got := counts[selectacct.ScoreKey(account.ProviderQwenToken, "team")]; got != 1 {
		t.Fatalf("Qwen Token sessions = %d, want shared Anthropic endpoint = 1", got)
	}
}
