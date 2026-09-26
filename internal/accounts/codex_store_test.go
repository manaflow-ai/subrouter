package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStoredAccountLockSerializesSameProcess(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}
	const identifier = "same-process@example.com"
	lock, err := store.lockStoredAccount(identifier)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- store.SaveStored(StoredCodexAccount{
			Email: identifier,
			Auth:  CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "replacement"},
		})
	}()
	select {
	case err := <-done:
		_ = lock.Close()
		t.Fatalf("same-process writer bypassed held account lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	account, found, err := store.findStoredExact(identifier)
	if err != nil || !found || account.Auth.OpenAIAPIKey != "replacement" {
		t.Fatalf("serialized same-process replacement = %+v, found %v, err %v", account, found, err)
	}
}

func TestStoredAccountLockSerializesAcrossProcesses(t *testing.T) {
	if os.Getenv("SUBROUTER_ACCOUNT_LOCK_HELPER") == "1" {
		store := CodexStore{Dir: os.Getenv("SUBROUTER_ACCOUNT_LOCK_ROOT")}
		if err := os.WriteFile(os.Getenv("SUBROUTER_ACCOUNT_LOCK_READY"), []byte("ready"), 0o600); err != nil {
			os.Exit(2)
		}
		if err := store.SaveStored(StoredCodexAccount{
			Email: "cross-process@example.com",
			Auth:  CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "replacement"},
		}); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}

	store := CodexStore{Dir: t.TempDir()}
	ready := filepath.Join(t.TempDir(), "ready")
	lock, err := store.lockStoredAccount("cross-process@example.com")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestStoredAccountLockSerializesAcrossProcesses$")
	cmd.Env = append(os.Environ(),
		"SUBROUTER_ACCOUNT_LOCK_HELPER=1",
		"SUBROUTER_ACCOUNT_LOCK_ROOT="+store.Dir,
		"SUBROUTER_ACCOUNT_LOCK_READY="+ready,
	)
	if err := cmd.Start(); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = lock.Close()
			_ = cmd.Process.Kill()
			t.Fatal("helper did not reach the cross-process account lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-waitCh:
		_ = lock.Close()
		t.Fatalf("helper bypassed held cross-process account lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-waitCh; err != nil {
		t.Fatal(err)
	}
	account, found, err := store.findStoredExact("cross-process@example.com")
	if err != nil || !found || account.Auth.OpenAIAPIKey != "replacement" {
		t.Fatalf("serialized cross-process replacement = %+v, found %v, err %v", account, found, err)
	}
}

func TestRemoveStoredExactDurableRestoresAfterPostRenameSyncFailure(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}
	wantAccount := StoredCodexAccount{Email: "apikey:work", Auth: CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "secret"}}
	if err := store.SaveStored(wantAccount); err != nil {
		t.Fatal(err)
	}
	wantAccount, _, _ = store.FindStored(wantAccount.Email)
	want := errors.New("post-rename directory sync failed")
	calls := 0
	_, removed, err := store.RemoveStoredExactDurable(wantAccount, func(string) error {
		calls++
		if calls == 1 {
			return want
		}
		return nil
	})
	if removed || !errors.Is(err, want) {
		t.Fatalf("durable removal = removed %v, err %v", removed, err)
	}
	got, found, readErr := store.FindStored(wantAccount.Email)
	if readErr != nil || !found || !reflect.DeepEqual(got, wantAccount) {
		t.Fatalf("restored exact account = found %v got %+v err %v", found, got, readErr)
	}
}

