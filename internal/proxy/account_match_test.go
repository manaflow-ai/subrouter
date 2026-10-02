package proxy

import (
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestAccountMatchesDoesNotUseNonUniqueOAuthDisplayEmail(t *testing.T) {
	account := accounts.Account{
		ID: "stable-routing-id", Label: "Production Codex", Email: "owner@example.com",
	}
	if !accountMatches(account, account.ID) || !accountMatches(account, account.Label) {
		t.Fatal("stable routing selectors did not match")
	}
	if accountMatches(account, account.Email) {
		t.Fatal("non-unique OAuth display identity was accepted as a routing selector")
	}
}

// An owner-keyed Codex record is labeled with its bare login email, which is
// also the ID of a legacy record for that email. Selecting or replacing the
// legacy record by ID must reach the legacy record, not the owner-keyed one
// that happens to come first.
func TestAccountLookupPrefersIDOverOwnerKeyedEmailLabel(t *testing.T) {
	owner := accounts.Account{
		ID: "codex-owner-9213a25e", Provider: accounts.ProviderCodex, Label: "lawrence@example.com",
		Email: "lawrence@example.com", AuthMode: accounts.AuthModeOAuth, Token: "owner-token",
	}
	legacy := accounts.Account{
		ID: "lawrence@example.com", Provider: accounts.ProviderCodex, Label: "lawrence@example.com",
		Email: "lawrence@example.com", AuthMode: accounts.AuthModeOAuth, Token: "legacy-token",
	}

	found, ok := findAccount([]accounts.Account{owner, legacy}, "lawrence@example.com")
	if !ok || found.ID != legacy.ID {
		t.Fatalf("findAccount(email) = %q ok=%v, want legacy record %q", found.ID, ok, legacy.ID)
	}
	if found, ok := findAccount([]accounts.Account{owner, legacy}, owner.ID); !ok || found.ID != owner.ID {
		t.Fatalf("findAccount(owner id) = %q ok=%v", found.ID, ok)
	}
	if found, ok := findAccount([]accounts.Account{owner}, "lawrence@example.com"); !ok || found.ID != owner.ID {
		t.Fatalf("label selector without an ID match = %q ok=%v, want owner-keyed record", found.ID, ok)
	}

	ref := &AccountRef{accounts: []accounts.Account{owner, legacy}}
	refreshedLegacy := legacy
	refreshedLegacy.Token = "legacy-token-2"
	ref.replace(refreshedLegacy)
	if len(ref.accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(ref.accounts))
	}
	if got := ref.accounts[0]; got.ID != owner.ID || got.Token != owner.Token {
		t.Fatalf("owner-keyed record overwritten: %+v", got)
	}
	if got := ref.accounts[1]; got.ID != legacy.ID || got.Token != "legacy-token-2" {
		t.Fatalf("legacy record not replaced: %+v", got)
	}
}
