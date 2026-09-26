//go:build !windows

package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	agentkimi "github.com/manaflow-ai/subrouter/internal/agents/kimi"
	"github.com/manaflow-ai/subrouter/internal/tenant"
)

func TestAccountImportCannotBeOverwrittenByConcurrentReload(t *testing.T) {
	t.Parallel()
	codexStore := accounts.CodexStore{Dir: t.TempDir()}
	seed := accounts.StoredCodexAccount{
		Email:    "apikey:seed",
		Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-seed",
		},
	}
	if err := codexStore.SaveStored(seed); err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	server := Server{AccountRef: ref, AdminToken: "secret"}
	handler := server.Handler()

	fifoPath := filepath.Join(codexStore.Dir, "zz-block-reload.json")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatal(err)
	}
	writerReady := make(chan *os.File, 1)
	writerError := make(chan error, 1)
	go func() {
		writer, err := os.OpenFile(fifoPath, os.O_WRONLY, 0)
		if err != nil {
			writerError <- err
			return
		}
		writerReady <- writer
	}()

	reloadDone := make(chan error, 1)
	go func() {
		_, _, err := server.reloadAccounts(context.Background())
		reloadDone <- err
	}()
	var writer *os.File
	select {
	case writer = <-writerReady:
	case err := <-writerError:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent reload did not reach the blocking store entry")
	}
	if err := os.Remove(fifoPath); err != nil {
		t.Fatal(err)
	}

	imported := accounts.StoredCodexAccount{
		Email:    "apikey:imported",
		Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-imported",
		},
	}
	payload, err := json.Marshal(map[string]any{"provider": "codex", "codex": imported})
	if err != nil {
		t.Fatal(err)
	}
	importDone := make(chan *http.Response, 1)
	go func() {
		importDone <- serveProtectedAccountImport(handler, payload).Result()
	}()
	var importResponse *http.Response
	select {
	case importResponse = <-importDone:
	case <-time.After(time.Second):
	}

	staleSnapshotEntry, err := json.Marshal(accounts.StoredCodexAccount{
		Email:    "apikey:fifo-snapshot",
		Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-fifo-snapshot",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(staleSnapshotEntry); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-reloadDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent reload remained blocked")
	}
	if importResponse == nil {
		select {
		case importResponse = <-importDone:
		case <-time.After(5 * time.Second):
			t.Fatal("account import remained blocked after reload completed")
		}
	}
	if importResponse.StatusCode != http.StatusOK {
		t.Fatalf("import status = %d, want 200", importResponse.StatusCode)
	}

	for _, account := range ref.All() {
		if account.Email == imported.Email {
			return
		}
	}
	t.Fatalf("concurrent reload removed imported account from memory: %+v", ref.All())
}

func TestConcurrentWorkerGenerationImportsShareCapacityLimit(t *testing.T) {
	t.Parallel()
	codexStore := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	seedCodexAPIKeyAccounts(t, codexStore, maxAccountImportAccounts-1)
	claudeStore := agentclaude.Store{Dir: codexStore.StoreDir()}
	newWorkerRef := NewAccountRef(codexStore, nil, nil)
	newWorkerRef.claudeStore = claudeStore
	retiringWorkerRef := NewAccountRef(codexStore, nil, nil)
	retiringWorkerRef.claudeStore = claudeStore
	handlers := []http.Handler{
		Server{AccountRef: newWorkerRef, AdminToken: "secret"}.Handler(),
		Server{AccountRef: retiringWorkerRef, AdminToken: "secret"}.Handler(),
	}

	lockPath := filepath.Join(codexStore.StoreDir(), ".account-import.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	responses := make(chan int, len(handlers))
	for index, handler := range handlers {
		account := accounts.StoredCodexAccount{
			Email:    fmt.Sprintf("apikey:concurrent-%d", index),
			Provider: accounts.ProviderCodex,
			Auth: accounts.CodexAuthFile{
				AuthMode:     "apikey",
				OpenAIAPIKey: fmt.Sprintf("sk-concurrent-%d", index),
			},
		}
		payload, err := json.Marshal(map[string]any{"provider": "codex", "codex": account})
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			responses <- serveProtectedAccountImport(handler, payload).Code
		}()
	}
	select {
	case status := <-responses:
		t.Fatalf("worker import bypassed the shared transaction lock with status %d", status)
	case <-time.After(100 * time.Millisecond):
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}

	statuses := make([]int, 0, len(handlers))
	for range handlers {
		select {
		case status := <-responses:
			statuses = append(statuses, status)
		case <-time.After(5 * time.Second):
			t.Fatal("worker import remained blocked after transaction unlock")
		}
	}
	sort.Ints(statuses)
	want := []int{http.StatusOK, http.StatusInsufficientStorage}
	sort.Ints(want)
	if !slices.Equal(statuses, want) {
		t.Fatalf("concurrent import statuses = %v, want %v", statuses, want)
	}
	stored, err := codexStore.ListStored()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != maxAccountImportAccounts {
		t.Fatalf("concurrent imports stored %d accounts, want %d", len(stored), maxAccountImportAccounts)
	}
}

