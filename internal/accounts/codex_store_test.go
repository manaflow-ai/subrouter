package accounts

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func TestCodexStoreMigrationBatchVisibilityIsAtomicForConcurrentReaders(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}
	const accountCount = 64
	staged := make([]StoredCodexAccount, 0, accountCount)
	for i := 0; i < accountCount; i++ {
		id := fmt.Sprintf("migration-%03d@example.com", i)
		staged = append(staged, StoredCodexAccount{
			Email:    id,
			Provider: ProviderCodex,
			Auth: CodexAuthFile{
				AuthMode:     "apikey",
				OpenAIAPIKey: "sk-test",
			},
		})
	}
	const batchID = "atomic-visibility"
	if err := store.StageMigrationBatch(batchID, staged); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	errors := make(chan error, 1)
	go func() {
		defer close(done)
		marker := store.migrationBatchMarker(batchID)
		body := []byte(`{"accountIds":[]}` + "\n")
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := writeFileAtomic(marker, body, 0o600); err != nil {
				errors <- err
				return
			}
			if err := os.Remove(marker); err != nil {
				errors <- err
				return
			}
		}
	}()
	defer func() {
		close(stop)
		<-done
	}()

	for i := 0; i < 500; i++ {
		select {
		case err := <-errors:
			t.Fatal(err)
		default:
		}
		accounts, err := store.ListStored()
		if err != nil {
			t.Fatal(err)
		}
		if len(accounts) != 0 && len(accounts) != accountCount {
			t.Fatalf("concurrent batch list exposed %d of %d accounts", len(accounts), accountCount)
		}
	}
}

func TestCodexStoreRollbackOwnsAnOrdinaryReplacementOfAnActiveBatchAccount(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}
	staged := StoredCodexAccount{
		Email:    "migrated@example.com",
		Provider: ProviderCodex,
		Auth: CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-migrated",
		},
	}
	const batchID = "replacement-ownership"
	if err := store.StageMigrationBatch(batchID, []StoredCodexAccount{staged}); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateMigrationBatch(batchID, []string{staged.Email}); err != nil {
		t.Fatal(err)
	}
	replacement := staged
	replacement.MigrationBatchID = ""
	replacement.Auth.OpenAIAPIKey = "sk-repaired"
	if err := store.SaveStored(replacement); err != nil {
		t.Fatal(err)
	}
	if err := store.RollbackMigrationBatch(batchID); err != nil {
		t.Fatal(err)
	}
	accounts, err := store.ListStored()
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 0 {
		t.Fatalf("rollback left ordinary replacement active: %+v", accounts)
	}
}

func TestCodexStoreRollbackIsAtomicForConcurrentReaders(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}
	const accountCount = 24
	const batchID = "rollback-visibility"
	staged := make([]StoredCodexAccount, 0, accountCount)
	ids := make([]string, 0, accountCount)
	for i := 0; i < accountCount; i++ {
		id := fmt.Sprintf("rollback-%03d@example.com", i)
		ids = append(ids, id)
		staged = append(staged, StoredCodexAccount{
			Email:    id,
			Provider: ProviderCodex,
			Auth: CodexAuthFile{
				AuthMode:     "apikey",
				OpenAIAPIKey: "sk-test",
			},
		})
	}
	if err := store.StageMigrationBatch(batchID, staged); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	errors := make(chan error, 1)
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := store.ActivateMigrationBatch(batchID, ids); err != nil {
				errors <- err
				return
			}
			if err := store.RollbackMigrationBatch(batchID); err != nil {
				errors <- err
				return
			}
			if err := store.StageMigrationBatch(batchID, staged); err != nil {
				errors <- err
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	defer func() {
		close(stop)
		<-done
	}()

	for i := 0; i < 200; i++ {
		select {
		case err := <-errors:
			t.Fatal(err)
		default:
		}
		accounts, err := store.ListStored()
		if err != nil {
			t.Fatal(err)
		}
		if len(accounts) != 0 && len(accounts) != accountCount {
			t.Fatalf("concurrent rollback list exposed %d of %d accounts", len(accounts), accountCount)
		}
	}
}

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

func TestCodexStoreCaseVariantUpdatesOneCanonicalAccount(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}
	first := StoredCodexAccount{
		Email:    "Founders@Example.com",
		Provider: ProviderCodex,
		Auth: CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-first",
		},
	}
	updated := first
	updated.Email = "founders@example.com"
	updated.Auth.OpenAIAPIKey = "sk-updated"

	if err := store.SaveStored(first); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStored(updated); err != nil {
		t.Fatal(err)
	}

	stored, ok, err := store.FindStored("FOUNDERS@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || stored.Email != first.Email || stored.Auth.OpenAIAPIKey != "sk-updated" {
		t.Fatalf("case-variant update = found:%v account:%+v", ok, stored)
	}
	accounts, err := store.ListStored()
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("case-variant update created %d account files, want 1", len(accounts))
	}
	removed, ok, err := store.RemoveStored("founders@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || removed.Email != first.Email {
		t.Fatalf("removed = found:%v account:%+v", ok, removed)
	}
}
