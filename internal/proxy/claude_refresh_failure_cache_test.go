package proxy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestIrreparableClaudeRefreshFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		errorText string
		want      bool
	}{
		{`Claude OAuth refresh failed: 400 Bad Request: {"error":"invalid_grant"}`, true},
		{"OAuth refresh_token_reused", true},
		{"Claude OAuth refresh failed: 429 Too Many Requests", false},
		{"Claude OAuth refresh failed: 503 Service Unavailable", false},
		{"unauthorized", false},
		{"unreadable credential", false},
	} {
		got := irreparableClaudeRefreshFailure(errors.New(tc.errorText))
		if got != tc.want {
			t.Errorf("irreparableClaudeRefreshFailure(%q) = %t, want %t", tc.errorText, got, tc.want)
		}
	}
	if irreparableClaudeRefreshFailure(nil) {
		t.Fatal("nil must not mean an irreparable refresh failure")
	}
}

func TestDeadClaudeRefreshIsRememberedBeyondFormerTTL(t *testing.T) {
	ref := &AccountRef{}
	account := accounts.Account{
		ID:                "claude-personal",
		Provider:          accounts.ProviderClaude,
		AuthMode:          accounts.AuthModeOAuth,
		Token:             "expired-token",
		CredentialVersion: "single-use-grant-v1",
	}
	ref.noteCredResult(account, errors.New(`Claude OAuth refresh failed: 400: {"error":"invalid_grant"}`))
	key := credFailureKey(account)
	ref.credFailMu.Lock()
	failure := ref.credFail[key]
	failure.at = time.Now().Add(-credFailureTTL - time.Second)
	ref.credFail[key] = failure
	ref.credFailMu.Unlock()

	if _, blocked := ref.terminalCredFailure(account); !blocked {
		t.Fatal("rejected single-use grant became eligible for repeated refresh after TTL")
	}
	_, err := ref.Refresh(context.Background(), account)
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("cached refresh failure was not returned without a new upstream request: %v", err)
	}

	// A new OAuth grant changes the credential identity and can be tried
	// immediately, without waiting for a cooldown or explicit cache reset.
	repaired := account
	repaired.CredentialVersion = "fresh-grant-v2"
	repaired.Token = "new-token"
	if _, blocked := ref.terminalCredFailure(repaired); blocked {
		t.Fatal("freshly authenticated credential inherited previous failure")
	}
}

func TestOtherClaudeTerminalFailuresRetainBoundedTTL(t *testing.T) {
	ref := &AccountRef{}
	account := accounts.Account{ID: "claude-personal", Provider: accounts.ProviderClaude, Token: "token"}
	ref.noteCredResult(account, errors.New("unreadable credential"))
	ref.credFailMu.Lock()
	record := ref.credFail[credFailureKey(account)]
	if record.irreparable {
		t.Fatal("unreadable credential should use the existing bounded failure TTL")
	}
	record.at = time.Now().Add(-credFailureTTL - time.Second)
	ref.credFail[credFailureKey(account)] = record
	ref.credFailMu.Unlock()
	if _, blocked := ref.terminalCredFailure(account); blocked {
		t.Fatal("bounded credential failure never expired")
	}
}

func TestDeadClaudeRefreshCacheIsProviderScoped(t *testing.T) {
	ref := &AccountRef{}
	claude := accounts.Account{
		ID:                "same-id",
		Provider:          accounts.ProviderClaude,
		Token:             "same-token",
		CredentialVersion: "version-a",
	}
	ref.noteCredResult(claude, errors.New("invalid_grant"))
	codex := claude
	codex.Provider = accounts.ProviderCodex
	if _, blocked := ref.terminalCredFailure(codex); blocked {
		t.Fatal("Claude OAuth error blocked unrelated provider account")
	}
}

func TestIrreparableClaudeRefreshSurvivesLateSuccessfulStatus(t *testing.T) {
	ref := &AccountRef{}
	old := accounts.Account{
		ID:                "claude-personal",
		Provider:          accounts.ProviderClaude,
		AuthMode:          accounts.AuthModeOAuth,
		Token:             "old-access",
		CredentialVersion: "old-grant",
	}
	ref.noteCredResult(old, errors.New("invalid_grant"))
	// A status sweep begun before the rejection may finish afterward and
	// report nil even though it merely observed the old cached access token.
	ref.noteCredResult(old, nil)
	ref.noteCredResult(old, context.DeadlineExceeded)
	ref.noteCredResult(old, errors.New("unreadable credential"))
	if _, blocked := ref.terminalCredFailure(old); !blocked {
		t.Fatal("a late status result cleared an irrevocably rejected grant")
	}
	if _, err := ref.claudeRefreshCandidate(old); err == nil {
		t.Fatal("old grant was allowed to retry")
	}
}

func TestRepairedClaudeSnapshotBypassesOldGrantBlock(t *testing.T) {
	old := accounts.Account{
		ID:                "claude-personal",
		Provider:          accounts.ProviderClaude,
		AuthMode:          accounts.AuthModeOAuth,
		Token:             "old-access",
		CredentialVersion: "old-grant",
	}
	repaired := old
	repaired.Token = "new-access"
	repaired.CredentialVersion = "new-grant"
	ref := &AccountRef{accounts: []accounts.Account{repaired}}
	ref.noteCredResult(old, errors.New("invalid_grant"))
	selected, err := ref.claudeRefreshCandidate(old)
	if err != nil || selected.CredentialIdentity() != repaired.CredentialIdentity() {
		t.Fatalf("a repaired profile should supersede stale in-flight selection: selected=%+v err=%v", selected, err)
	}
	// The repaired credential is eligible without waiting or probing.
	if selected, err = ref.claudeRefreshCandidate(repaired); err != nil || selected.CredentialIdentity() != repaired.CredentialIdentity() {
		t.Fatalf("repaired account was rejected: selected=%+v err=%v", selected, err)
	}
}
