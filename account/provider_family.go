package account

// Family is the client-facing provider. Legacy Qwen provider values continue
// to identify credential backends in saved accounts and explicit routes.
func (p Provider) Family() Provider {
	switch p {
	case ProviderQwen, ProviderQwenOAuth, ProviderQwenToken, ProviderQwenAnthropic:
		return ProviderQwen
	default:
		return p
	}
}

// Backend distinguishes independently funded Qwen accounts without changing
// their persisted identifiers or OAuth credential chains.
func (p Provider) Backend() string {
	switch p {
	case ProviderQwen:
		return "coding-plan"
	case ProviderQwenOAuth:
		return "subscription"
	case ProviderQwenToken, ProviderQwenAnthropic:
		return "token-plan"
	default:
		return ""
	}
}
