//go:build !windows

package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
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
