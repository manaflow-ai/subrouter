package proxy

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// Codex and Claude are not registry providers, and the registry lookup sits
// below their explicit branches on every path it touches. These assertions pin
// that: a registry entry must never be able to capture a Codex or Claude
// request, whatever gets added to the table later.
func TestRegistryDoesNotCaptureCodexOrClaudeRouting(t *testing.T) {
	server := Server{
		CodexUpstream:  mustURL(t, "https://chatgpt.test/backend-api"),
		APIUpstream:    mustURL(t, "https://api.openai.test"),
		ClaudeUpstream: mustURL(t, "https://api.anthropic.test"),
		KimiUpstream:   mustURL(t, "https://kimi.test/coding/v1"),
		ZAIUpstream:    mustURL(t, "https://zai.test/api/coding/paas/v4"),
	}

	upstreams := []struct {
		name    string
		account accounts.Account
		path    string
		want    *url.URL
	}{
		{name: "claude oauth", account: accounts.Account{Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}, path: "/v1/messages", want: server.ClaudeUpstream},
		{name: "claude api key", account: accounts.Account{Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey}, path: "/v1/messages", want: server.ClaudeUpstream},
		{name: "codex api key", account: accounts.Account{Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeAPIKey}, path: "/v1/responses", want: server.APIUpstream},
		{name: "codex oauth", account: accounts.Account{Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth}, path: "/v1/responses", want: server.CodexUpstream},
		{name: "provider-less oauth", account: accounts.Account{AuthMode: accounts.AuthModeOAuth}, path: "/v1/responses", want: server.CodexUpstream},
	}
	for _, tc := range upstreams {
		t.Run("upstream/"+tc.name, func(t *testing.T) {
			if got := server.upstreamForRequest(tc.path, tc.account); got != tc.want {
				t.Fatalf("upstreamForRequest = %v, want %v", got, tc.want)
			}
		})
	}

	// Claude paths pass through untouched; no prefix is stripped and no version
	// segment is collapsed.
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens", "/"} {
		if got := server.pathForUpstream(path, accounts.Account{Provider: accounts.ProviderClaude}); got != path {
			t.Fatalf("pathForUpstream(%q) for claude = %q, want it unchanged", path, got)
		}
	}

	// Session agent type and plan label fall through for the non-registry
	// providers rather than being replaced by a provider name.
	for _, provider := range []accounts.Provider{accounts.ProviderCodex, accounts.ProviderClaude, ""} {
		if got := agentTypeForProviderSession("claude", provider); got != "claude" {
			t.Fatalf("agentTypeForProviderSession(%q) = %q, want the caller's agent type", provider, got)
		}
		if got := apiKeyPlanType(provider); got != "api key" {
			t.Fatalf("apiKeyPlanType(%q) = %q, want the generic label", provider, got)
		}
	}

	// A provider-less legacy account still backs Codex and Claude selection, and
	// is still withheld from registry providers.
	legacy := []accounts.Account{{ID: "legacy", AuthMode: accounts.AuthModeOAuth}}
	for _, provider := range []accounts.Provider{accounts.ProviderCodex, accounts.ProviderClaude} {
		if got := filterAccountsForProvider(legacy, provider); len(got) != 1 {
			t.Fatalf("filterAccountsForProvider(%q) dropped the legacy account", provider)
		}
	}
	for _, entry := range keyedProviders() {
		if got := filterAccountsForProvider(legacy, entry.Provider); len(got) != 0 {
			t.Fatalf("registry provider %q must not inherit provider-less legacy accounts", entry.Provider)
		}
	}

	// No registry entry may claim a path prefix that a non-registry provider
	// already routes on.
	for _, reserved := range []string{"v1", "backend-api", "messages", "responses"} {
		if _, ok := keyedProviderForPathPrefix(reserved); ok {
			t.Fatalf("a registry entry claims the reserved path segment %q", reserved)
		}
	}
	for _, reserved := range []string{"codex", "openai", "openai-codex", "claude", "anthropic"} {
		if _, ok := keyedProviderForName(reserved); ok {
			t.Fatalf("a registry entry claims the reserved provider name %q", reserved)
		}
	}
}

// The auth headers for Codex and Claude must be exactly what they were before
// the registry existed, including the Claude API-key and OAuth split.
func TestRegistryDoesNotChangeCodexOrClaudeAuthHeaders(t *testing.T) {
	claudeOAuth := http.Header{}
	claudeOAuth.Set("X-Api-Key", "client-supplied")
	setAccountAuthHeaders(claudeOAuth, accounts.Account{Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "oauth-token"}, "")
	if claudeOAuth.Get("Authorization") != "Bearer oauth-token" {
		t.Fatalf("claude oauth Authorization = %q", claudeOAuth.Get("Authorization"))
	}
	if claudeOAuth.Get("X-Api-Key") != "" {
		t.Fatal("claude oauth must strip a client X-Api-Key")
	}
	if claudeOAuth.Get("Anthropic-Beta") != claudeOAuthBetaHeader {
		t.Fatalf("claude oauth Anthropic-Beta = %q, want %q", claudeOAuth.Get("Anthropic-Beta"), claudeOAuthBetaHeader)
	}

	claudeKey := http.Header{}
	claudeKey.Set("Anthropic-Beta", claudeOAuthBetaHeader)
	setAccountAuthHeaders(claudeKey, accounts.Account{Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey, Token: "sk-ant"}, "")
	if claudeKey.Get("Authorization") != "" {
		t.Fatal("claude api-key auth must not send Authorization")
	}
	if claudeKey.Get("X-Api-Key") != "sk-ant" {
		t.Fatalf("claude api-key X-Api-Key = %q", claudeKey.Get("X-Api-Key"))
	}
	if claudeKey.Get("Anthropic-Beta") == claudeOAuthBetaHeader {
		t.Fatal("claude api-key auth must drop the OAuth beta header")
	}

	codex := http.Header{}
	setAccountAuthHeaders(codex, accounts.Account{Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "codex-token", AccountID: "acct-1"}, "")
	if codex.Get("Authorization") != "Bearer codex-token" {
		t.Fatalf("codex Authorization = %q", codex.Get("Authorization"))
	}
	if codex.Get("ChatGPT-Account-ID") != "acct-1" {
		t.Fatalf("codex ChatGPT-Account-ID = %q, want the account id", codex.Get("ChatGPT-Account-ID"))
	}

	// A provider-less account is still treated as Codex.
	legacy := http.Header{}
	setAccountAuthHeaders(legacy, accounts.Account{AuthMode: accounts.AuthModeOAuth, Token: "legacy-token", AccountID: "acct-2"}, "")
	if legacy.Get("ChatGPT-Account-ID") != "acct-2" {
		t.Fatalf("provider-less ChatGPT-Account-ID = %q, want the account id", legacy.Get("ChatGPT-Account-ID"))
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
