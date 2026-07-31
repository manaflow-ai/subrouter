//go:build !windows

package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

func TestAccountImportCannotBeOverwrittenByConcurrentReload(t *testing.T) {
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
	codexStore := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	for index := 0; index < maxAccountImportAccounts-1; index++ {
		account := accounts.StoredCodexAccount{
			Email:    fmt.Sprintf("apikey:seed-%03d", index),
			Provider: accounts.ProviderCodex,
			Auth: accounts.CodexAuthFile{
				AuthMode:     "apikey",
				OpenAIAPIKey: fmt.Sprintf("sk-seed-%03d", index),
			},
		}
		if err := codexStore.SaveStored(account); err != nil {
			t.Fatal(err)
		}
	}
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