func TestReconcileStoredRemovalStagesWaitsForActiveRemovalRollback(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}
	wantAccount := StoredCodexAccount{
		Email: "apikey:work[primary]*",
		Auth:  CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "exact-secret"},
	}
	if err := store.SaveStored(wantAccount); err != nil {
		t.Fatal(err)
	}
	wantAccount, _, _ = store.FindStored(wantAccount.Email)
	wantSyncErr := errors.New("first directory sync failed")
	firstSyncReached := make(chan struct{})
	returnFirstSync := make(chan struct{})
	removeDone := make(chan struct {
		account StoredCodexAccount
		removed bool
		err     error
	}, 1)
	go func() {
		account, removed, err := store.RemoveStoredExactDurable(wantAccount, func(string) error {
			select {
			case <-firstSyncReached:
				return nil
			default:
				close(firstSyncReached)
				<-returnFirstSync
				return wantSyncErr
			}
		})
		removeDone <- struct {
			account StoredCodexAccount
			removed bool
			err     error
		}{account: account, removed: removed, err: err}
	}()
	<-firstSyncReached

	reconcileSawStage := make(chan struct{})
	reconcileDone := make(chan error, 1)
	go func() {
		reconcileDone <- store.reconcileStoredRemovalStages(func(string) error { return nil }, func(identifier string) {
			if identifier != wantAccount.Email {
				t.Errorf("reconciliation derived identity %q, want %q", identifier, wantAccount.Email)
			}
			close(reconcileSawStage)
		})
	}()
	<-reconcileSawStage
	select {
	case err := <-reconcileDone:
		t.Fatalf("reconciliation bypassed the active account lease: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	stagedPath := filepath.Join(store.Dir, emailToFilename(wantAccount.Email)) + storedRemovalStageSuffix
	if _, err := os.Stat(stagedPath); err != nil {
		t.Fatalf("active removal stage disappeared before sync result: %v", err)
	}

	close(returnFirstSync)
	removedResult := <-removeDone
	if removedResult.removed || !errors.Is(removedResult.err, wantSyncErr) || !reflect.DeepEqual(removedResult.account, wantAccount) {
		t.Fatalf("failed removal = account %+v removed %v err %v", removedResult.account, removedResult.removed, removedResult.err)
	}
	if err := <-reconcileDone; err != nil {
		t.Fatal(err)
	}
	got, found, err := store.FindStored(wantAccount.Email)
	if err != nil || !found || !reflect.DeepEqual(got, wantAccount) {
		t.Fatalf("restored exact credential = found %v got %+v err %v", found, got, err)
	}
}

func TestRemoveStoredExactDurableReportsRestoredWhenRestoreSyncFails(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}
	wantAccount := StoredCodexAccount{Email: "apikey:work", Auth: CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "secret"}}
	if err := store.SaveStored(wantAccount); err != nil {
		t.Fatal(err)
	}
	wantAccount, _, _ = store.FindStored(wantAccount.Email)
	wantStageSync := errors.New("stage directory sync failed")
	wantRestoreSync := errors.New("restore directory sync failed")
	calls := 0
	_, removed, err := store.RemoveStoredExactDurable(wantAccount, func(string) error {
		calls++
		if calls == 1 {
			return wantStageSync
		}
		return wantRestoreSync
	})
	if removed || !errors.Is(err, wantStageSync) || !errors.Is(err, wantRestoreSync) {
		t.Fatalf("restore-sync-failed removal = removed %v, err %v", removed, err)
	}
	got, found, readErr := store.FindStored(wantAccount.Email)
	if readErr != nil || !found || !reflect.DeepEqual(got, wantAccount) {
		t.Fatalf("live restored account = found %v got %+v err %v", found, got, readErr)
	}
}

func TestRemoveStoredExactDurableFinalSyncFailureRemainsReconcileable(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}
	wantAccount := StoredCodexAccount{Email: "apikey:work", Auth: CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "secret"}}
	if err := store.SaveStored(wantAccount); err != nil {
		t.Fatal(err)
	}
	wantAccount, _, _ = store.FindStored(wantAccount.Email)
	want := errors.New("final staged-unlink sync failed")
	calls := 0
	_, removed, err := store.RemoveStoredExactDurable(wantAccount, func(string) error {
		calls++
		if calls == 2 {
			return want
		}
		return nil
	})
	if !removed || !errors.Is(err, want) {
		t.Fatalf("final-sync-failed removal = removed %v, err %v", removed, err)
	}
	if _, found, readErr := store.FindStored(wantAccount.Email); readErr != nil || found {
		t.Fatalf("committed account remained live = found %v err %v", found, readErr)
	}
	// On a real crash the staged unlink may either persist or roll back. Model
	// the latter and prove startup reconciliation completes it idempotently.
	path := filepath.Join(store.Dir, emailToFilename(wantAccount.Email))
	body, marshalErr := json.MarshalIndent(wantAccount, "", "  ")
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if err := os.WriteFile(path+storedRemovalStageSuffix, append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileStoredRemovalStages(func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + storedRemovalStageSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged secret remained after restart reconciliation: %v", err)
	}
}

