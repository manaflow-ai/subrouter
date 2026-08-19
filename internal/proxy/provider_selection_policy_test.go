package proxy

import (
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// The policy table is the single place a provider's session-routing rules
// live. The zero value (an unlisted provider) must be the safe default:
// fully sticky, no gated moves.
func TestSelectionPolicyDefaults(t *testing.T) {
	codex := selectionPolicyFor(accounts.Account{Provider: accounts.ProviderCodex})
	if !codex.retentionFloorGated || !codex.constrainedMoveGated {
		t.Fatalf("codex policy = %+v, want both gates on", codex)
	}
	if empty := selectionPolicyFor(accounts.Account{}); empty != codex {
		t.Fatalf("empty provider must resolve to the codex policy, got %+v", empty)
	}
	claude := selectionPolicyFor(accounts.Account{Provider: accounts.ProviderClaude})
	if claude.retentionFloorGated || claude.constrainedMoveGated {
		t.Fatalf("claude policy = %+v, want fully sticky with ungated moves (fable fallback depends on it)", claude)
	}
	unknown := selectionPolicyFor(accounts.Account{Provider: accounts.Provider("future-provider")})
	if unknown.retentionFloorGated || unknown.constrainedMoveGated {
		t.Fatalf("unknown provider policy = %+v, want the safe zero value", unknown)
	}
}
