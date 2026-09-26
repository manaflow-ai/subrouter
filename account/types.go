package account

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

type Provider string

const (
	ProviderCodex  Provider = "codex"
	ProviderClaude Provider = "claude"
	ProviderKimi   Provider = "kimi"
	ProviderZAI    Provider = "zai"

	// ProviderOpenRouter routes to OpenRouter's OpenAI-compatible API with a
	// per-account API key.
	ProviderOpenRouter  Provider = "openrouter"
	ProviderDeepSeek    Provider = "deepseek"
	ProviderTogether    Provider = "together"
	ProviderFireworks   Provider = "fireworks"
	ProviderOpenCodeZen Provider = "opencode-zen"

	// ProviderGrok routes to xAI. An API-key account reaches the
	// OpenAI-compatible API; an OAuth account is a Grok subscription behind
	// the device-code grant and reaches the CLI's chat-proxy endpoint.
	ProviderGrok Provider = "grok"

	// ProviderQwen routes to Alibaba Cloud Model Studio's Coding Plan, whose
	// subscription is addressed with a plan-specific API key on a dedicated
	// OpenAI-compatible endpoint.
	ProviderQwen Provider = "qwen"

	// ProviderQwenToken routes to Model Studio's Token Plan, which is a
	// separate subscription from the Coding Plan with its own key and its own
	// endpoint.
	ProviderQwenToken Provider = "qwen-token"

	// ProviderQwenAnthropic routes to the Token Plan's Anthropic-protocol
	// endpoint, which serves the same subscription to Anthropic-shaped clients.
	ProviderQwenAnthropic Provider = "qwen-anthropic"

	// ProviderAntigravity is a Google Antigravity subscription reached through
	// the Antigravity CLI's OAuth credential. OAuth-only: there is no API-key
	// mode for it.
	ProviderAntigravity Provider = "antigravity"
)

type AuthMode string

const (
	AuthModeOAuth  AuthMode = "oauth"
	AuthModeAPIKey AuthMode = "apikey"
)

type Account struct {
	ID       string
	Provider Provider
	AuthMode AuthMode
	Label    string
	Email    string
	AddedAt  time.Time
	Token    string
	// CredentialVersion identifies the complete OAuth credential chain without
	// exposing either token. It is deliberately omitted from serialized account
	// views and changes when a repair replaces only the refresh token.
	CredentialVersion string `json:"-"`
	AccountID         string
	Source            string
}

func OAuthCredentialVersion(accessToken, refreshToken string) string {
	sum := sha256.Sum256([]byte(accessToken + "\x00" + refreshToken))
	return hex.EncodeToString(sum[:])
}

func (a Account) CredentialIdentity() string {
	if a.CredentialVersion != "" {
		return a.CredentialVersion
	}
	return a.Token
}

func (a Account) AuthorizationHeader() string {
	if a.Token == "" {
		return ""
	}
	return "Bearer " + a.Token
}
