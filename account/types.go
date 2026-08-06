package account

import "time"

type Provider string

const (
	ProviderCodex       Provider = "codex"
	ProviderClaude      Provider = "claude"
	ProviderKimi        Provider = "kimi"
	ProviderZAI         Provider = "zai"
	ProviderAzureOpenAI Provider = "azure-openai"
)

type AuthMode string

const (
	AuthModeOAuth    AuthMode = "oauth"
	AuthModeAPIKey   AuthMode = "apikey"
	AuthModeAzureCLI AuthMode = "azure-cli"
)

type Account struct {
	ID        string
	Provider  Provider
	AuthMode  AuthMode
	Label     string
	Email     string
	AddedAt   time.Time
	Token     string
	AccountID string
	Source    string
}

func (a Account) AuthorizationHeader() string {
	if a.Token == "" {
		return ""
	}
	return "Bearer " + a.Token
}
