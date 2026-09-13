package accounts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// CodexOwner is one provider user in one selected ChatGPT workspace. Email is
// display data and never participates when the stable owner is available.
type CodexOwner struct {
	UserID      string `json:"userId"`
	WorkspaceID string `json:"workspaceId"`
}

func (o CodexOwner) Complete() bool { return o.UserID != "" && o.WorkspaceID != "" }

func ParseCodexOwner(auth CodexAuthFile) (CodexOwner, error) {
	if auth.Tokens == nil {
		return CodexOwner{}, fmt.Errorf("Codex OAuth tokens are required")
	}
	owner := CodexOwner{}
	if err := mergeOwnerClaim(&owner.WorkspaceID, auth.Tokens.AccountID); err != nil {
		return owner, err
	}
	for index, token := range []string{auth.Tokens.IDToken, auth.Tokens.AccessToken} {
		claims, err := DecodeJWTClaims(token)
		if err != nil {
			continue
		} // Legacy access tokens can be opaque.
		nested, _ := claims["https://api.openai.com/auth"].(map[string]any)
		for _, source := range []map[string]any{claims, nested} {
			if err := mergeOwnerValue(&owner.WorkspaceID, source["chatgpt_account_id"]); err != nil {
				return owner, err
			}
			if err := mergeOwnerValue(&owner.UserID, source["chatgpt_user_id"]); err != nil {
				return owner, err
			}
		}
		if index == 0 && owner.UserID == "" {
			if err := mergeOwnerValue(&owner.UserID, nested["user_id"]); err != nil {
				return owner, err
			}
		}
	}
	return owner, nil
}

func mergeOwnerValue(target *string, value any) error {
	if value == nil {
		return nil
	}
	claim, ok := value.(string)
	if !ok {
		return fmt.Errorf("Codex owner claim is invalid")
	}
	return mergeOwnerClaim(target, claim)
}

func mergeOwnerClaim(target *string, claim string) error {
	if claim == "" {
		return nil
	}
	if strings.TrimSpace(claim) != claim || len(claim) > 512 || strings.ContainsAny(claim, "\r\n\t\x00") {
		return fmt.Errorf("Codex owner claim is invalid")
	}
	if *target != "" && *target != claim {
		return fmt.Errorf("Codex credentials identify different owners")
	}
	*target = claim
	return nil
}

func (o CodexOwner) Key() string {
	encoded, _ := json.Marshal([]string{"codex", o.UserID, o.WorkspaceID})
	digest := sha256.Sum256(encoded)
	return "codex-owner-" + hex.EncodeToString(digest[:])
}

// CodexOAuthIdentifier returns an immutable key for new identified accounts.
// Legacy records lacking owner claims retain their old key until re-enrollment.
func CodexOAuthIdentifier(auth CodexAuthFile) (string, error) {
	owner, err := ParseCodexOwner(auth)
	if err != nil {
		return "", err
	}
	if owner.Complete() {
		return owner.Key(), nil
	}
	return legacyCodexOAuthIdentifier(auth)
}

func legacyCodexOAuthIdentifier(auth CodexAuthFile) (string, error) {
	if auth.Tokens == nil {
		return "", fmt.Errorf("Codex OAuth tokens are required")
	}
	email, err := ExtractEmailFromJWT(auth.Tokens.IDToken)
	if err != nil || strings.TrimSpace(email) == "" {
		return "", fmt.Errorf("Codex OAuth email is required")
	}
	id := strings.ToLower(strings.TrimSpace(email))
	if workspace := ExtractChatGPTAccountID(auth); workspace != "" {
		id += "#" + workspace
	}
	if err := validateStoredAccountIdentifier(id); err != nil {
		return "", err
	}
	return id, nil
}

// SameCodexOAuthIdentity never equates a known owner with a different or missing
// owner. Legacy-only equality keeps old records usable without adopting a login.
func SameCodexOAuthIdentity(a, b CodexAuthFile) bool {
	left, leftErr := ParseCodexOwner(a)
	right, rightErr := ParseCodexOwner(b)
	if leftErr != nil || rightErr != nil {
		return false
	}
	if left.Complete() && right.Complete() {
		return left == right
	}
	// A legacy copy of the exact refresh credential proves the same owner;
	// matching email or workspace alone does not.
	if left.UserID != "" && right.UserID != "" && left.UserID != right.UserID {
		return false
	}
	if left.WorkspaceID != "" && right.WorkspaceID != "" && left.WorkspaceID != right.WorkspaceID {
		return false
	}
	return a.Tokens.RefreshToken != "" && a.Tokens.RefreshToken == b.Tokens.RefreshToken
}

// A legacy repair may establish missing claims, but cannot change any claim
// already known. Once identified, email changes do not change ownership.
func CanReplaceCodexOAuthIdentity(stored, incoming CodexAuthFile) bool {
	return SameCodexOAuthIdentity(stored, incoming)
}

// Existing records keep their routing IDs, filenames and session references.
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
			return StoredCodexAccount{}, false, fmt.Errorf("multiple stored records have the same Codex owner: %q and %q", match.Email, candidate.Email)
		}
		match = candidate
	}
	if match != nil {
		return *match, true, nil
	}
	owner, _ := ParseCodexOwner(auth)
	if !owner.Complete() {
		return StoredCodexAccount{}, false, fmt.Errorf("Codex user and workspace identity are required; sign in again")
	}
	return StoredCodexAccount{Email: id, Auth: auth}, false, nil
}

// Accept either the stable key, the old email/workspace key, or login email on
// import. A caller cannot redirect credentials to another owner's record.
func CodexIdentifierMatchesAuth(identifier string, auth CodexAuthFile) bool {
	stable, err := CodexOAuthIdentifier(auth)
	if err != nil {
		return false
	}
	legacy, _ := legacyCodexOAuthIdentifier(auth)
	email, _ := ExtractEmailFromJWT(auth.Tokens.IDToken)
	return strings.EqualFold(identifier, stable) || strings.EqualFold(identifier, legacy) || strings.EqualFold(identifier, strings.TrimSpace(email))
}