func TestConcurrentWorkerKimiImportsShareFreshAllProviderCapacity(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	codexStore := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	seedCodexAPIKeyAccounts(t, codexStore, maxAccountImportAccounts-1)
	claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
	kimiStore := agentkimi.Store{
		Path:       filepath.Join(root, "kimi", "cli.json"),
		ManagedDir: filepath.Join(root, "kimi", "managed"),
	}
	refs := []*AccountRef{
		NewAccountRef(codexStore, nil, nil),
		NewAccountRef(codexStore, nil, nil),
	}
	handlers := make([]http.Handler, 0, len(refs))
	for _, ref := range refs {
		ref.claudeStore = claudeStore
		ref.oauthSources = []OAuthAccountSource{kimiStore}
		handlers = append(handlers, Server{AccountRef: ref, AdminToken: "secret"}.Handler())
	}

	lockPath := filepath.Join(codexStore.StoreDir(), ".account-import.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	responses := make(chan int, len(handlers))
	for index, handler := range handlers {
		input := accountImportRequest{
			Provider: accounts.ProviderKimi,
			Kimi: &kimiAccountImport{
				Label: fmt.Sprintf("kimi-%d", index),
				Credential: agentkimi.CredentialInfo{
					AccessToken: "access", RefreshToken: "refresh",
					OAuthDeviceID: fmt.Sprintf("device-%d", index), ExpiresAt: time.Now().Add(time.Hour),
				},
			},
		}
		payload, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			responses <- serveProtectedAccountImport(handler, payload).Code
		}()
	}
	select {
	case status := <-responses:
		t.Fatalf("worker import bypassed the shared transaction lock with status %d", status)
	case <-time.After(100 * time.Millisecond):
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}

	statuses := make([]int, 0, len(handlers))
	for range handlers {
		select {
		case status := <-responses:
			statuses = append(statuses, status)
		case <-time.After(5 * time.Second):
			t.Fatal("worker Kimi import remained blocked after transaction unlock")
		}
	}
	sort.Ints(statuses)
	want := []int{http.StatusOK, http.StatusInsufficientStorage}
	sort.Ints(want)
	if !slices.Equal(statuses, want) {
		t.Fatalf("concurrent Kimi import statuses = %v, want %v", statuses, want)
	}
	managed, err := kimiStore.ListAccounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(managed) != 1 {
		t.Fatalf("concurrent imports stored %d Kimi accounts, want 1", len(managed))
	}
}

