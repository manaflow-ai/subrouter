package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func TestPublishAccountDiskMutationDoesNotMutateWhenPublicationFails(t *testing.T) {
	called := false
	want := errors.New("generation unavailable")
	err := publishAccountDiskMutation(
		context.Background(),
		conventionalAccountStore(t.TempDir()),
		func(string) error { return want },
		func() (bool, error) {
			called = true
			return true, nil
		},
	)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want publication failure", err)
	}
	if called {
		t.Fatal("credential mutation ran after generation publication failed")
	}
}

func TestPublishAccountDiskMutationAcceptsCommittedClaudeRemoval(t *testing.T) {
	committedErr := fmt.Errorf("%w: injected directory sync failure", agentclaude.ErrProfileRegistryWriteCommitted)
	err := publishAccountDiskMutation(
		context.Background(), conventionalAccountStore(t.TempDir()), advanceAccountDiskGeneration,
		func() (bool, error) { return true, committedErr },
	)
	if err != nil {
		t.Fatalf("committed ordinary Claude removal = %v", err)
	}
	err = publishAccountDiskMutation(
		context.Background(), conventionalAccountStore(t.TempDir()), advanceAccountDiskGeneration,
		func() (bool, error) { return false, committedErr },
	)
	if !errors.Is(err, agentclaude.ErrProfileRegistryWriteCommitted) {
		t.Fatalf("uncommitted Claude removal error = %v", err)
	}
}

