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
