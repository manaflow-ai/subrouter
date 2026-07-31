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

func TestCodexStoreRejectsDistinctIdentifiersWithSameStorageKey(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}
	first := StoredCodexAccount{
		Email:    "a+b@example.com",
		Provider: ProviderCodex,
		Auth: CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-first",
		},
	}
	second := StoredCodexAccount{
		Email:    "a_b@example.com",
		Provider: ProviderCodex,
		Auth: CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-second",
		},
	}
	if emailToFilename(first.Email) != emailToFilename(second.Email) {
		t.Fatal("test identifiers no longer reproduce the legacy storage-key collision")
	}
	if err := store.SaveStored(first); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStored(second); err == nil {
		t.Fatal("distinct account identifier overwrote an existing storage key")
	}

	stored, ok, err := store.FindStored(first.Email)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || stored.Email != first.Email || stored.Auth.OpenAIAPIKey != "sk-first" {
		t.Fatalf("original account was not preserved: %+v", stored)
	}
	if _, ok, err := store.FindStored(second.Email); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("colliding identifier resolved to a different stored account")
	}
}
