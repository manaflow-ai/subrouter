package accounts

import "github.com/manaflow-ai/subrouter/account"

// The account value types moved to the importable package
// github.com/manaflow-ai/subrouter/account so that consumers outside this
// module (notably the hosted gateway) can use the scheduler, which is typed in
// terms of them. Go's internal/ rule made that impossible while they lived here.
//
// These aliases exist so the move stayed mechanical: every existing reference
// to accounts.Account or accounts.ProviderCodex keeps compiling and keeps
// meaning the identical type, since a type alias is the same type rather than a
// new one. New code should import the account package directly.

type (
	Provider = account.Provider
	AuthMode = account.AuthMode
	Account  = account.Account
)

const (
	ProviderCodex       = account.ProviderCodex
	ProviderClaude      = account.ProviderClaude
	ProviderKimi        = account.ProviderKimi
	ProviderZAI         = account.ProviderZAI
	ProviderAntigravity = account.ProviderAntigravity

	ProviderOpenRouter  = account.ProviderOpenRouter
	ProviderDeepSeek    = account.ProviderDeepSeek
	ProviderTogether    = account.ProviderTogether
	ProviderFireworks   = account.ProviderFireworks
	ProviderOpenCodeZen = account.ProviderOpenCodeZen

	ProviderGrok = account.ProviderGrok

	ProviderQwen = account.ProviderQwen

	ProviderQwenToken = account.ProviderQwenToken

	ProviderQwenAnthropic = account.ProviderQwenAnthropic

	AuthModeOAuth  = account.AuthModeOAuth
	AuthModeAPIKey = account.AuthModeAPIKey
)
