package accounts

import (
	"fmt"
	"strings"
)

// CodexOAuthIdentifier names a login by email and selected ChatGPT workspace.
// Email is the historical store key (also used for non-email provider aliases).
// Keep it distinct from Account.Email, which is the actual login email.
func CodexOAuthIdentifier(auth CodexAuthFile) (string, error) {
	if auth.Tokens == nil {
		return "", fmt.Errorf("Codex OAuth tokens are required")
	}
	email, err := ExtractEmailFromJWT(auth.Tokens.IDToken)
	if err != nil || strings.TrimSpace(email) == "" {
		return "", fmt.Errorf("Codex OAuth email is required")
	}
	id := strings.ToLower(strings.TrimSpace(email))
	workspace := strings.TrimSpace(ExtractChatGPTAccountID(auth))
	for _, token := range []string{auth.Tokens.IDToken, auth.Tokens.AccessToken} {
		if selected := strings.TrimSpace(ExtractChatGPTAccountIDFromJWT(token)); selected != "" && selected != workspace {
			return "", fmt.Errorf("Codex OAuth tokens identify different ChatGPT workspaces")
		}
	}
	if workspace != "" {
		id += "#" + workspace
	}
	if err := validateStoredAccountIdentifier(id); err != nil {
		return "", err
	}
	return id, nil
}

// SameCodexOAuthIdentity includes the workspace because one login email can
// own both a personal subscription and one or more team subscriptions.
func SameCodexOAuthIdentity(a, b CodexAuthFile) bool {
	if a.Tokens == nil || b.Tokens == nil {
		return false
	}
	if _, err := CodexOAuthIdentifier(a); err != nil {
		return false
	}
	if _, err := CodexOAuthIdentifier(b); err != nil {
		return false
	}
	aEmail, aErr := ExtractEmailFromJWT(a.Tokens.IDToken)
	bEmail, bErr := ExtractEmailFromJWT(b.Tokens.IDToken)
	return aErr == nil && bErr == nil && strings.TrimSpace(aEmail) != "" &&
		strings.EqualFold(strings.TrimSpace(aEmail), strings.TrimSpace(bEmail)) &&
		strings.TrimSpace(ExtractChatGPTAccountID(a)) == strings.TrimSpace(ExtractChatGPTAccountID(b))
}

// CanReplaceCodexOAuthIdentity permits an explicit repair to establish a
// workspace for legacy records that did not save one. A known workspace can
// never change. Automatic sync and lookup use SameCodexOAuthIdentity instead.
func CanReplaceCodexOAuthIdentity(stored, incoming CodexAuthFile) bool {
	if stored.Tokens == nil || incoming.Tokens == nil {
		return false
	}
	if _, err := CodexOAuthIdentifier(incoming); err != nil {
		return false
	}
	email, err := ExtractEmailFromJWT(stored.Tokens.IDToken)
	nextEmail, nextErr := ExtractEmailFromJWT(incoming.Tokens.IDToken)
	workspace := ExtractChatGPTAccountID(stored)
	return err == nil && nextErr == nil && strings.TrimSpace(email) != "" &&
		strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(nextEmail)) &&
		(workspace == "" || workspace == ExtractChatGPTAccountID(incoming))
}

// ResolveCodexOAuthAccount preserves an existing key for this exact workspace,
// including legacy email keys, without adopting another workspace's record.
// New workspaces get deterministic keys, so concurrent adds address one lock.
func (s CodexStore) ResolveCodexOAuthAccount(auth CodexAuthFile) (StoredCodexAccount, bool, error) {
	id, err := CodexOAuthIdentifier(auth)
	if err != nil {
		return StoredCodexAccount{}, false, err
	}
	all, err := s.ListStored()
	if err != nil {
		return StoredCodexAccount{}, false, err
	}
	var match *StoredCodexAccount
	for i := range all {
		candidate := &all[i]
		if candidate.ProviderOrDefault() != ProviderCodex || candidate.IsAPIKey() || !SameCodexOAuthIdentity(candidate.Auth, auth) {
			continue
		}
		if match != nil {
			return StoredCodexAccount{}, false, fmt.Errorf("multiple stored accounts have the same Codex workspace: %q and %q", match.Email, candidate.Email)
		}
		match = candidate
	}
	if match != nil {
		return *match, true, nil
	}
	return StoredCodexAccount{Email: id, Auth: auth}, false, nil
}
