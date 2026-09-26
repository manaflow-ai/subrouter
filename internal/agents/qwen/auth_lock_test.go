package qwen

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConsoleCredentialLockSerializesAcrossProcesses(t *testing.T) {
	if os.Getenv("SUBROUTER_QWEN_LOCK_HELPER") == "1" {
		root := os.Getenv("SUBROUTER_QWEN_LOCK_ROOT")
		ready := os.Getenv("SUBROUTER_QWEN_LOCK_READY")
		if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
			os.Exit(2)
		}
		if err := SaveConsoleCredentialIn(root, "qwen-token:work", ConsoleCredential{AccessToken: "replacement"}); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}

	root := t.TempDir()
	ready := filepath.Join(t.TempDir(), "ready")
	lock, err := lockConsoleCredential(root, "qwen-token:work")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestConsoleCredentialLockSerializesAcrossProcesses$")
	cmd.Env = append(os.Environ(),
		"SUBROUTER_QWEN_LOCK_HELPER=1",
		"SUBROUTER_QWEN_LOCK_ROOT="+root,
		"SUBROUTER_QWEN_LOCK_READY="+ready,
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
			t.Fatal("helper did not reach the cross-process lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-waitCh:
		_ = lock.Close()
		t.Fatalf("helper bypassed held cross-process lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-waitCh; err != nil {
		t.Fatal(err)
	}
	credential, err := ExportConsoleCredentialIn(root, "qwen-token:work")
	if err != nil || credential.AccessToken != "replacement" {
		t.Fatalf("serialized replacement = %+v, err %v", credential, err)
	}
}

func TestExactConsoleRemovalReplaysCrashSafeStage(t *testing.T) {
	if root := os.Getenv("SUBROUTER_QWEN_REMOVE_CRASH_ROOT"); root != "" {
		accountID := os.Getenv("SUBROUTER_QWEN_REMOVE_CRASH_ACCOUNT")
		version := os.Getenv("SUBROUTER_QWEN_REMOVE_CRASH_VERSION")
		modelPath := os.Getenv("SUBROUTER_QWEN_REMOVE_CRASH_MODEL")
		phase := os.Getenv("SUBROUTER_QWEN_REMOVE_CRASH_PHASE")
		_, _ = RemoveConsoleCredentialExactIn(root, accountID, true, version, func() (bool, error) {
			if phase == "after-model" {
				if err := os.Remove(modelPath); err != nil {
					os.Exit(43)
				}
				os.Exit(42)
			}
			os.Exit(41)
			return false, nil
		})
		os.Exit(44)
	}

	for _, phase := range []string{"before-model", "after-model"} {
		t.Run(phase, func(t *testing.T) {
			root := t.TempDir()
			accountID := "qwen-token:work"
			modelPath := filepath.Join(t.TempDir(), "model-account")
			if err := os.WriteFile(modelPath, []byte("model-secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := SaveConsoleCredentialIn(root, accountID, ConsoleCredential{AccessToken: "console-secret", Account: "owner@example.com"}); err != nil {
				t.Fatal(err)
			}
			found, version, err := ConsoleCredentialVersionIn(root, accountID)
			if err != nil || !found {
				t.Fatalf("initial version = found %v version %q err %v", found, version, err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestExactConsoleRemovalReplaysCrashSafeStage$")
			cmd.Env = append(os.Environ(),
				"SUBROUTER_QWEN_REMOVE_CRASH_ROOT="+root,
				"SUBROUTER_QWEN_REMOVE_CRASH_ACCOUNT="+accountID,
				"SUBROUTER_QWEN_REMOVE_CRASH_VERSION="+version,
				"SUBROUTER_QWEN_REMOVE_CRASH_MODEL="+modelPath,
				"SUBROUTER_QWEN_REMOVE_CRASH_PHASE="+phase,
			)
			runErr := cmd.Run()
			wantExit := 41
			if phase == "after-model" {
				wantExit = 42
			}
			if exitErr, ok := runErr.(*exec.ExitError); !ok || exitErr.ExitCode() != wantExit {
				t.Fatalf("crash helper = %v, want exit %d", runErr, wantExit)
			}
			liveDir := ConsoleConfigDirIn(root, accountID)
			if _, err := os.Stat(liveDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("console profile remained live across crash: %v", err)
			}
			if _, err := os.Stat(liveDir + consoleRemovalStageSuffix); err != nil {
				t.Fatalf("durable console stage missing after crash: %v", err)
			}
			stagedFound, stagedVersion, err := ConsoleCredentialVersionIn(root, accountID)
			if err != nil || !stagedFound || stagedVersion != version {
				t.Fatalf("staged version = found %v version %q err %v", stagedFound, stagedVersion, err)
			}
			if err := ReconcileConsoleCredentialRemovalsIn(root, func(gotAccountID string) (bool, error) {
				if gotAccountID != accountID {
					return false, errors.New("unexpected journal account")
				}
				_, err := os.Stat(modelPath)
				if err == nil {
					return true, nil
				}
				if errors.Is(err, os.ErrNotExist) {
					return false, nil
				}
				return false, err
			}); err != nil {
				t.Fatalf("startup reconciliation: %v", err)
			}
			if _, err := os.Stat(liveDir + consoleRemovalStageSuffix); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("staged console profile after replay: %v", err)
			}
			if _, err := os.Stat(consoleRemovalJournalPathIn(root, accountID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("removal journal after replay: %v", err)
			}
			if phase == "before-model" {
				credential, err := ExportConsoleCredentialIn(root, accountID)
				if err != nil || credential.AccessToken != "console-secret" {
					t.Fatalf("restored console credential = %+v err %v", credential, err)
				}
				if _, err := os.Stat(modelPath); err != nil {
					t.Fatalf("live model account after rollback: %v", err)
				}
			} else {
				if _, err := os.Stat(liveDir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("live console profile after committed replay: %v", err)
				}
				if _, err := os.Stat(modelPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("model account after committed replay: %v", err)
				}
			}
		})
	}
}

func TestExactConsoleRemovalAbsentCrashPreservesOrphanMarker(t *testing.T) {
	if root := os.Getenv("SUBROUTER_QWEN_ABSENT_CRASH_ROOT"); root != "" {
		accountID := os.Getenv("SUBROUTER_QWEN_ABSENT_CRASH_ACCOUNT")
		modelPath := os.Getenv("SUBROUTER_QWEN_ABSENT_CRASH_MODEL")
		liveDir := ConsoleConfigDirIn(root, accountID)
		_, _ = RemoveConsoleCredentialExactIn(root, accountID, false, "", func() (bool, error) {
			if err := os.MkdirAll(liveDir, 0o700); err != nil {
				os.Exit(51)
			}
			if err := os.WriteFile(filepath.Join(liveDir, "config.json"), []byte(`{"access_token":"orphan"}`), 0o600); err != nil {
				os.Exit(52)
			}
			if err := os.Remove(modelPath); err != nil {
				os.Exit(53)
			}
			os.Exit(54)
			return false, nil
		})
		os.Exit(55)
	}

	root := t.TempDir()
	accountID := "qwen-token:absent-crash-orphan"
	modelPath := filepath.Join(t.TempDir(), "model-account")
	if err := os.WriteFile(modelPath, []byte("model-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestExactConsoleRemovalAbsentCrashPreservesOrphanMarker$")
	cmd.Env = append(os.Environ(),
		"SUBROUTER_QWEN_ABSENT_CRASH_ROOT="+root,
		"SUBROUTER_QWEN_ABSENT_CRASH_ACCOUNT="+accountID,
		"SUBROUTER_QWEN_ABSENT_CRASH_MODEL="+modelPath,
	)
	runErr := cmd.Run()
	if exitErr, ok := runErr.(*exec.ExitError); !ok || exitErr.ExitCode() != 54 {
		t.Fatalf("absent crash helper = %v, want exit 54", runErr)
	}
	livePath := filepath.Join(ConsoleConfigDirIn(root, accountID), "config.json")
	journalPath := consoleRemovalJournalPathIn(root, accountID)
	for _, path := range []string{livePath, journalPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("crash did not preserve %s: %v", path, err)
		}
	}
	if _, err := os.Stat(modelPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("model deletion did not commit before crash: %v", err)
	}
	journal, err := readConsoleRemovalJournal(journalPath)
	if err != nil || journal.CredentialFound || journal.CredentialVersion != "" {
		t.Fatalf("absence journal = %+v err %v", journal, err)
	}
	if err := ReconcileConsoleCredentialRemovalsIn(root, func(string) (bool, error) { return false, nil }); err == nil || !strings.Contains(err.Error(), "absence removal journal has a live console replacement") {
		t.Fatalf("startup did not detect absent-snapshot orphan: %v", err)
	}
	for _, path := range []string{livePath, journalPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("startup discarded fail-closed %s: %v", path, err)
		}
	}
}

func TestExactConsoleRemovalRestoresDurablyWhenParentSyncFails(t *testing.T) {
	root := t.TempDir()
	accountID := "qwen-token:work"
	if err := SaveConsoleCredentialIn(root, accountID, ConsoleCredential{AccessToken: "original"}); err != nil {
		t.Fatal(err)
	}
	found, version, err := ConsoleCredentialVersionIn(root, accountID)
	if err != nil || !found {
		t.Fatalf("version = found %v, err %v", found, err)
	}
	want := errors.New("directory sync unavailable")
	originalSync := syncConsoleDirectory
	syncConsoleDirectory = func(path string) error {
		if filepath.Clean(path) == filepath.Clean(root) {
			return want
		}
		return originalSync(path)
	}
	t.Cleanup(func() { syncConsoleDirectory = originalSync })
	accountRemoved := false
	removed, err := RemoveConsoleCredentialExactIn(root, accountID, true, version, func() (bool, error) {
		accountRemoved = true
		return true, nil
	})
	if removed || accountRemoved || !errors.Is(err, want) {
		t.Fatalf("sync-failed removal = removed %v account_removed %v err %v", removed, accountRemoved, err)
	}
	credential, readErr := ExportConsoleCredentialIn(root, accountID)
	if readErr != nil || strings.TrimSpace(credential.AccessToken) != "original" {
		t.Fatalf("sync-failed removal did not restore credential: %+v err %v", credential, readErr)
	}
}

func TestExactConsoleRemovalRevalidatesStageBeforeModelDeletion(t *testing.T) {
	root := t.TempDir()
	accountID := "qwen-token:concurrent-console-writer"
	if err := SaveConsoleCredentialIn(root, accountID, ConsoleCredential{AccessToken: "original"}); err != nil {
		t.Fatal(err)
	}
	found, version, err := ConsoleCredentialVersionIn(root, accountID)
	if err != nil || !found {
		t.Fatalf("version = found %v, err %v", found, err)
	}
	liveDir := ConsoleConfigDirIn(root, accountID)
	stagedDir := liveDir + consoleRemovalStageSuffix
	latePath := filepath.Join(stagedDir, "late-console-write")
	originalSync := syncConsoleDirectory
	rootSyncs := 0
	syncConsoleDirectory = func(path string) error {
		if err := originalSync(path); err != nil {
			return err
		}
		if filepath.Clean(path) == filepath.Clean(root) {
			rootSyncs++
			// First root sync publishes the journal. The second makes the
			// live-to-staged rename durable; simulate a non-cooperating CLI
			// writer immediately after that publication boundary.
			if rootSyncs == 2 {
				return os.WriteFile(latePath, []byte("late-secret"), 0o600)
			}
		}
		return nil
	}
	t.Cleanup(func() { syncConsoleDirectory = originalSync })
	callbackCalled := false
	removed, err := RemoveConsoleCredentialExactIn(root, accountID, true, version, func() (bool, error) {
		callbackCalled = true
		return true, nil
	})
	if removed || callbackCalled || err == nil || !strings.Contains(err.Error(), "changed during removal") {
		t.Fatalf("post-stage mutation = removed %v callback %v err %v", removed, callbackCalled, err)
	}
	credential, readErr := ExportConsoleCredentialIn(root, accountID)
	if readErr != nil || credential.AccessToken != "original" {
		t.Fatalf("restored credential = %+v err %v", credential, readErr)
	}
	lateBody, readErr := os.ReadFile(filepath.Join(liveDir, "late-console-write"))
	if readErr != nil || string(lateBody) != "late-secret" {
		t.Fatalf("restored late write = %q err %v", lateBody, readErr)
	}
	if _, err := os.Lstat(stagedDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged directory remains after safe restore: %v", err)
	}
	if _, err := os.Lstat(consoleRemovalJournalPathIn(root, accountID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal remains after durable restore: %v", err)
	}
}

func TestExactConsoleRemovalPreservesStageMutatedDuringModelDeletion(t *testing.T) {
	root := t.TempDir()
	accountID := "qwen-token:callback-stage-writer"
	if err := SaveConsoleCredentialIn(root, accountID, ConsoleCredential{AccessToken: "original"}); err != nil {
		t.Fatal(err)
	}
	found, version, err := ConsoleCredentialVersionIn(root, accountID)
	if err != nil || !found {
		t.Fatalf("version = found %v, err %v", found, err)
	}
	liveDir := ConsoleConfigDirIn(root, accountID)
	stagedDir := liveDir + consoleRemovalStageSuffix
	removed, err := RemoveConsoleCredentialExactIn(root, accountID, true, version, func() (bool, error) {
		if err := os.WriteFile(filepath.Join(stagedDir, "late-console-write"), []byte("late-secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		return true, nil
	})
	if !removed || err == nil || !strings.Contains(err.Error(), "changed after model removal") {
		t.Fatalf("callback-stage mutation = removed %v err %v", removed, err)
	}
	if _, err := os.Lstat(liveDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live profile resurrected after committed model removal: %v", err)
	}
	for _, name := range []string{"config.json", "late-console-write"} {
		if _, err := os.Stat(filepath.Join(stagedDir, name)); err != nil {
			t.Fatalf("preserved staged %s: %v", name, err)
		}
	}
	if _, err := os.Stat(consoleRemovalJournalPathIn(root, accountID)); err != nil {
		t.Fatalf("fail-closed journal was cleared: %v", err)
	}
	if err := ReconcileConsoleCredentialRemovalsIn(root, func(string) (bool, error) { return false, nil }); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mutated stage did not reconcile fail closed: %v", err)
	}
}

func TestExactConsoleRemovalPreservesLiveReplacementAfterModelDeletion(t *testing.T) {
	root := t.TempDir()
	accountID := "qwen-token:callback-live-writer"
	if err := SaveConsoleCredentialIn(root, accountID, ConsoleCredential{AccessToken: "original"}); err != nil {
		t.Fatal(err)
	}
	found, version, err := ConsoleCredentialVersionIn(root, accountID)
	if err != nil || !found {
		t.Fatalf("version = found %v, err %v", found, err)
	}
	liveDir := ConsoleConfigDirIn(root, accountID)
	stagedDir := liveDir + consoleRemovalStageSuffix
	removed, err := RemoveConsoleCredentialExactIn(root, accountID, true, version, func() (bool, error) {
		if err := os.MkdirAll(liveDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(liveDir, "config.json"), []byte(`{"access_token":"replacement"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return true, nil
	})
	if !removed || err == nil || !strings.Contains(err.Error(), "replacement appeared after model removal") {
		t.Fatalf("callback-live replacement = removed %v err %v", removed, err)
	}
	for _, path := range []string{filepath.Join(liveDir, "config.json"), filepath.Join(stagedDir, "config.json"), consoleRemovalJournalPathIn(root, accountID)} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("preserved fail-closed path %s: %v", path, err)
		}
	}
	if err := ReconcileConsoleCredentialRemovalsIn(root, func(string) (bool, error) { return false, nil }); err == nil || !strings.Contains(err.Error(), "live console replacement after model removal") {
		t.Fatalf("restart did not preserve committed split state: %v", err)
	}
	for _, path := range []string{filepath.Join(liveDir, "config.json"), filepath.Join(stagedDir, "config.json"), consoleRemovalJournalPathIn(root, accountID)} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("restart discarded fail-closed path %s: %v", path, err)
		}
	}
}

func TestExactConsoleRemovalDetectsLiveProfileCreatedFromAbsentState(t *testing.T) {
	root := t.TempDir()
	accountID := "qwen-token:absent-callback-live-writer"
	found, version, err := ConsoleCredentialVersionIn(root, accountID)
	if err != nil || found || version != "" {
		t.Fatalf("initial version = found %v version %q err %v", found, version, err)
	}
	liveDir := ConsoleConfigDirIn(root, accountID)
	removed, err := RemoveConsoleCredentialExactIn(root, accountID, false, "", func() (bool, error) {
		if err := os.MkdirAll(liveDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(liveDir, "config.json"), []byte(`{"access_token":"replacement"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return true, nil
	})
	if !removed || err == nil || !strings.Contains(err.Error(), "replacement appeared after model removal") {
		t.Fatalf("absent-state live replacement = removed %v err %v", removed, err)
	}
	body, readErr := os.ReadFile(filepath.Join(liveDir, "config.json"))
	if readErr != nil || !strings.Contains(string(body), "replacement") {
		t.Fatalf("preserved replacement = %q err %v", body, readErr)
	}
	if _, err := os.Stat(consoleRemovalJournalPathIn(root, accountID)); err != nil {
		t.Fatalf("absent-state fail-closed journal missing: %v", err)
	}
	if err := ReconcileConsoleCredentialRemovalsIn(root, func(string) (bool, error) { return false, nil }); err == nil || !strings.Contains(err.Error(), "absence removal journal has a live console replacement") {
		t.Fatalf("absent-state orphan did not reconcile fail closed: %v", err)
	}
}

func TestExactConsoleRemovalClearsAbsentJournalWhenModelDeletionDoesNotCommit(t *testing.T) {
	root := t.TempDir()
	accountID := "qwen-token:absent-model-remains"
	want := errors.New("model account changed")
	removed, err := RemoveConsoleCredentialExactIn(root, accountID, false, "", func() (bool, error) {
		return false, want
	})
	if removed || !errors.Is(err, want) {
		t.Fatalf("absent rollback = removed %v err %v", removed, err)
	}
	if _, err := os.Lstat(consoleRemovalJournalPathIn(root, accountID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent rollback journal remains: %v", err)
	}
}

func TestAbsentConsoleRemovalJournalReconciliation(t *testing.T) {
	tests := []struct {
		name        string
		modelFound  bool
		liveRepair  bool
		wantError   string
		wantJournal bool
	}{
		{name: "model remains and live repair", modelFound: true, liveRepair: true},
		{name: "model deletion completed without live profile"},
		{name: "model deletion completed with orphan", liveRepair: true, wantError: "absence removal journal has a live console replacement", wantJournal: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			accountID := "qwen-token:absent-reconcile"
			if err := writeConsoleRemovalJournalUnlockedIn(root, accountID, false, ""); err != nil {
				t.Fatal(err)
			}
			livePath := filepath.Join(ConsoleConfigDirIn(root, accountID), "config.json")
			if tt.liveRepair {
				if err := os.MkdirAll(filepath.Dir(livePath), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(livePath, []byte(`{"access_token":"repair"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := ReconcileConsoleCredentialRemovalsIn(root, func(string) (bool, error) { return tt.modelFound, nil })
			if tt.wantError == "" && err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if tt.wantError != "" && (err == nil || !strings.Contains(err.Error(), tt.wantError)) {
				t.Fatalf("reconcile error = %v, want %q", err, tt.wantError)
			}
			if tt.liveRepair {
				if _, err := os.Stat(livePath); err != nil {
					t.Fatalf("live repair was not preserved: %v", err)
				}
			}
			_, journalErr := os.Stat(consoleRemovalJournalPathIn(root, accountID))
			if tt.wantJournal && journalErr != nil {
				t.Fatalf("journal was not preserved: %v", journalErr)
			}
			if !tt.wantJournal && !errors.Is(journalErr, os.ErrNotExist) {
				t.Fatalf("completed journal remains: %v", journalErr)
			}
		})
	}
}

func TestConsoleRemovalReconciliationPreservesConcurrentEmptyLiveReplacement(t *testing.T) {
	root := t.TempDir()
	accountID := "qwen-token:restart-live-race"
	if err := SaveConsoleCredentialIn(root, accountID, ConsoleCredential{AccessToken: "original"}); err != nil {
		t.Fatal(err)
	}
	found, version, err := ConsoleCredentialVersionIn(root, accountID)
	if err != nil || !found {
		t.Fatalf("version = found %v version %q err %v", found, version, err)
	}
	liveDir := ConsoleConfigDirIn(root, accountID)
	stagedDir := liveDir + consoleRemovalStageSuffix
	if err := writeConsoleRemovalJournalUnlockedIn(root, accountID, true, version); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(liveDir, stagedDir); err != nil {
		t.Fatal(err)
	}
	err = ReconcileConsoleCredentialRemovalsIn(root, func(string) (bool, error) {
		// Recreate an empty live directory after reconciliation observed it as
		// absent but before it restores the staged profile.
		if err := os.Mkdir(liveDir, 0o700); err != nil {
			return false, err
		}
		return true, nil
	})
	if err == nil {
		t.Fatal("restart overwrote a concurrently recreated empty live directory")
	}
	entries, readErr := os.ReadDir(liveDir)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("live replacement changed: entries=%v err=%v", entries, readErr)
	}
	if _, err := os.Stat(filepath.Join(stagedDir, "config.json")); err != nil {
		t.Fatalf("exact staged profile was not preserved: %v", err)
	}
	if _, err := os.Stat(consoleRemovalJournalPathIn(root, accountID)); err != nil {
		t.Fatalf("fail-closed journal was not preserved: %v", err)
	}
}

func TestReadConsoleRemovalJournalVersionOneCompatibility(t *testing.T) {
	root := t.TempDir()
	accountID := "qwen-token:legacy-journal"
	digest := strings.Repeat("a", sha256.Size*2)
	path := consoleRemovalJournalPathIn(root, accountID)
	body := fmt.Sprintf(`{"version":1,"account_id":%q,"credential_version":%q}`, accountID, digest)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	journal, err := readConsoleRemovalJournal(path)
	if err != nil || !journal.CredentialFound || journal.CredentialVersion != digest {
		t.Fatalf("legacy journal = %+v err %v", journal, err)
	}
}

func TestExactConsoleRollbackReportsDurableRestoreFailure(t *testing.T) {
	root := t.TempDir()
	accountID := "qwen-token:work"
	if err := SaveConsoleCredentialIn(root, accountID, ConsoleCredential{AccessToken: "original"}); err != nil {
		t.Fatal(err)
	}
	found, version, err := ConsoleCredentialVersionIn(root, accountID)
	if err != nil || !found {
		t.Fatalf("version = found %v, err %v", found, err)
	}
	wantRemove := errors.New("account removal failed")
	wantSync := errors.New("restore sync failed")
	originalSync := syncConsoleDirectory
	call := 0
	syncConsoleDirectory = func(path string) error {
		call++
		// Journal publication and console staging each sync the parent before
		// model deletion. Fail the subsequent rollback publication.
		if call >= 3 {
			return wantSync
		}
		return originalSync(path)
	}
	t.Cleanup(func() { syncConsoleDirectory = originalSync })
	removed, err := RemoveConsoleCredentialExactIn(root, accountID, true, version, func() (bool, error) {
		return false, wantRemove
	})
	if removed || !errors.Is(err, wantRemove) || !errors.Is(err, wantSync) {
		t.Fatalf("rollback result = removed %v, err %v", removed, err)
	}
}

func TestExactConsoleRemovalDeletesInterruptedTokenlessLoginState(t *testing.T) {
	root := t.TempDir()
	accountID := "qwen-token:interrupted"
	if err := SaveConsoleCredentialIn(root, accountID, ConsoleCredential{
		AccessToken: "old-console-token",
		Account:     "owner@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if err := PrepareConsoleLoginIn(root, accountID, "temporary-model-api-key", "https://example.test/v1"); err != nil {
		t.Fatal(err)
	}
	config, err := readExistingRawConsoleConfigIn(root, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if token, _ := config["access_token"].(string); strings.TrimSpace(token) != "" {
		t.Fatalf("interrupted login unexpectedly retained access_token %q", token)
	}
	if config["api_key"] != "temporary-model-api-key" || config[previousAccessTokenKey] != "old-console-token" {
		t.Fatalf("interrupted login state = %#v", config)
	}
	found, version, err := ConsoleCredentialVersionIn(root, accountID)
	if err != nil || !found || version == "" {
		t.Fatalf("tokenless secret state version = found %v version %q err %v", found, version, err)
	}
	accountRemoved := false
	removed, err := RemoveConsoleCredentialExactIn(root, accountID, found, version, func() (bool, error) {
		accountRemoved = true
		return true, nil
	})
	if err != nil || !removed || !accountRemoved {
		t.Fatalf("interrupted login removal = removed %v account_removed %v err %v", removed, accountRemoved, err)
	}
	if _, err := os.Lstat(ConsoleConfigDirIn(root, accountID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted login secrets remain after removal: %v", err)
	}
}

func TestExactConsoleRemovalRestoresInterruptedTokenlessLoginState(t *testing.T) {
	root := t.TempDir()
	accountID := "qwen-token:interrupted-rollback"
	if err := SaveConsoleCredentialIn(root, accountID, ConsoleCredential{AccessToken: "old-console-token"}); err != nil {
		t.Fatal(err)
	}
	if err := PrepareConsoleLoginIn(root, accountID, "temporary-model-api-key", "https://example.test/v1"); err != nil {
		t.Fatal(err)
	}
	found, version, err := ConsoleCredentialVersionIn(root, accountID)
	if err != nil || !found {
		t.Fatalf("version = found %v err %v", found, err)
	}
	want := errors.New("model account removal failed")
	removed, err := RemoveConsoleCredentialExactIn(root, accountID, found, version, func() (bool, error) {
		return false, want
	})
	if removed || !errors.Is(err, want) {
		t.Fatalf("interrupted login rollback = removed %v err %v", removed, err)
	}
	config, readErr := readExistingRawConsoleConfigIn(root, accountID)
	if readErr != nil || config["api_key"] != "temporary-model-api-key" || config[previousAccessTokenKey] != "old-console-token" {
		t.Fatalf("restored interrupted state = %#v err %v", config, readErr)
	}
	if token, _ := config["access_token"].(string); strings.TrimSpace(token) != "" {
		t.Fatalf("rollback invented access_token %q", token)
	}
}

func TestExactConsoleRemovalDoesNotRestoreAfterCommittedModelDeletionError(t *testing.T) {
	root := t.TempDir()
	accountID := "qwen-token:committed-error"
	if err := SaveConsoleCredentialIn(root, accountID, ConsoleCredential{AccessToken: "console-secret"}); err != nil {
		t.Fatal(err)
	}
	found, version, err := ConsoleCredentialVersionIn(root, accountID)
	if err != nil || !found {
		t.Fatalf("version = found %v err %v", found, err)
	}
	want := errors.New("model deletion committed with cleanup error")
	removed, err := RemoveConsoleCredentialExactIn(root, accountID, found, version, func() (bool, error) {
		return true, want
	})
	if !removed || !errors.Is(err, want) {
		t.Fatalf("committed model deletion = removed %v err %v", removed, err)
	}
	if _, err := os.Lstat(ConsoleConfigDirIn(root, accountID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed model deletion restored console secret: %v", err)
	}
}

func TestExactConsoleRemovalRejectsMetadataOnlyReplacement(t *testing.T) {
	root := t.TempDir()
	accountID := "qwen-token:metadata-race"
	if err := SaveConsoleCredentialIn(root, accountID, ConsoleCredential{
		AccessToken: "console-secret",
		Account:     "original-account",
	}); err != nil {
		t.Fatal(err)
	}
	found, version, err := ConsoleCredentialVersionIn(root, accountID)
	if err != nil || !found {
		t.Fatalf("version = found %v err %v", found, err)
	}
	if err := SetConsoleAccountIn(root, accountID, "replacement-account"); err != nil {
		t.Fatal(err)
	}
	accountRemoved := false
	removed, err := RemoveConsoleCredentialExactIn(root, accountID, found, version, func() (bool, error) {
		accountRemoved = true
		return true, nil
	})
	if removed || accountRemoved || err == nil || !strings.Contains(err.Error(), "changed during removal") {
		t.Fatalf("metadata-raced removal = removed %v account_removed %v err %v", removed, accountRemoved, err)
	}
	credential, readErr := ExportConsoleCredentialIn(root, accountID)
	if readErr != nil || credential.AccessToken != "console-secret" {
		t.Fatalf("credential after metadata race = %+v err %v", credential, readErr)
	}
	if got := ConsoleAccountIn(root, accountID); got != "replacement-account" {
		t.Fatalf("account label after metadata race = %q", got)
	}
}

func TestExactConsoleRemovalDeletesMetadataOnlyProfile(t *testing.T) {
	root := t.TempDir()
	accountID := "qwen-token:metadata-only"
	if err := SetConsoleAccountIn(root, accountID, "owner@example.com"); err != nil {
		t.Fatal(err)
	}
	found, version, err := ConsoleCredentialVersionIn(root, accountID)
	if err != nil || !found || version == "" {
		t.Fatalf("metadata-only version = found %v version %q err %v", found, version, err)
	}
	accountRemoved := false
	removed, err := RemoveConsoleCredentialExactIn(root, accountID, found, version, func() (bool, error) {
		accountRemoved = true
		return true, nil
	})
	if err != nil || !removed || !accountRemoved {
		t.Fatalf("metadata-only removal = removed %v account_removed %v err %v", removed, accountRemoved, err)
	}
	if _, err := os.Lstat(ConsoleConfigDirIn(root, accountID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata-only profile remains after removal: %v", err)
	}
}

func TestExactConsoleRemovalRestoresMetadataOnlyProfileOnRollback(t *testing.T) {
	root := t.TempDir()
	accountID := "qwen-token:metadata-only-rollback"
	if err := SetConsoleAccountIn(root, accountID, "owner@example.com"); err != nil {
		t.Fatal(err)
	}
	found, version, err := ConsoleCredentialVersionIn(root, accountID)
	if err != nil || !found {
		t.Fatalf("metadata-only version = found %v err %v", found, err)
	}
	want := errors.New("model account changed")
	removed, err := RemoveConsoleCredentialExactIn(root, accountID, found, version, func() (bool, error) {
		return false, want
	})
	if removed || !errors.Is(err, want) {
		t.Fatalf("metadata-only rollback = removed %v err %v", removed, err)
	}
	if got := ConsoleAccountIn(root, accountID); got != "owner@example.com" {
		t.Fatalf("metadata-only account after rollback = %q", got)
	}
	if _, err := os.Stat(ConsoleConfigPathIn(root, accountID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback invented config.json: %v", err)
	}
}

func TestExactConsoleRemovalVersionsUnknownOnlyProfile(t *testing.T) {
	root := t.TempDir()
	accountID := "qwen-token:future-layout"
	dir := ConsoleConfigDirIn(root, accountID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	unknownPath := filepath.Join(dir, "future-credential.dat")
	if err := os.WriteFile(unknownPath, []byte("first-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	found, originalVersion, err := ConsoleCredentialVersionIn(root, accountID)
	if err != nil || !found || originalVersion == "" {
		t.Fatalf("unknown-only version = found %v version %q err %v", found, originalVersion, err)
	}
	if err := os.WriteFile(unknownPath, []byte("replacement-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	callbackCalled := false
	removed, err := RemoveConsoleCredentialExactIn(root, accountID, true, originalVersion, func() (bool, error) {
		callbackCalled = true
		return true, nil
	})
	if removed || callbackCalled || err == nil || !strings.Contains(err.Error(), "changed during removal") {
		t.Fatalf("mutated unknown-only removal = removed %v callback %v err %v", removed, callbackCalled, err)
	}
	body, readErr := os.ReadFile(unknownPath)
	if readErr != nil || string(body) != "replacement-secret" {
		t.Fatalf("replacement unknown file = %q err %v", body, readErr)
	}
	found, replacementVersion, err := ConsoleCredentialVersionIn(root, accountID)
	if err != nil || !found || replacementVersion == originalVersion {
		t.Fatalf("replacement version = found %v version %q err %v", found, replacementVersion, err)
	}
	removed, err = RemoveConsoleCredentialExactIn(root, accountID, true, replacementVersion, func() (bool, error) {
		return true, nil
	})
	if err != nil || !removed {
		t.Fatalf("exact unknown-only removal = removed %v err %v", removed, err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unknown-only directory remains after removal: %v", err)
	}
}

func TestExactConsoleRemovalVersionsAndRestoresNestedContent(t *testing.T) {
	root := t.TempDir()
	accountID := "qwen-token:nested-layout"
	dir := ConsoleConfigDirIn(root, accountID)
	nestedPath := filepath.Join(dir, "future", "tokens", "credential.bin")
	if err := os.MkdirAll(filepath.Dir(nestedPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nestedPath, []byte("nested-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	found, version, err := ConsoleCredentialVersionIn(root, accountID)
	if err != nil || !found {
		t.Fatalf("nested version = found %v version %q err %v", found, version, err)
	}
	want := errors.New("model account changed")
	removed, err := RemoveConsoleCredentialExactIn(root, accountID, true, version, func() (bool, error) {
		return false, want
	})
	if removed || !errors.Is(err, want) {
		t.Fatalf("nested rollback = removed %v err %v", removed, err)
	}
	body, readErr := os.ReadFile(nestedPath)
	if readErr != nil || string(body) != "nested-secret" {
		t.Fatalf("nested content after rollback = %q err %v", body, readErr)
	}
	if err := os.WriteFile(nestedPath, []byte("changed-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	callbackCalled := false
	removed, err = RemoveConsoleCredentialExactIn(root, accountID, true, version, func() (bool, error) {
		callbackCalled = true
		return true, nil
	})
	if removed || callbackCalled || err == nil || !strings.Contains(err.Error(), "changed during removal") {
		t.Fatalf("mutated nested removal = removed %v callback %v err %v", removed, callbackCalled, err)
	}
}

func TestConsoleCredentialVersionRejectsSymbolicLink(t *testing.T) {
	root := t.TempDir()
	accountID := "qwen-token:unsafe-link"
	dir := ConsoleConfigDirIn(root, accountID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside-secret")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "linked-secret")); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	found, version, err := ConsoleCredentialVersionIn(root, accountID)
	if err == nil || found || version != "" || !strings.Contains(err.Error(), "unsafe symbolic link") {
		t.Fatalf("unsafe-link version = found %v version %q err %v", found, version, err)
	}
}
