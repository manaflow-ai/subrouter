package accounts

import (
	"os"
	"testing"
)

func TestCodexStoreRejectsAccountIdentifierThatWouldCreateHiddenState(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}
	account := StoredCodexAccount{
		Email:    ".hidden@example.com",
		Provider: ProviderCodex,
		Auth: CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-test",
		},
	}

	if err := store.SaveStored(account); err == nil {
		t.Fatal("hidden account identifier was accepted")
	}
	entries, err := os.ReadDir(store.Dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("hidden account identifier created %d directory entries", len(entries))
	}
}