func TestReconcileStoredRemovalStagesPreservesReplacement(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}
	original := StoredCodexAccount{Email: "apikey:work", Auth: CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "old"}}
	if err := store.SaveStored(original); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir, emailToFilename(original.Email))
	if err := os.Rename(path, path+storedRemovalStageSuffix); err != nil {
		t.Fatal(err)
	}
	replacement := original
	replacement.Auth.OpenAIAPIKey = "replacement"
	if err := store.SaveStored(replacement); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileStoredRemovalStages(func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.FindStored(original.Email)
	if err != nil || !found || got.Auth.OpenAIAPIKey != "replacement" {
		t.Fatalf("replacement after reconcile = found %v got %+v err %v", found, got, err)
	}
}

func TestReconcileStoredRemovalStagesTreatsStoreMetacharactersLiterally(t *testing.T) {
	root := t.TempDir()
	store := CodexStore{Dir: filepath.Join(root, "state[one]*")}
	if err := os.MkdirAll(store.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	original := StoredCodexAccount{Email: "apikey:work", Auth: CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "old"}}
	if err := store.SaveStored(original); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir, emailToFilename(original.Email))
	staged := path + storedRemovalStageSuffix
	if err := os.Rename(path, staged); err != nil {
		t.Fatal(err)
	}
	replacement := original
	replacement.Auth.OpenAIAPIKey = "replacement"
	if err := store.SaveStored(replacement); err != nil {
		t.Fatal(err)
	}

	// A glob-expanded implementation could reach this sibling when the literal
	// store path contains metacharacters. Reconciliation is confined to Dir.
	siblingDir := filepath.Join(root, "stateo-sibling")
	if err := os.MkdirAll(siblingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	siblingStage := filepath.Join(siblingDir, "sibling.json"+storedRemovalStageSuffix)
	if err := os.WriteFile(siblingStage, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := store.ReconcileStoredRemovalStages(func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged deletion still present or unreadable: %v", err)
	}
	if _, err := os.Stat(siblingStage); err != nil {
		t.Fatalf("sibling tombstone was touched: %v", err)
	}
	got, found, err := store.FindStored(original.Email)
	if err != nil || !found || got.Auth.OpenAIAPIKey != "replacement" {
		t.Fatalf("replacement after reconcile = found %v got %+v err %v", found, got, err)
	}
}

func TestReconcileStoredRemovalStagesRejectsAmbiguousStageIdentity(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}
	stageName := emailToFilename("apikey:expected") + storedRemovalStageSuffix
	stagePath := filepath.Join(store.Dir, stageName)
	body, err := json.Marshal(StoredCodexAccount{
		Email: "apikey:different",
		Auth:  CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "preserve-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stagePath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileStoredRemovalStages(func(string) error { return nil }); err == nil {
		t.Fatal("reconciliation accepted a stage whose filename and identity disagree")
	}
	if got, err := os.ReadFile(stagePath); err != nil || !reflect.DeepEqual(got, body) {
		t.Fatalf("ambiguous stage changed: body %q err %v", got, err)
	}
}

func TestStoredOAuthAccountKeepsRoutingIDSeparateFromLoginIdentity(t *testing.T) {
	stored := StoredCodexAccount{
		Email: "stable-routing-id",
		Label: "Production Codex",
		Auth: CodexAuthFile{AuthMode: "chatgpt", Tokens: &CodexTokens{
			AccessToken: "access", RefreshToken: "refresh",
			IDToken: testJWT("owner@example.com", time.Now().Add(time.Hour)),
		}},
	}
	account, ok := stored.Account("test")
	if !ok {
		t.Fatal("stored OAuth account was not usable")
	}
	if account.ID != "stable-routing-id" || account.Email != "owner@example.com" || account.Label != "Production Codex" {
		t.Fatalf("account = %#v", account)
	}
}

func TestReplaceStoredOAuthWithIsolatedRejectsDifferentAccountID(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}
	const email = "owner@example.com"
	stored := StoredCodexAccount{
		Email:    email,
		Provider: ProviderCodex,
		Auth: CodexAuthFile{AuthMode: "chatgpt", Tokens: &CodexTokens{
			AccessToken: "old-access", RefreshToken: "old-refresh",
			IDToken: testJWT(email, time.Now().Add(time.Hour)), AccountID: "workspace-a",
		}},
	}
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	incoming := CodexAuthFile{AuthMode: "chatgpt", Tokens: &CodexTokens{
		AccessToken: "new-access", RefreshToken: "new-refresh",
		IDToken: testJWT(email, time.Now().Add(time.Hour)), AccountID: "workspace-b",
	}}
	if err := store.ReplaceStoredOAuthWithIsolated(context.Background(), email, incoming); err == nil {
		t.Fatal("different account ID was accepted for the same login email")
	}
	after, found, err := store.findStoredExact(email)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("stored account disappeared after rejected replacement")
	}
	if after.Auth.Tokens.AccessToken != "old-access" || after.Auth.Tokens.AccountID != "workspace-a" {
		t.Fatalf("rejected replacement mutated stored credential: %+v", after.Auth.Tokens)
	}
}