func TestPublishAccountDiskMutationPublishesBeforeMutation(t *testing.T) {
	published := false
	err := publishAccountDiskMutation(
		context.Background(),
		conventionalAccountStore(t.TempDir()),
		func(string) error {
			published = true
			return nil
		},
		func() (bool, error) {
			if !published {
				t.Fatal("credential mutation ran before generation publication")
			}
			return true, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestWithAccountDiskMutationPublicationSkipsNoOpGeneration(t *testing.T) {
	published := 0
	err := withAccountDiskMutationPublication(
		context.Background(),
		conventionalAccountStore(t.TempDir()),
		func(string) error {
			published++
			return nil
		},
		func(func() error) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if published != 0 {
		t.Fatalf("no-op refresh published %d account generations, want 0", published)
	}
}

func TestWithAccountDiskMutationPublicationRunsHookOnceBeforeMutation(t *testing.T) {
	published := 0
	mutated := false
	err := withAccountDiskMutationPublication(
		context.Background(),
		conventionalAccountStore(t.TempDir()),
		func(string) error {
			if mutated {
				t.Fatal("account generation published after credential mutation")
			}
			published++
			return nil
		},
		func(publish func() error) error {
			if err := publish(); err != nil {
				return err
			}
			if err := publish(); err != nil {
				return err
			}
			mutated = true
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if published != 1 || !mutated {
		t.Fatalf("publication/mutation = %d/%v, want 1/true", published, mutated)
	}
}

func TestRollbackUnpublishedAccountDiskMutationFailsClosedBeforeRemoval(t *testing.T) {
	removed := false
	published := 0
	storeDir := t.TempDir()
	err := rollbackUnpublishedAccountDiskMutation(
		context.Background(),
		storeDir,
		func(string) error {
			published++
			if err := os.WriteFile(accountDiskGenerationPath(storeDir), []byte{byte('0' + published)}, 0o600); err != nil {
				return err
			}
			active, err := accountRollbackActive(storeDir)
			if err != nil || !active {
				t.Fatalf("rollback publication %d did not retain fail-closed marker: active=%v err=%v", published, active, err)
			}
			if published == 2 && !removed {
				t.Fatal("removal was not durable before completion publication")
			}
			return nil
		},
		func() error {
			removed = true
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if published != 2 {
		t.Fatalf("publication count = %d, want fail-closed and completion generations", published)
	}
	if active, err := accountRollbackActive(storeDir); err != nil || active {
		t.Fatalf("rollback marker remained after success: active=%v err=%v", active, err)
	}
}

func TestRollbackUnpublishedAccountDiskMutationFailsClosedWhenPublicationFails(t *testing.T) {
	removed := false
	wantErr := errors.New("generation unavailable")
	storeDir := t.TempDir()
	err := rollbackUnpublishedAccountDiskMutation(
		context.Background(),
		storeDir,
		func(string) error { return wantErr },
		func() error {
			removed = true
			return nil
		},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want publication failure", err)
	}
	if !removed {
		t.Fatal("durable fail-closed marker did not permit secret removal after generation failure")
	}
	if active, activeErr := accountRollbackActive(storeDir); activeErr != nil || !active {
		t.Fatalf("publication failure did not retain fail-closed marker: active=%v err=%v", active, activeErr)
	}
}

func TestOpenAccountRefDoesNotLoadCredentialsWhileRollbackMarkerExists(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email: "apikey:must-not-load", Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-secret"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := setAccountRollbackActive(store.StoreDir()); err != nil {
		t.Fatal(err)
	}
	ref, err := OpenAccountRef(store, agentclaude.Store{Dir: t.TempDir()}, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if got := ref.All(); len(got) != 0 {
		t.Fatalf("new worker loaded credentials behind rollback marker: %+v", got)
	}
}

func TestOpenAccountRefRestoresPreRegistryClaudeRemovalStage(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
	if err := claudeStore.ImportProfileCredential("work", agentclaude.CredentialInfo{
		AccessToken: "claude-access", RefreshToken: "claude-refresh",
	}); err != nil {
		t.Fatal(err)
	}
	profile, found := claudeStore.FindProfile("work")
	if !found {
		t.Fatal("Claude profile missing before simulated crash")
	}
	instancePath := filepath.Join(claudeStore.InstancesDir(), profile.Dir)
	snapshot, found, err := claudeStore.SnapshotProfileRemovalContext(t.Context(), "work")
	if err != nil || !found {
		t.Fatalf("Claude snapshot = found %v, err %v", found, err)
	}
	stageRoot := filepath.Join(filepath.Dir(instancePath), "."+filepath.Base(instancePath)+".remove-12345")
	stageClaudeRemovalForTest(t, instancePath, stageRoot, snapshot.CredentialVersion)

	ref, err := OpenAccountRef(store, claudeStore, http.DefaultClient)
	if err != nil {
		t.Fatalf("OpenAccountRef restart reconciliation: %v", err)
	}
	got := ref.All()
	if len(got) != 1 || got[0].ID != "work" || got[0].Token != "claude-access" {
		t.Fatalf("restarted accounts = %+v, want restored Claude account", got)
	}
	if _, err := os.Stat(filepath.Join(instancePath, ".credentials.json")); err != nil {
		t.Fatalf("canonical Claude credential was not restored: %v", err)
	}
	if _, err := os.Lstat(stageRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reconciled stage remains: %v", err)
	}
}

func TestAccountRefEvictsCredentialSnapshotWhileRollbackMarkerExists(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	storeDir := store.StoreDir()
	credential := accounts.StoredCodexAccount{
		Email: "apikey:must-evict", Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-secret"},
	}
	if err := store.SaveStored(credential); err != nil {
		t.Fatal(err)
	}
	ref, err := OpenAccountRef(store, agentclaude.Store{Dir: t.TempDir()}, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if got := ref.All(); len(got) != 1 {
		t.Fatalf("initial accounts = %+v", got)
	}
	if err := setAccountRollbackActive(storeDir); err != nil {
		t.Fatal(err)
	}
	reloaded, _, err := ref.reloadIfDiskGenerationChanged(context.Background())
	if !errors.Is(err, errAccountRollbackIncomplete) {
		t.Fatalf("rollback reload error = %v, want incomplete rollback", err)
	}
	if !reloaded {
		t.Fatal("rollback marker did not evict the live worker snapshot")
	}
	if got := ref.All(); len(got) != 0 {
		t.Fatalf("worker retained credential during rollback: %+v", got)
	}
	// Even without a generation change, the durable marker remains a direct
	// fail-closed signal after a crash.
	if reloaded, _, err := ref.reloadIfDiskGenerationChanged(context.Background()); reloaded || !errors.Is(err, errAccountRollbackIncomplete) {
		t.Fatalf("already-evicted snapshot reload = %v, err=%v", reloaded, err)
	}
}

func TestCompletedAccountRollbackJournalSelfReconciles(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email: "apikey:unrelated", Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-secret"},
	}); err != nil {
		t.Fatal(err)
	}
	ref, err := OpenAccountRef(store, agentclaude.Store{Dir: t.TempDir()}, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if err := setAccountRollbackActive(store.StoreDir()); err != nil {
		t.Fatal(err)
	}
	if err := markAccountRollbackRemoved(store.StoreDir()); err != nil {
		t.Fatal(err)
	}
	reloaded, _, err := ref.reloadIfDiskGenerationChanged(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded {
		t.Fatal("completed rollback reconciliation did not reload account generation")
	}
	if active, err := accountRollbackActive(store.StoreDir()); err != nil || active {
		t.Fatalf("completed rollback journal remained active: active=%v err=%v", active, err)
	}
	if got := ref.All(); len(got) != 1 || got[0].ID != "apikey:unrelated" {
		t.Fatalf("reconciliation did not restore unaffected account snapshot: %+v", got)
	}
}

func TestPreparedClaudeRollbackJournalCompletesAfterRestart(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email: "apikey:unaffected", Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-unaffected"},
	}); err != nil {
		t.Fatal(err)
	}
	claudeStore := agentclaude.Store{Dir: root}
	if _, err := claudeStore.CreateProfile("survivor"); err != nil {
		t.Fatal(err)
	}
	if _, err := claudeStore.CreateProfile("unpublished"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := prepareClaudeProfileRollback(context.Background(), root, "unpublished"); err != nil || !found {
		t.Fatalf("prepare rollback = found %v, err %v", found, err)
	}

	// Opening a new worker models restart immediately after the prepared
	// journal became durable and before the initiating process removed anything.
	ref, err := OpenAccountRef(store, claudeStore, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := claudeStore.FindProfile("unpublished"); ok {
		t.Fatal("restart reconciliation retained the unpublished Claude profile")
	}
	if _, ok := claudeStore.FindProfile("survivor"); !ok {
		t.Fatal("restart reconciliation removed an unaffected Claude profile")
	}
	if active, err := accountRollbackActive(root); err != nil || active {
		t.Fatalf("completed restart rollback marker = active %v, err %v", active, err)
	}
	if got := ref.All(); len(got) != 1 || got[0].ID != "apikey:unaffected" {
		t.Fatalf("unaffected account snapshot after recovery = %+v", got)
	}
	if generation, err := readAccountDiskGeneration(root); err != nil || generation == "" {
		t.Fatalf("completion generation = %q, err %v", generation, err)
	}
}

func TestPreparedClaudeRollbackJournalAllowsLastUsedChange(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	claudeStore := agentclaude.Store{Dir: root}
	if _, err := claudeStore.CreateProfile("survivor"); err != nil {
		t.Fatal(err)
	}
	if _, err := claudeStore.CreateProfile("unpublished"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := prepareClaudeProfileRollback(context.Background(), root, "unpublished"); err != nil || !found {
		t.Fatalf("prepare rollback = found %v, err %v", found, err)
	}
	if err := claudeStore.SetActiveProfile("unpublished"); err != nil {
		t.Fatal(err)
	}

	_, err := OpenAccountRef(store, claudeStore, http.DefaultClient)
	if err != nil {
		t.Fatalf("ordinary LastUsed update wedged rollback recovery: %v", err)
	}
	if _, ok := claudeStore.FindProfile("unpublished"); ok {
		t.Fatal("rollback did not remove profile after ordinary LastUsed update")
	}
	if active, activeErr := accountRollbackActive(root); activeErr != nil || active {
		t.Fatalf("LastUsed recovery marker = active %v, err %v", active, activeErr)
	}
}

func TestPreparedClaudeRollbackJournalRejectsSubstitutedTarget(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	claudeStore := agentclaude.Store{Dir: root}
	if _, err := claudeStore.CreateProfile("unpublished"); err != nil {
		t.Fatal(err)
	}
	if _, err := claudeStore.CreateProfile("other"); err != nil {
		t.Fatal(err)
	}
	journal, found, err := prepareClaudeProfileRollback(context.Background(), root, "unpublished")
	if err != nil || !found {
		t.Fatalf("prepare rollback = found %v, err %v", found, err)
	}
	journal.TargetID = "other"
	if err := writeAccountRollbackJournal(root, journal); err != nil {
		t.Fatal(err)
	}

	_, err = OpenAccountRef(store, claudeStore, http.DefaultClient)
	if err == nil || !strings.Contains(err.Error(), "identity version does not match") {
		t.Fatalf("restart error = %v, want substituted-target diagnostic", err)
	}
	for _, name := range []string{"unpublished", "other"} {
		if _, ok := claudeStore.FindProfile(name); !ok {
			t.Fatalf("substituted journal removed profile %q", name)
		}
	}
	if _, statErr := os.Stat(accountRollbackActivePath(root)); statErr != nil {
		t.Fatalf("substituted-target marker was not retained: %v", statErr)
	}
}

func TestPreparedClaudeRollbackJournalRejectsChangedInstanceDir(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	claudeStore := agentclaude.Store{Dir: root}
	if _, err := claudeStore.CreateProfile("unpublished"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := prepareClaudeProfileRollback(context.Background(), root, "unpublished"); err != nil || !found {
		t.Fatalf("prepare rollback = found %v, err %v", found, err)
	}
	mutateClaudeRegistryForRollbackTest(t, claudeStore, func(profiles map[string]agentclaude.Profile) {
		profile := profiles["unpublished"]
		profile.Dir = "replacement-instance"
		profiles["unpublished"] = profile
	})

	_, err := OpenAccountRef(store, claudeStore, http.DefaultClient)
	if err == nil || !strings.Contains(err.Error(), "instance directory changed") {
		t.Fatalf("restart error = %v, want changed-instance diagnostic", err)
	}
	if profile, ok := claudeStore.FindProfile("unpublished"); !ok || profile.Dir != "replacement-instance" {
		t.Fatalf("changed rollback target was mutated: %+v, found %v", profile, ok)
	}
	if active, activeErr := accountRollbackActive(root); activeErr != nil || !active {
		t.Fatalf("changed-instance marker = active %v, err %v", active, activeErr)
	}
}

func TestPreparedClaudeRollbackJournalRejectsCaseOnlySurvivorInstanceDir(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("case-only filesystem aliases are conservatively rejected on macOS and Windows")
	}
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	claudeStore := agentclaude.Store{Dir: root}
	instancePath := filepath.Join(claudeStore.InstancesDir(), "Work")
	if err := os.MkdirAll(instancePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := claudeStore.RegisterProfile("Work", "Work"); err != nil {
		t.Fatal(err)
	}
	credentialPath := filepath.Join(instancePath, ".credentials.json")
	if err := os.WriteFile(credentialPath, []byte(`{"claudeAiOauth":{"accessToken":"survivor-secret"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Model a legacy/corrupt registry created before duplicate-dir prevention.
	mutateClaudeRegistryForRollbackTest(t, claudeStore, func(profiles map[string]agentclaude.Profile) {
		profiles["work"] = agentclaude.Profile{Name: "work", Dir: "work", CreatedAt: "legacy"}
	})
	if _, found, err := prepareClaudeProfileRollback(context.Background(), root, "Work"); err != nil || !found {
		t.Fatalf("prepare rollback = found %v, err %v", found, err)
	}

	recordPath := installFakeClaudeSecurityForRollbackTest(t)
	_, err := OpenAccountRef(store, claudeStore, http.DefaultClient)
	if err == nil || !strings.Contains(err.Error(), "already owned by profile \"work\"") {
		t.Fatalf("restart error = %v, want shared-instance diagnostic", err)
	}
	if matches, globErr := filepath.Glob(filepath.Join(filepath.Dir(instancePath), ".Work.remove-*")); globErr != nil || len(matches) != 0 {
		t.Fatalf("shared survivor path was staged: %v, err %v", matches, globErr)
	}
	if body, readErr := os.ReadFile(credentialPath); readErr != nil || !strings.Contains(string(body), "survivor-secret") {
		t.Fatalf("shared survivor credential was touched: body %q, err %v", body, readErr)
	}
	if runtime.GOOS == "darwin" {
		if _, readErr := os.ReadFile(recordPath); !errors.Is(readErr, os.ErrNotExist) {
			t.Fatalf("case-alias refusal touched Keychain: %v", readErr)
		}
	}
	for _, name := range []string{"Work", "work"} {
		if _, ok := claudeStore.FindProfile(name); !ok {
			t.Fatalf("shared-dir refusal removed profile %q", name)
		}
	}
	if _, statErr := os.Stat(accountRollbackActivePath(root)); statErr != nil {
		t.Fatalf("shared-dir refusal did not retain fail-closed marker: %v", statErr)
	}
}

func TestPreparedClaudeRollbackJournalRejectsSymlinkedSurvivorInstanceDir(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	claudeStore := agentclaude.Store{Dir: root}
	instancePath, err := claudeStore.CreateProfile("target")
	if err != nil {
		t.Fatal(err)
	}
	credentialPath := filepath.Join(instancePath, ".credentials.json")
	if err := os.WriteFile(credentialPath, []byte(`{"claudeAiOauth":{"accessToken":"survivor-secret"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	aliasPath := filepath.Join(claudeStore.InstancesDir(), "survivor-alias")
	if err := os.Symlink(instancePath, aliasPath); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	mutateClaudeRegistryForRollbackTest(t, claudeStore, func(profiles map[string]agentclaude.Profile) {
		profiles["survivor"] = agentclaude.Profile{Name: "survivor", Dir: "survivor-alias", CreatedAt: "legacy"}
	})
	if _, found, err := prepareClaudeProfileRollback(context.Background(), root, "target"); err != nil || !found {
		t.Fatalf("prepare rollback = found %v, err %v", found, err)
	}
	recordPath := installFakeClaudeSecurityForRollbackTest(t)

	_, err = OpenAccountRef(store, claudeStore, http.DefaultClient)
	if err == nil || !strings.Contains(err.Error(), "already owned by profile \"survivor\"") {
		t.Fatalf("restart error = %v, want symlink-alias diagnostic", err)
	}
	if body, readErr := os.ReadFile(credentialPath); readErr != nil || !strings.Contains(string(body), "survivor-secret") {
		t.Fatalf("symlink survivor credential was touched: body %q, err %v", body, readErr)
	}
	if matches, globErr := filepath.Glob(filepath.Join(filepath.Dir(instancePath), ".target.remove-*")); globErr != nil || len(matches) != 0 {
		t.Fatalf("symlink survivor path was staged: %v, err %v", matches, globErr)
	}
	if runtime.GOOS == "darwin" {
		if _, readErr := os.ReadFile(recordPath); !errors.Is(readErr, os.ErrNotExist) {
			t.Fatalf("symlink-alias refusal touched Keychain: %v", readErr)
		}
	}
	if _, statErr := os.Stat(accountRollbackActivePath(root)); statErr != nil {
		t.Fatalf("symlink-alias refusal did not retain fail-closed marker: %v", statErr)
	}
}

func TestPreparedClaudeRollbackJournalCompletesCredentialCleanupAfterCrash(t *testing.T) {
	for _, registryRemoved := range []bool{false, true} {
		name := "after-staging"
		if registryRemoved {
			name = "after-registry-deletion"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
			claudeStore := agentclaude.Store{Dir: root}
			instancePath, err := claudeStore.CreateProfile("unpublished")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(instancePath, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"must-delete"}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			snapshot, found, err := claudeStore.SnapshotProfileRemovalContext(t.Context(), "unpublished")
			if err != nil || !found {
				t.Fatalf("snapshot = found %v, err %v", found, err)
			}
			if _, found, err := prepareClaudeProfileRollback(context.Background(), root, "unpublished"); err != nil || !found {
				t.Fatalf("prepare rollback = found %v, err %v", found, err)
			}
			// Match the decimal suffix emitted by os.MkdirTemp in
			// stageProfileInstancePaths. Recovery intentionally ignores arbitrary
			// .remove-* lookalikes so it cannot claim another profile's directory.
			stagingRoot := filepath.Join(filepath.Dir(instancePath), ".unpublished.remove-123456789")
			stageClaudeRemovalForTest(t, instancePath, stagingRoot, snapshot.CredentialVersion)
			if registryRemoved {
				mutateClaudeRegistryForRollbackTest(t, claudeStore, func(profiles map[string]agentclaude.Profile) {
					delete(profiles, "unpublished")
				})
			}
			recordPath := installFakeClaudeSecurityForRollbackTest(t)

			if _, err := OpenAccountRef(store, claudeStore, http.DefaultClient); err != nil {
				t.Fatal(err)
			}
			if _, ok := claudeStore.FindProfile("unpublished"); ok {
				t.Fatal("restart cleanup retained unpublished registry entry")
			}
			if _, err := os.Lstat(instancePath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("restart cleanup restored canonical credential path: %v", err)
			}
			matches, err := filepath.Glob(filepath.Join(filepath.Dir(instancePath), ".unpublished.remove-*"))
			if err != nil || len(matches) != 0 {
				t.Fatalf("restart cleanup retained staged credential dirs %v, err %v", matches, err)
			}
			if active, activeErr := accountRollbackActive(root); activeErr != nil || active {
				t.Fatalf("completed cleanup marker = active %v, err %v", active, activeErr)
			}
			if runtime.GOOS == "darwin" {
				record, err := os.ReadFile(recordPath)
				if err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256([]byte(instancePath))
				wantService := "Claude Code-credentials-" + hex.EncodeToString(digest[:])[:8]
				if got := string(record); !strings.Contains(got, "delete-generic-password") || !strings.Contains(got, wantService) {
					t.Fatalf("restart Keychain cleanup = %q, want exact service %q", got, wantService)
				}
			}
		})
	}
}

func TestPreparedTenantClaudeDeleteJournalCompletesAcrossCrash(t *testing.T) {
	for _, registryRemoved := range []bool{false, true} {
		name := "after-staging"
		if registryRemoved {
			name = "after-registry-removal"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			journalDir := filepath.Join(root, "accounts")
			claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude-store")}
			instancePath, err := claudeStore.CreateProfile("work")
			if err != nil {
				t.Fatal(err)
			}
			if err := claudeStore.ImportProfileCredential("work", agentclaude.CredentialInfo{
				AccessToken: "must-delete", RefreshToken: "must-delete-refresh",
			}); err != nil {
				t.Fatal(err)
			}
			snapshot, found, err := claudeStore.SnapshotProfileRemovalContext(t.Context(), "work")
			if err != nil || !found {
				t.Fatalf("snapshot = found %v, err %v", found, err)
			}
			if err := advanceAccountDiskGeneration(journalDir); err != nil {
				t.Fatal(err)
			}
			if _, found, err := prepareClaudeProfileDelete(t.Context(), journalDir, "work", claudeStore, snapshot); err != nil || !found {
				t.Fatalf("prepare delete = found %v, err %v", found, err)
			}
			// Match the decimal suffix emitted by os.MkdirTemp in
			// stageProfileInstancePaths.
			stagingRoot := filepath.Join(filepath.Dir(instancePath), ".work.remove-123456789")
			stageClaudeRemovalForTest(t, instancePath, stagingRoot, snapshot.CredentialVersion)
			if registryRemoved {
				mutateClaudeRegistryForRollbackTest(t, claudeStore, func(profiles map[string]agentclaude.Profile) {
					delete(profiles, "work")
				})
			}
			installFakeClaudeSecurityForRollbackTest(t)

			reconciled, err := reconcileCompletedAccountRollback(t.Context(), conventionalAccountStore(journalDir), advanceAccountDiskGeneration)
			if err != nil || !reconciled {
				t.Fatalf("restart reconciliation = reconciled %v, err %v", reconciled, err)
			}
			if _, found := claudeStore.FindProfile("work"); found {
				t.Fatal("restart retained deleted Claude registry entry")
			}
			if matches, err := filepath.Glob(filepath.Join(filepath.Dir(instancePath), ".work.remove-*")); err != nil || len(matches) != 0 {
				t.Fatalf("restart retained staged credential dirs %v, err %v", matches, err)
			}
			if active, err := accountRollbackActive(journalDir); err != nil || active {
				t.Fatalf("completed delete marker = active %v, err %v", active, err)
			}
		})
	}
}

func TestTenantDeleteJournalPreservesDistinctSelectorAndStoredIdentity(t *testing.T) {
	root := t.TempDir()
	journalDir := filepath.Join(root, "accounts")
	storedStore := accounts.CodexStore{Dir: filepath.Join(root, "stored")}
	stored := accounts.StoredCodexAccount{
		Email: "work@example.com", Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "model-secret"},
	}
	if err := storedStore.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	stored, found, err := storedStore.FindStored(stored.Email)
	if err != nil || !found {
		t.Fatalf("stored account = found %v, err %v", found, err)
	}
	claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
	if _, err := claudeStore.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	snapshot, found, err := claudeStore.SnapshotProfileRemovalContext(t.Context(), "work")
	if err != nil || !found {
		t.Fatalf("Claude snapshot = found %v, err %v", found, err)
	}
	journal, found, err := prepareTenantAccountDelete(
		t.Context(), journalDir, "work", claudeStore, snapshot,
		stored, storedStore.StoreDir(), tenantQwenConsoleVersion{}, "",
	)
	if err != nil || !found {
		t.Fatalf("prepare tenant delete = found %v, err %v", found, err)
	}
	if journal.TargetID != "work" || journal.StoredTargetID != "work@example.com" {
		t.Fatalf("journal identities = selector %q stored %q", journal.TargetID, journal.StoredTargetID)
	}
	loaded, active, err := readAccountRollbackJournal(journalDir)
	if err != nil || !active {
		t.Fatalf("read tenant journal = active %v, err %v", active, err)
	}
	if loaded.TargetID != journal.TargetID || loaded.StoredTargetID != journal.StoredTargetID {
		t.Fatalf("loaded identities = selector %q stored %q", loaded.TargetID, loaded.StoredTargetID)
	}
}

func TestPreparedTenantClaudeDeleteJournalAbortsSameDirectoryReplacement(t *testing.T) {
	root := t.TempDir()
	journalDir := filepath.Join(root, "accounts")
	claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude-store")}
	if err := claudeStore.ImportProfileCredential("work", agentclaude.CredentialInfo{
		AccessToken: "old-access", RefreshToken: "old-refresh",
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, found, err := claudeStore.SnapshotProfileRemovalContext(t.Context(), "work")
	if err != nil || !found {
		t.Fatalf("snapshot = found %v, err %v", found, err)
	}
	if err := advanceAccountDiskGeneration(journalDir); err != nil {
		t.Fatal(err)
	}
	if _, found, err := prepareClaudeProfileDelete(t.Context(), journalDir, "work", claudeStore, snapshot); err != nil || !found {
		t.Fatalf("prepare delete = found %v, err %v", found, err)
	}
	instancePath := claudeStore.ClaudeConfigDir("work")
	journalStage := filepath.Join(filepath.Dir(instancePath), "."+filepath.Base(instancePath)+".remove-123456789")
	stageClaudeRemovalForTest(t, instancePath, journalStage, snapshot.CredentialVersion)
	unrelatedStage := filepath.Join(filepath.Dir(instancePath), "."+filepath.Base(instancePath)+".remove-unrelated")
	if err := os.MkdirAll(filepath.Join(unrelatedStage, "instance"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unrelatedStage, "instance", ".credentials.json"), []byte(`{"unrelated":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := claudeStore.ImportProfileCredential("work", agentclaude.CredentialInfo{
		AccessToken: "replacement-access", RefreshToken: "replacement-refresh",
	}); err != nil {
		t.Fatal(err)
	}

	reconciled, err := reconcileCompletedAccountRollback(t.Context(), conventionalAccountStore(journalDir), advanceAccountDiskGeneration)
	if err != nil || !reconciled {
		t.Fatalf("replacement reconciliation = reconciled %v, err %v", reconciled, err)
	}
	if active, err := accountRollbackActive(journalDir); err != nil || active {
		t.Fatalf("replacement marker = active %v, err %v", active, err)
	}
	credential, err := claudeStore.ReadCredential(t.Context(), claudeStore.ClaudeConfigDir("work"))
	if err != nil || credential == nil || credential.AccessToken != "replacement-access" {
		t.Fatalf("replacement changed by reconciliation: credential=%+v err=%v", credential, err)
	}
	if _, err := os.Lstat(journalStage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal-owned staged credential was not cleaned: %v", err)
	}
	if _, err := os.Lstat(unrelatedStage); err != nil {
		t.Fatalf("unrelated staged credential was removed: %v", err)
	}
}

func mutateClaudeRegistryForRollbackTest(t *testing.T, store agentclaude.Store, mutate func(map[string]agentclaude.Profile)) {
	t.Helper()
	body, err := os.ReadFile(store.ProfilesPath())
	if err != nil {
		t.Fatal(err)
	}
	var registry struct {
		Active   string                         `json:"active,omitempty"`
		Profiles map[string]agentclaude.Profile `json:"profiles"`
	}
	if err := json.Unmarshal(body, &registry); err != nil {
		t.Fatal(err)
	}
	mutate(registry.Profiles)
	body, err = json.MarshalIndent(registry, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(store.ProfilesPath(), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func installFakeClaudeSecurityForRollbackTest(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "darwin" {
		return ""
	}
	fakeBin := t.TempDir()
	recordPath := filepath.Join(fakeBin, "security-record")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$SUBROUTER_ROLLBACK_KEYCHAIN_RECORD\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "security"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SUBROUTER_ROLLBACK_KEYCHAIN_RECORD", recordPath)
	return recordPath
}

func stageClaudeRemovalForTest(t *testing.T, originalPath, stagingRoot, credentialSetVersion string) {
	t.Helper()
	originalPath, err := filepath.Abs(filepath.Clean(originalPath))
	if err != nil {
		t.Fatal(err)
	}
	stagingRoot, err = filepath.Abs(filepath.Clean(stagingRoot))
	if err != nil {
		t.Fatal(err)
	}
	operationID := "00112233445566778899aabbccddeeff"
	manifest, err := json.Marshal(map[string]any{
		"version":                3,
		"original_path":          originalPath,
		"staging_root":           stagingRoot,
		"entry_name":             "instance",
		"operation_id":           operationID,
		"credential_set_version": credentialSetVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stagingRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagingRoot, ".subrouter-removal-stage.json"), append(manifest, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	marker, err := json.Marshal(map[string]any{
		"version":                2,
		"original_path":          originalPath,
		"operation_id":           operationID,
		"credential_set_version": credentialSetVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(originalPath, ".subrouter-removal-operation.json"), append(marker, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(originalPath, filepath.Join(stagingRoot, "instance")); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedClaudeRollbackJournalRejectsCorruptRegistry(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	claudeStore := agentclaude.Store{Dir: root}
	if _, err := claudeStore.CreateProfile("unpublished"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := prepareClaudeProfileRollback(context.Background(), root, "unpublished"); err != nil || !found {
		t.Fatalf("prepare rollback = found %v, err %v", found, err)
	}
	if err := os.WriteFile(claudeStore.ProfilesPath(), []byte("{not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := OpenAccountRef(store, claudeStore, http.DefaultClient)
	if err == nil || !strings.Contains(err.Error(), "decode Claude profile registry") {
		t.Fatalf("restart error = %v, want corrupt-registry diagnostic", err)
	}
	if active, activeErr := accountRollbackActive(root); activeErr != nil || !active {
		t.Fatalf("corrupt-registry marker = active %v, err %v", active, activeErr)
	}
}

func TestSyncAccountStateDirWindowsSkipsUnsupportedDirectoryOpen(t *testing.T) {
	opened := false
	err := syncAccountStateDirForOS("windows", "C:\\state", func(string) (*os.File, error) {
		opened = true
		return nil, errors.New("must not open")
	})
	if err != nil || opened {
		t.Fatalf("Windows directory sync = opened %v, err %v", opened, err)
	}
}

func TestSyncAccountStateDirUnixUsesProvidedDirectory(t *testing.T) {
	dirPath := t.TempDir()
	var opened string
	err := syncAccountStateDirForOS("darwin", dirPath, func(path string) (*os.File, error) {
		opened = path
		return os.Open(path)
	})
	if err != nil {
		t.Fatal(err)
	}
	if opened != dirPath {
		t.Fatalf("opened directory = %q, want %q", opened, dirPath)
	}
}

func TestAdvanceAccountDiskGenerationReportsDirectorySyncFailure(t *testing.T) {
	storeDir := t.TempDir()
	want := errors.New("generation directory sync unavailable")
	err := advanceAccountDiskGenerationWithSync(storeDir, func(path string) error {
		if path != storeDir {
			t.Fatalf("sync path = %q, want %q", path, storeDir)
		}
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("generation sync failure = %v, want %v", err, want)
	}
	if generation, readErr := readAccountDiskGeneration(storeDir); readErr != nil || generation == "" {
		t.Fatalf("visible generation = %q, err %v", generation, readErr)
	}
}

func TestAccountRefKeepsServingCompleteSnapshotDuringPublishedMutation(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	seed := accounts.StoredCodexAccount{
		Email: "apikey:seed", Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-seed"},
	}
	if err := store.SaveStored(seed); err != nil {
		t.Fatal(err)
	}
	ref, err := OpenAccountRef(store, agentclaude.Store{Dir: t.TempDir()}, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	held, err := lockAccountImportTransaction(context.Background(), store.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := advanceAccountDiskGeneration(store.StoreDir()); err != nil {
		_ = held.Close()
		t.Fatal(err)
	}
	added := accounts.StoredCodexAccount{
		Email: "apikey:added", Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-added"},
	}
	if err := store.SaveStored(added); err != nil {
		_ = held.Close()
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	reloaded, _, err := ref.reloadIfDiskGenerationChanged(ctx)
	if err != nil || reloaded {
		_ = held.Close()
		t.Fatalf("in-flight mutation reload = %v, %v, want false/nil", reloaded, err)
	}
	if len(ref.All()) != 1 {
		_ = held.Close()
		t.Fatalf("in-flight mutation exposed partial snapshot: %+v", ref.All())
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	reloaded, _, err = ref.reloadIfDiskGenerationChanged(context.Background())
	if err != nil || !reloaded {
		t.Fatalf("completed mutation reload = %v, %v, want true/nil", reloaded, err)
	}
	if len(ref.All()) != 2 {
		t.Fatalf("completed mutation snapshot = %+v", ref.All())
	}
}

func TestOlderUsageRefreshCannotOverwriteNewerAccountReload(t *testing.T) {
	accountStore := accounts.CodexStore{Dir: t.TempDir()}
	initialStored := proxyStoredOAuthAccount("old@example.com", "old", time.Now().Add(time.Hour))
	addedStored := proxyStoredOAuthAccount("new@example.com", "new", time.Now().Add(time.Hour))
	if err := accountStore.SaveStored(initialStored); err != nil {
		t.Fatal(err)
	}
	initial, err := accountStore.List()
	if err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(accountStore, initial, nil)
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{
		AccountID: "old@example.com", Headroom: 0.9, ShortHeadroom: 0.9,
	}}))
	schedulerRef.SetUpdatedAt(time.Time{})
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	refreshDone := make(chan struct{})
	server := Server{
		AccountRef:    ref,
		SchedulerRef:  schedulerRef,
		UsageScoreTTL: time.Nanosecond,
		ScoreAccounts: func(_ context.Context, available []accounts.Account) ([]selectacct.Score, int) {
			if len(available) == 1 {
				close(refreshStarted)
				<-releaseRefresh
				return []selectacct.Score{{
					AccountID: "old@example.com", Headroom: 0.1, ShortHeadroom: 0.1,
				}}, 1
			}
			return []selectacct.Score{
				{AccountID: "old@example.com", Headroom: 0.8, ShortHeadroom: 0.8},
				{AccountID: "new@example.com", Headroom: 0.42, ShortHeadroom: 0.42},
			}, 2
		},
	}
	go func() {
		server.refreshUsageScoresIfStale(context.Background())
		close(refreshDone)
	}()
	select {
	case <-refreshStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("usage refresh did not start")
	}

	if err := accountStore.SaveStored(addedStored); err != nil {
		t.Fatal(err)
	}
	if _, _, err := server.reloadAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(releaseRefresh)
	select {
	case <-refreshDone:
	case <-time.After(5 * time.Second):
		t.Fatal("usage refresh did not finish")
	}

	got := schedulerRef.Get().ScoreFor(accounts.ProviderCodex, "new@example.com")
	if got.Headroom != 0.42 {
		t.Fatalf("older refresh overwrote reloaded scheduler: new-account headroom = %v, want 0.42", got.Headroom)
	}
}

func TestGenericReloadPreservesUnchangedTerminalCredentialState(t *testing.T) {
	store := accounts.CodexStore{Dir: t.TempDir()}
	stored := proxyStoredOAuthAccount("dead@example.com", "dead", time.Now().Add(time.Hour))
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	account := loaded[0]
	ref := NewAccountRef(store, loaded, nil)
	scheduler := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	ref.noteCredResult(account, errors.New("invalid_grant"))
	scheduler.MarkExhaustedUntil(account.Provider, account.ID, "", time.Now().Add(time.Hour))
	server := Server{AccountRef: ref, SchedulerRef: scheduler, CredentialBroker: &fakeCredentialBroker{}}

	if _, _, err := server.reloadAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}

	if _, dead := ref.terminalCredFailure(account); !dead {
		t.Fatal("generic reload cleared an unchanged terminal credential failure")
	}
	if _, exhausted := scheduler.ExhaustedUntilFor(account.Provider, account.ID, ""); !exhausted {
		t.Fatal("generic reload cleared an unchanged credential exclusion")
	}
}

func TestTerminalCredentialFailureIsScopedToCredentialToken(t *testing.T) {
	old := accounts.Account{
		ID: "repaired@example.com", Provider: accounts.ProviderCodex, Token: "same-access-token",
		CredentialVersion: "old-refresh-chain",
	}
	replacement := old
	replacement.CredentialVersion = "replacement-refresh-chain"
	ref := &AccountRef{}

	ref.noteCredResult(old, errors.New("invalid_grant"))
	if _, dead := ref.terminalCredFailure(replacement); dead {
		t.Fatal("replacement inherited the old credential's terminal failure")
	}

	// A late result from work that captured the old credential stays attached
	// to that credential and cannot poison the replacement.
	ref.noteCredResult(old, errors.New("invalid_grant"))
	if _, dead := ref.terminalCredFailure(replacement); dead {
		t.Fatal("late old-credential failure poisoned the replacement")
	}

	ref.noteCredResult(replacement, errors.New("invalid_grant"))
	if _, dead := ref.terminalCredFailure(replacement); !dead {
		t.Fatal("replacement's own terminal failure was not remembered")
	}
}

func TestQwenAnthropicUnauthorizedMarksSharedTokenPlanCredential(t *testing.T) {
	for _, storedProvider := range []accounts.Provider{
		accounts.ProviderQwenToken,
		accounts.ProviderQwenAnthropic,
	} {
		t.Run(string(storedProvider), func(t *testing.T) {
			stored := accounts.Account{
				ID: "qwen-token:work", Provider: storedProvider,
				AuthMode: accounts.AuthModeAPIKey, Token: "shared-key",
			}
			requestAccount := stored
			requestAccount.Provider = accounts.ProviderQwenAnthropic
			ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{stored}, nil)
			scheduler := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{
				AccountID: stored.ID, Provider: accounts.ProviderQwenToken, Headroom: 1, ShortHeadroom: 1,
			}}))
			server := Server{AccountRef: ref, SchedulerRef: scheduler}
			server.accountListSnapshotContext(context.Background())

			server.markAccountExhaustedFromResponseForAccount(
				requestAccount, "", http.StatusUnauthorized, http.Header{},
			)
			until, blocked := scheduler.ExhaustedUntilFor(accounts.ProviderQwenToken, stored.ID, "")
			if !blocked || time.Until(until) < 50*time.Minute {
				t.Fatalf("shared credential was not excluded after cross-protocol 401: blocked=%v until=%v", blocked, until)
			}
		})
	}
}

func TestQwenAnthropicForbiddenMarksSharedTokenPlanAccount(t *testing.T) {
	account := accounts.Account{
		ID: "qwen-token:work", Provider: accounts.ProviderQwenAnthropic,
		AuthMode: accounts.AuthModeAPIKey, Token: "shared-key",
	}
	scheduler := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{
		AccountID: account.ID, Provider: accounts.ProviderQwenToken,
		Headroom: 1, ShortHeadroom: 1,
	}}))
	server := Server{SchedulerRef: scheduler}

	server.markAccountExhaustedFromResponseForAccount(
		account, "", http.StatusForbidden, http.Header{},
	)
	until, blocked := scheduler.ExhaustedUntilFor(accounts.ProviderQwenToken, account.ID, "")
	if !blocked || time.Until(until) < 50*time.Minute {
		t.Fatalf("shared account was not excluded after cross-protocol 403: blocked=%v until=%v", blocked, until)
	}
}