func TestKimiLogicalAliasesConflictBeforeMutation(t *testing.T) {
	t.Parallel()
	for _, full := range []bool{false, true} {
		capacity := "below capacity"
		if full {
			capacity = "full capacity"
		}
		for _, symlink := range []bool{false, true} {
			aliasKind := "noncanonical filename"
			if symlink {
				aliasKind = "noncanonical symlink"
			}
			t.Run(capacity+"/"+aliasKind, func(t *testing.T) {
				root := t.TempDir()
				codexStore := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
				storedAccounts := 0
				if full {
					storedAccounts = maxAccountImportAccounts - 1
				}
				seedCodexAPIKeyAccounts(t, codexStore, storedAccounts)
				claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
				kimiStore := agentkimi.Store{
					Path:       filepath.Join(root, "kimi", "cli.json"),
					ManagedDir: filepath.Join(root, "kimi", "managed"),
				}
				credential := agentkimi.CredentialInfo{
					AccessToken: "access", RefreshToken: "refresh",
					OAuthDeviceID: "device", ExpiresAt: time.Now().Add(time.Hour),
				}
				if _, err := kimiStore.SaveManagedCredential("work", credential); err != nil {
					t.Fatal(err)
				}
				entries, err := os.ReadDir(kimiStore.ManagedDir)
				if err != nil || len(entries) != 1 {
					t.Fatalf("canonical fixture entries = %d, err = %v", len(entries), err)
				}
				canonicalPath := filepath.Join(kimiStore.ManagedDir, entries[0].Name())
				aliasName := base64.RawURLEncoding.EncodeToString([]byte("WORK")) + ".json"
				aliasPath := filepath.Join(kimiStore.ManagedDir, aliasName)
				if symlink {
					target := filepath.Join(root, "credential-target.json")
					if err := os.Rename(canonicalPath, target); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, aliasPath); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Rename(canonicalPath, aliasPath); err != nil {
					t.Fatal(err)
				}
				originalCredential, err := os.ReadFile(aliasPath)
				if err != nil {
					t.Fatal(err)
				}
				logical, err := kimiStore.ListAccounts(t.Context())
				if err == nil || len(logical) != 0 {
					t.Fatalf("logical alias fixture = %+v, err = %v", logical, err)
				}
				if exists, err := kimiStore.ManagedAccountExists("work"); err != nil || exists {
					t.Fatalf("canonical path exists before import: exists=%v err=%v", exists, err)
				}
				ref := NewAccountRef(codexStore, nil, nil)
				ref.claudeStore = claudeStore
				ref.oauthSources = []OAuthAccountSource{kimiStore}
				if _, _, err := ref.ReloadSnapshot(); err != nil {
					t.Fatal(err)
				}
				handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
				payload, err := json.Marshal(accountImportRequest{
					Provider: accounts.ProviderKimi,
					Kimi: &kimiAccountImport{
						Label: "work", Credential: credential,
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				beforeGeneration, err := readAccountDiskGeneration(codexStore.StoreDir())
				if err != nil {
					t.Fatal(err)
				}
				response := serveProtectedAccountImport(handler, payload)
				if response.Code != http.StatusConflict {
					t.Fatalf("status = %d, want 409, body = %s", response.Code, response.Body.String())
				}
				afterGeneration, err := readAccountDiskGeneration(codexStore.StoreDir())
				if err != nil {
					t.Fatal(err)
				}
				if afterGeneration != beforeGeneration {
					t.Fatal("alias conflict published an account generation")
				}
				if exists, err := kimiStore.ManagedAccountExists("work"); err != nil || exists {
					t.Fatalf("rejected import created canonical path: exists=%v err=%v", exists, err)
				}
				entries, err = os.ReadDir(kimiStore.ManagedDir)
				if err != nil || len(entries) != 1 {
					t.Fatalf("managed entries after conflict = %d, err = %v", len(entries), err)
				}
				afterCredential, err := os.ReadFile(aliasPath)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(afterCredential, originalCredential) {
					t.Fatal("alias conflict changed the original credential")
				}
				logical, err = kimiStore.ListAccounts(t.Context())
				if err == nil || len(logical) != 0 {
					t.Fatalf("logical accounts after conflict = %+v, err = %v", logical, err)
				}
				routed := 0
				for _, account := range ref.All() {
					if account.Provider == accounts.ProviderKimi && account.ID == "kimi-subscription:work" {
						routed++
					}
				}
				if routed != 0 {
					t.Fatalf("routed Kimi aliases after conflict = %d, want 0", routed)
				}
			})
		}
	}
}

func TestUnreadableKimiLogicalAliasesConflictBeforeMutation(t *testing.T) {
	t.Parallel()
	for _, full := range []bool{false, true} {
		capacity := "below capacity"
		if full {
			capacity = "full capacity"
		}
		for _, dangling := range []bool{false, true} {
			aliasKind := "malformed file"
			if dangling {
				aliasKind = "dangling symlink"
			}
			t.Run(capacity+"/"+aliasKind, func(t *testing.T) {
				root := t.TempDir()
				codexStore := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
				storedAccounts := 0
				if full {
					storedAccounts = maxAccountImportAccounts - 1
				}
				seedCodexAPIKeyAccounts(t, codexStore, storedAccounts)
				claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
				kimiStore := agentkimi.Store{
					Path:       filepath.Join(root, "kimi", "cli.json"),
					ManagedDir: filepath.Join(root, "kimi", "managed"),
				}
				if err := os.MkdirAll(kimiStore.ManagedDir, 0o700); err != nil {
					t.Fatal(err)
				}
				aliasName := base64.RawURLEncoding.EncodeToString([]byte("WORK")) + ".json"
				aliasPath := filepath.Join(kimiStore.ManagedDir, aliasName)
				malformed := []byte("{not-json")
				danglingTarget := filepath.Join(root, "missing-credential.json")
				if dangling {
					if err := os.Symlink(danglingTarget, aliasPath); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(aliasPath, malformed, 0o600); err != nil {
					t.Fatal(err)
				}
				ids, err := kimiStore.AccountInventoryIDs(t.Context())
				if err != nil || len(ids) != 1 || ids[0] != "kimi-subscription:work" {
					t.Fatalf("durable alias IDs = %v, err = %v", ids, err)
				}
				logical, err := kimiStore.ListAccounts(t.Context())
				if err == nil || len(logical) != 0 {
					t.Fatalf("unreadable alias accounts = %+v, err = %v", logical, err)
				}
				if exists, err := kimiStore.ManagedAccountExists("work"); err != nil || exists {
					t.Fatalf("canonical path exists before import: exists=%v err=%v", exists, err)
				}
				ref := NewAccountRef(codexStore, nil, nil)
				ref.claudeStore = claudeStore
				ref.oauthSources = []OAuthAccountSource{kimiStore}
				if _, _, err := ref.ReloadSnapshot(); err != nil {
					t.Fatal(err)
				}
				handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
				payload, err := json.Marshal(accountImportRequest{
					Provider: accounts.ProviderKimi,
					Kimi: &kimiAccountImport{
						Label: "work",
						Credential: agentkimi.CredentialInfo{
							AccessToken: "replacement", RefreshToken: "refresh",
							OAuthDeviceID: "device", ExpiresAt: time.Now().Add(time.Hour),
						},
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				beforeGeneration, err := readAccountDiskGeneration(codexStore.StoreDir())
				if err != nil {
					t.Fatal(err)
				}
				response := serveProtectedAccountImport(handler, payload)
				if response.Code != http.StatusConflict {
					t.Fatalf("status = %d, want 409, body = %s", response.Code, response.Body.String())
				}
				afterGeneration, err := readAccountDiskGeneration(codexStore.StoreDir())
				if err != nil {
					t.Fatal(err)
				}
				if afterGeneration != beforeGeneration {
					t.Fatal("unreadable alias conflict published an account generation")
				}
				if exists, err := kimiStore.ManagedAccountExists("work"); err != nil || exists {
					t.Fatalf("conflict created canonical path: exists=%v err=%v", exists, err)
				}
				entries, err := os.ReadDir(kimiStore.ManagedDir)
				if err != nil || len(entries) != 1 {
					t.Fatalf("managed entries after conflict = %d, err = %v", len(entries), err)
				}
				if dangling {
					target, err := os.Readlink(aliasPath)
					if err != nil || target != danglingTarget {
						t.Fatalf("dangling alias changed: target=%q err=%v", target, err)
					}
				} else {
					after, err := os.ReadFile(aliasPath)
					if err != nil || !bytes.Equal(after, malformed) {
						t.Fatalf("malformed alias changed: body=%q err=%v", after, err)
					}
				}
				ids, err = kimiStore.AccountInventoryIDs(t.Context())
				if err != nil || len(ids) != 1 || ids[0] != "kimi-subscription:work" {
					t.Fatalf("durable alias IDs after conflict = %v, err = %v", ids, err)
				}
				for _, account := range ref.All() {
					if account.Provider == accounts.ProviderKimi && account.ID == "kimi-subscription:work" {
						t.Fatalf("unreadable alias became routable after conflict: %+v", account)
					}
				}
			})
		}
	}
}

func TestCanonicalDanglingKimiAccountRemovalReconcilesState(t *testing.T) {
	root := t.TempDir()
	codexStore := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	kimiStore := agentkimi.Store{
		Path: filepath.Join(root, "kimi", "cli.json"), ManagedDir: filepath.Join(root, "kimi", "managed"),
	}
	credential := agentkimi.CredentialInfo{
		AccessToken: "access", RefreshToken: "refresh", OAuthDeviceID: "device", ExpiresAt: time.Now().Add(time.Hour),
	}
	if _, err := kimiStore.SaveManagedCredential("work", credential); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(kimiStore.ManagedDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("canonical fixture entries = %d, err = %v", len(entries), err)
	}
	canonicalPath := filepath.Join(kimiStore.ManagedDir, entries[0].Name())
	if err := os.Remove(canonicalPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "missing.json"), canonicalPath); err != nil {
		t.Fatal(err)
	}
	if count, err := kimiStore.AccountInventoryCount(t.Context()); err != nil || count != 1 {
		t.Fatalf("inventory before removal = %d, err = %v", count, err)
	}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: filepath.Join(root, "claude")}
	ref.oauthSources = []OAuthAccountSource{kimiStore}
	if _, _, err := ref.ReloadSnapshot(); err != nil {
		t.Fatal(err)
	}
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
	payload, err := json.Marshal(accountImportRequest{
		Provider: accounts.ProviderKimi,
		Kimi:     &kimiAccountImport{Label: "work", Remove: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	beforeGeneration, err := readAccountDiskGeneration(codexStore.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	beforeLiveGeneration := ref.Generation()
	response := serveProtectedAccountImport(handler, payload)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", response.Code, response.Body.String())
	}
	if _, err := os.Lstat(canonicalPath); !os.IsNotExist(err) {
		t.Fatalf("canonical dangling link remains after removal: %v", err)
	}
	if count, err := kimiStore.AccountInventoryCount(t.Context()); err != nil || count != 0 {
		t.Fatalf("inventory after removal = %d, err = %v", count, err)
	}
	afterGeneration, err := readAccountDiskGeneration(codexStore.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	if afterGeneration == beforeGeneration || ref.Generation() <= beforeLiveGeneration {
		t.Fatalf("generation after removal: disk=%q live=%d before_disk=%q before_live=%d", afterGeneration, ref.Generation(), beforeGeneration, beforeLiveGeneration)
	}
	for _, account := range ref.All() {
		if account.Provider == accounts.ProviderKimi && account.ID == "kimi-subscription:work" {
			t.Fatalf("removed dangling account remains routed: %+v", account)
		}
	}
}

func TestFullCapacityCanonicalKimiRepair(t *testing.T) {
	t.Parallel()
	for _, dangling := range []bool{false, true} {
		name := "malformed file"
		if dangling {
			name = "dangling symlink"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			codexStore := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
			seedCodexAPIKeyAccounts(t, codexStore, maxAccountImportAccounts-1)
			kimiStore := agentkimi.Store{
				Path: filepath.Join(root, "kimi", "cli.json"), ManagedDir: filepath.Join(root, "kimi", "managed"),
			}
			fixtureCredential := agentkimi.CredentialInfo{
				AccessToken: "old", RefreshToken: "old-refresh", OAuthDeviceID: "old-device", ExpiresAt: time.Now().Add(time.Hour),
			}
			if _, err := kimiStore.SaveManagedCredential("work", fixtureCredential); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(kimiStore.ManagedDir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("canonical fixture entries = %d, err = %v", len(entries), err)
			}
			canonicalPath := filepath.Join(kimiStore.ManagedDir, entries[0].Name())
			if err := os.Remove(canonicalPath); err != nil {
				t.Fatal(err)
			}
			if dangling {
				if err := os.Symlink(filepath.Join(root, "missing.json"), canonicalPath); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(canonicalPath, []byte("{not-json"), 0o600); err != nil {
				t.Fatal(err)
			}
			if count, err := kimiStore.AccountInventoryCount(t.Context()); err != nil || count != 1 {
				t.Fatalf("Kimi inventory before repair = %d, err = %v", count, err)
			}
			ref := NewAccountRef(codexStore, nil, nil)
			ref.claudeStore = agentclaude.Store{Dir: filepath.Join(root, "claude")}
			ref.oauthSources = []OAuthAccountSource{kimiStore}
			if _, _, err := ref.ReloadSnapshot(); err != nil {
				t.Fatal(err)
			}
			handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
			replacement := agentkimi.CredentialInfo{
				AccessToken: "new", RefreshToken: "new-refresh", OAuthDeviceID: "new-device", ExpiresAt: time.Now().Add(time.Hour),
			}
			payload, err := json.Marshal(accountImportRequest{
				Provider: accounts.ProviderKimi,
				Kimi:     &kimiAccountImport{Label: "work", Credential: replacement},
			})
			if err != nil {
				t.Fatal(err)
			}
			beforeGeneration, err := readAccountDiskGeneration(codexStore.StoreDir())
			if err != nil {
				t.Fatal(err)
			}
			beforeLiveGeneration := ref.Generation()
			response := serveProtectedAccountImport(handler, payload)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body = %s", response.Code, response.Body.String())
			}
			stored, ok, err := kimiStore.ReadManagedCredential("work", time.Now())
			if err != nil || !ok || stored.AccessToken != replacement.AccessToken {
				t.Fatalf("repaired credential = %+v, ok=%v err=%v", stored, ok, err)
			}
			info, err := os.Lstat(canonicalPath)
			if err != nil || info.Mode()&os.ModeSymlink != 0 {
				t.Fatalf("canonical repair did not leave a regular credential: info=%v err=%v", info, err)
			}
			if count, err := kimiStore.AccountInventoryCount(t.Context()); err != nil || count != 1 {
				t.Fatalf("Kimi inventory after repair = %d, err = %v", count, err)
			}
			afterGeneration, err := readAccountDiskGeneration(codexStore.StoreDir())
			if err != nil {
				t.Fatal(err)
			}
			if afterGeneration == beforeGeneration || ref.Generation() <= beforeLiveGeneration {
				t.Fatalf("generation after repair: disk=%q live=%d before_disk=%q before_live=%d", afterGeneration, ref.Generation(), beforeGeneration, beforeLiveGeneration)
			}
			routed := 0
			for _, account := range ref.All() {
				if account.Provider == accounts.ProviderKimi && account.ID == "kimi-subscription:work" {
					routed++
				}
			}
			if routed != 1 {
				t.Fatalf("routed repaired Kimi accounts = %d, want 1", routed)
			}
		})
	}
}

func TestAccountRefStartupSnapshotWaitsForImportTransaction(t *testing.T) {
	t.Parallel()
	codexStore := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	seed := accounts.StoredCodexAccount{
		Email:    "apikey:seed",
		Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-seed",
		},
	}
	if err := codexStore.SaveStored(seed); err != nil {
		t.Fatal(err)
	}
	initial, err := codexStore.List()
	if err != nil {
		t.Fatal(err)
	}

	transactionLock, err := lockAccountImportTransaction(context.Background(), codexStore.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := advanceAccountDiskGeneration(codexStore.StoreDir()); err != nil {
		_ = transactionLock.Close()
		t.Fatal(err)
	}

	refReady := make(chan *AccountRef, 1)
	go func() {
		refReady <- NewAccountRef(codexStore, initial, nil)
	}()
	var ref *AccountRef
	select {
	case ref = <-refReady:
		// The old constructor returned here after pairing the stale account list
		// with the new disk marker. Keep going so the final assertion captures it.
	case <-time.After(100 * time.Millisecond):
	}

	imported := accounts.StoredCodexAccount{
		Email:    "apikey:imported",
		Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-imported",
		},
	}
	if err := codexStore.SaveStored(imported); err != nil {
		_ = transactionLock.Close()
		t.Fatal(err)
	}
	if err := transactionLock.Close(); err != nil {
		t.Fatal(err)
	}
	if ref == nil {
		select {
		case ref = <-refReady:
		case <-time.After(5 * time.Second):
			t.Fatal("account reference startup remained blocked after import completed")
		}
	}

	if !slices.ContainsFunc(ref.All(), func(account accounts.Account) bool {
		return account.ID == imported.Email
	}) {
		t.Fatalf("startup snapshot omitted account imported in the same transaction: %+v", ref.All())
	}
}

func TestAccountImportTransactionLockHonorsCancellation(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "accounts")
	held, err := lockAccountImportTransaction(context.Background(), storeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if lock, err := lockAccountImportTransaction(ctx, storeDir); !errors.Is(err, context.Canceled) {
		if lock != nil {
			_ = lock.Close()
		}
		t.Fatalf("contended lock error = %v, want context canceled", err)
	}
}

func TestBlockedTenantInitializationDoesNotBlockAnotherTenant(t *testing.T) {
	registry := tenant.NewRegistry(t.TempDir())
	blockedTenant, _, err := registry.Create("blocked")
	if err != nil {
		t.Fatal(err)
	}
	healthyTenant, _, err := registry.Create("healthy")
	if err != nil {
		t.Fatal(err)
	}
	blockedStoreDir := filepath.Join(registry.Dir(blockedTenant.ID), "codex", "accounts")
	held, err := lockAccountImportTransaction(context.Background(), accounts.CodexStore{Dir: blockedStoreDir}.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	multi := &MultiTenant{Base: Server{}, Registry: registry}
	blockedBaseCtx, cancelBlocked := context.WithCancel(context.Background())
	defer cancelBlocked()
	blockedCtx := &lockReachedContext{Context: blockedBaseCtx, reached: make(chan struct{})}
	blockedDone := make(chan error, 1)
	go func() {
		_, err := multi.handlerFor(blockedCtx, blockedTenant)
		blockedDone <- err
	}()
	// Start the healthy tenant only after the blocked tenant reaches the
	// filesystem lock. It must still initialize because no registry-wide mutex
	// is held around OpenAccountRefContext.
	select {
	case <-blockedCtx.reached:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked tenant did not reach the account transaction lock")
	}
	healthyDone := make(chan error, 1)
	go func() {
		_, err := multi.handlerFor(context.Background(), healthyTenant)
		healthyDone <- err
	}()
	select {
	case err := <-healthyDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		cancelBlocked()
		<-blockedDone
		t.Fatal("one tenant's account lock blocked another tenant's initialization")
	}
	cancelBlocked()
	if err := <-blockedDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked tenant error = %v, want context canceled", err)
	}
}

type lockReachedContext struct {
	context.Context
	once    sync.Once
	reached chan struct{}
}

func (c *lockReachedContext) Err() error {
	c.once.Do(func() { close(c.reached) })
	return c.Context.Err()
}
