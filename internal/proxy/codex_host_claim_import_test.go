package proxy

import (
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// An uploaded account starts a chain this server owns, so the uploader's host
// claim must not come along; otherwise the server would refuse its own refresh.
func TestAccountImportDropsUploadedHostClaim(t *testing.T) {
	account, err := validateStoredAccountImport(accounts.ProviderCodex, accounts.StoredCodexAccount{
		Email:     "apikey:team",
		HostClaim: &accounts.CodexHostClaim{Host: "laptop"},
		Auth:      accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if account.HostClaim != nil {
		t.Fatalf("imported account kept the uploader's claim: %+v", account.HostClaim)
	}
}