func TestReplaceStoredOAuthWithIsolatedRejectsMissingAccountID(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}
	const email = "owner@example.com"
	stored := StoredCodexAccount{
		Email: email, Provider: ProviderCodex,
		Auth: CodexAuthFile{AuthMode: "chatgpt", Tokens: &CodexTokens{
			AccessToken: "old-access", RefreshToken: "old-refresh",
			IDToken: testJWT(email, time.Now().Add(time.Hour)), AccountID: "workspace-a",
		}},
	}
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	incoming := CodexAuthFile{AuthMode: "chatgpt", Tokens: &CodexTokens{
		AccessToken: "new-access", RefreshToken: "new-refresh",
		IDToken: testJWT(email, time.Now().Add(time.Hour)),
	}}
	if err := store.ReplaceStoredOAuthWithIsolated(context.Background(), email, incoming); err == nil {
		t.Fatal("missing account ID was accepted for an account with a stored workspace identity")
	}
	after, found, err := store.findStoredExact(email)
	if err != nil || !found {
		t.Fatalf("stored account lookup: found=%v err=%v", found, err)
	}
	if after.Auth.Tokens.AccessToken != "old-access" || after.Auth.Tokens.AccountID != "workspace-a" {
		t.Fatalf("rejected replacement mutated stored credential: %+v", after.Auth.Tokens)
	}
}

func TestReplaceStoredOAuthWithIsolatedRejectsUnprovenLegacyOwner(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}
	const email = "owner@example.com"
	if err := store.SaveStored(StoredCodexAccount{
		Email: email, Provider: ProviderCodex,
		Auth: CodexAuthFile{AuthMode: "chatgpt", Tokens: &CodexTokens{
			AccessToken: "old-access", RefreshToken: "old-refresh",
			IDToken: testJWT(email, time.Now().Add(time.Hour)),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	incoming := CodexAuthFile{AuthMode: "chatgpt", Tokens: &CodexTokens{
		AccessToken: "new-access", RefreshToken: "new-refresh",
		IDToken: testJWT(email, time.Now().Add(time.Hour)), AccountID: "workspace-a",
	}}
	if err := store.ReplaceStoredOAuthWithIsolated(context.Background(), email, incoming); err == nil {
		t.Fatal("repair adopted an unproven legacy owner")
	}
	after, found, err := store.findStoredExact(email)
	if err != nil || !found {
		t.Fatalf("stored account lookup: found=%v err=%v", found, err)
	}
	if after.Auth.Tokens.AccessToken != "old-access" || after.Auth.Tokens.AccountID != "" {
		t.Fatalf("rejected replacement changed the record: %+v", after.Auth.Tokens)
	}
}

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

func TestCodexStoreRejectsIdentifierThatCannotFitDecoratedFilenames(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}
	account := StoredCodexAccount{
		Email: strings.Repeat("a", 216),
		Auth:  CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "test-key"},
	}
	if err := store.SaveStored(account); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("SaveStored error = %v, want identifier length rejection", err)
	}
}
