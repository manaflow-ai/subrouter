//go:build !windows

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/proxy"
)

func TestLocalServingStoreBindingSelectsSeparateDaemonState(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, ".subrouter", "codex", "accounts")}
	stateDir := filepath.Join(home, "candidate-state")
	if err := os.MkdirAll(filepath.Join(stateDir, "codex", "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeLocalServingStoreBinding(t, store, stateDir)

	got, err := localServingStore(store)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(stateDir, "codex", "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Dir != want {
		t.Fatalf("serving store = %q, want %q", got.Dir, want)
	}
}

func TestLocalServingStoreExplicitStateWinsOverBinding(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, "explicit", "codex", "accounts")}
	stateDir := filepath.Join(home, "candidate-state")
	if err := os.MkdirAll(filepath.Join(stateDir, "codex", "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeLocalServingStoreBinding(t, store, stateDir)
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(home, "explicit"))
	if err := os.Remove(localServingStoreBindingPath(store)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "missing"), localServingStoreBindingPath(store)); err != nil {
		t.Fatal(err)
	}

	got, err := localServingStore(store)
	if err != nil {
		t.Fatal(err)
	}
	if got.Dir != store.Dir {
		t.Fatalf("explicit serving store = %q, want %q", got.Dir, store.Dir)
	}
}

func TestLocalServingStoreBindingCommandsRejectExplicitState(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, "explicit", "codex", "accounts")}
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(home, "explicit"))
	if err := bindLocalServingStore(filepath.Join(home, "candidate"), store, io.Discard); err == nil || !strings.Contains(err.Error(), "must run with SUBROUTER_STATE_DIR unset") {
		t.Fatalf("bind with explicit state error = %v", err)
	}
	if err := unbindLocalServingStore(store, io.Discard); err == nil || !strings.Contains(err.Error(), "must run with SUBROUTER_STATE_DIR unset") {
		t.Fatalf("unbind with explicit state error = %v", err)
	}
}

func TestLegacyV1ServingBindingFailsClosedForCredentialChannel(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, ".subrouter", "codex", "accounts")}
	stateA := filepath.Join(home, "state-a")
	stateB := filepath.Join(home, "state-b")
	for _, state := range []string{stateA, stateB} {
		if err := os.MkdirAll(filepath.Join(state, "codex", "accounts"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeLocalServingStoreBinding(t, store, stateA)
	storeA, err := localServingStore(store)
	if err != nil {
		t.Fatal(err)
	}
	writeLocalServingStoreBinding(t, store, stateB)
	writeLocalServingStoreBinding(t, store, stateA)
	var active atomic.Value
	active.Store(storeA)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/_subrouter/health" {
			writeLocalStoreAuthorityHealth(t, w, request, active.Load().(accounts.CodexStore), "enabled")
			return
		}
		if request.URL.Path == "/protected" {
			w.Header().Set("Connection", "close")
			_, _ = io.WriteString(w, "ok")
			return
		}
		http.NotFound(w, request)
	}))
	defer server.Close()
	client, err := newLocalDataClientWithResolver(server.Client(), server.URL, localServingStoreResolver(store))
	if err != nil {
		t.Fatal(err)
	}
	if _, requestErr := client.Get(server.URL + "/protected"); requestErr == nil || !strings.Contains(requestErr.Error(), "published private local data socket") {
		t.Fatalf("legacy v1 request error = %v, want fail-closed socket requirement", requestErr)
	}
}

func TestLocalServingRelayUsesLegacyAttestationForV1Binding(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, ".subrouter", "codex", "accounts")}
	stateDir := filepath.Join(home, "legacy-state")
	if err := os.MkdirAll(filepath.Join(stateDir, "codex", "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeLocalServingStoreBinding(t, store, stateDir)
	servingStore, err := localServingStore(store)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/_subrouter/health" {
			writeLocalStoreAuthorityHealth(t, w, request, servingStore, "enabled")
			return
		}
		if request.URL.Path == "/protected" {
			_, _ = io.WriteString(w, "ok")
			return
		}
		http.NotFound(w, request)
	}))
	defer server.Close()

	transport, err := localServingRelayTransport(server.URL, store)
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Transport: transport}).Get(server.URL + "/protected")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("legacy relay status = %d", response.StatusCode)
	}
}

func TestLocalServingStoreRejectsUnsafeBinding(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, ".subrouter", "codex", "accounts")}
	if err := os.MkdirAll(store.StoreDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	path := localServingStoreBindingPath(store)
	if err := os.WriteFile(path, []byte(`{"schema":"subrouter.local-serving-store/v1","state_dir":"relative"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := localServingStore(store); err == nil || !strings.Contains(err.Error(), "private regular file") {
		t.Fatalf("unsafe binding error = %v", err)
	}
}

func TestLocalServingStoreRejectsSchemaSocketMismatch(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, ".subrouter", "codex", "accounts")}
	accountsDir := filepath.Join(home, "served", "codex", "accounts")
	if err := os.MkdirAll(accountsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(localServingStoreBindingPath(store)), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		body string
	}{
		{"v1 with socket", fmt.Sprintf(`{"schema":%q,"accounts_dir":%q,"local_data_socket":"/private/tmp/data.sock"}`, localServingStoreSchemaV1, accountsDir)},
		{"v2 without socket", fmt.Sprintf(`{"schema":%q,"accounts_dir":%q}`, localServingStoreSchema, accountsDir)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(localServingStoreBindingPath(store), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := localServingStore(store); err == nil || !strings.Contains(err.Error(), "schema/socket") {
				t.Fatalf("mismatch error = %v", err)
			}
		})
	}
}

func TestLocalServingStoreMissingBindingPreservesDefault(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), ".subrouter", "codex", "accounts")}
	got, err := localServingStore(store)
	if err != nil {
		t.Fatal(err)
	}
	if got.Dir != store.Dir {
		t.Fatalf("serving store = %q, want default %q", got.Dir, store.Dir)
	}
}

func TestLocalServingStoreRejectsInvalidBindings(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, ".subrouter", "codex", "accounts")}
	if err := os.MkdirAll(store.StoreDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	path := localServingStoreBindingPath(store)
	validState := filepath.Join(home, "valid-state")
	if err := os.MkdirAll(filepath.Join(validState, "codex", "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	validAccounts, err := filepath.EvalSymlinks(filepath.Join(validState, "codex", "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.StoreAuthorityProof(validAccounts, strings.Repeat("00", 32)); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"malformed":     []byte(`{"schema":`),
		"unknown field": []byte(fmt.Sprintf(`{"schema":%q,"accounts_dir":%q,"extra":true}`, localServingStoreSchema, validAccounts)),
		"trailing value": []byte(fmt.Sprintf(
			`{"schema":%q,"accounts_dir":%q} {}`, localServingStoreSchema, validAccounts,
		)),
		"relative": []byte(fmt.Sprintf(`{"schema":%q,"accounts_dir":"relative"}`, localServingStoreSchema)),
		"stale": []byte(fmt.Sprintf(
			`{"schema":%q,"accounts_dir":%q}`, localServingStoreSchema, filepath.Join(home, "missing", "codex", "accounts"),
		)),
		"oversized": bytes.Repeat([]byte{'x'}, 4097),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := localServingStore(store); err == nil {
				t.Fatal("invalid serving-store binding unexpectedly succeeded")
			}
		})
	}
}

func TestLocalServingStoreRejectsSymlinkBindingAndPublicAuthorityKey(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, ".subrouter", "codex", "accounts")}
	stateDir := filepath.Join(home, "state")
	if err := os.MkdirAll(filepath.Join(stateDir, "codex", "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeLocalServingStoreBinding(t, store, stateDir)
	path := localServingStoreBindingPath(store)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	realBinding := filepath.Join(home, "real-binding.json")
	if err := os.WriteFile(realBinding, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realBinding, path); err != nil {
		t.Skipf("symlink test unavailable: %v", err)
	}
	if _, err := localServingStore(store); err == nil {
		t.Fatal("symlinked serving-store binding unexpectedly succeeded")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	accountsDir := filepath.Join(stateDir, "codex", "accounts")
	keyPath := filepath.Join(filepath.Dir(accountsDir), ".store-authority-key")
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := localServingStore(store); err == nil {
		t.Fatal("public serving-store authority key unexpectedly succeeded")
	}
}

func TestBindLocalServingStoreProvesDaemonBeforePublishing(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, ".subrouter", "codex", "accounts")}
	stateDir := filepath.Join(home, "candidate-state")
	if err := os.MkdirAll(filepath.Join(stateDir, "codex", "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	accountsDir, err := filepath.EvalSymlinks(filepath.Join(stateDir, "codex", "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	servingStore := accounts.CodexStore{Dir: accountsDir}
	if _, err := accounts.StoreAuthorityProof(servingStore.Dir, strings.Repeat("00", 32)); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/_subrouter/health" && request.URL.Path != proxy.StoreHandshakePath {
			http.NotFound(w, request)
			return
		}
		writeLocalStoreAuthorityHealth(t, w, request, servingStore, "enabled")
	}))
	defer server.Close()
	attachPrivateLocalTestListener(t, server)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL+"/v1")

	var out bytes.Buffer
	if err := bindLocalServingStore(stateDir, store, &out); err != nil {
		t.Fatal(err)
	}
	got, err := localServingStore(store)
	if err != nil {
		t.Fatal(err)
	}
	if got.Dir != servingStore.Dir || !strings.Contains(out.String(), stateDir) {
		t.Fatalf("binding result = %+v output=%q", got, out.String())
	}
	if err := unbindLocalServingStore(store, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(localServingStoreBindingPath(store)); !os.IsNotExist(err) {
		t.Fatalf("binding still exists: %v", err)
	}
}

func TestBindLocalServingStoreHonorsCanceledContext(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, ".subrouter", "codex", "accounts")}
	stateDir := filepath.Join(home, "candidate-state")
	if err := os.MkdirAll(filepath.Join(stateDir, "codex", "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL+"/v1")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := bindLocalServingStoreIfCurrent(
		ctx, stateDir, store, io.Discard, localServingStoreExpectation{},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled binding error = %v, want context canceled", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("canceled binding sent %d request(s)", requests.Load())
	}
}

func TestBindLocalServingStoreRejectsWrongDaemonWithoutPublishing(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, ".subrouter", "codex", "accounts")}
	stateDir := filepath.Join(home, "candidate-state")
	wrongState := filepath.Join(home, "wrong-state")
	for _, path := range []string{stateDir, wrongState} {
		if err := os.MkdirAll(filepath.Join(path, "codex", "accounts"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	wrongStore := accounts.CodexStore{Dir: filepath.Join(wrongState, "codex", "accounts")}
	candidateStore := accounts.CodexStore{Dir: filepath.Join(stateDir, "codex", "accounts")}
	for _, keyed := range []accounts.CodexStore{wrongStore, candidateStore} {
		if _, err := accounts.StoreAuthorityProof(keyed.Dir, strings.Repeat("00", 32)); err != nil {
			t.Fatal(err)
		}
	}
	var protected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/_subrouter/health" || request.URL.Path == proxy.StoreHandshakePath {
			writeLocalStoreAuthorityHealth(t, w, request, wrongStore, "enabled")
			return
		}
		protected.Add(1)
		http.Error(w, "credential sink", http.StatusUnauthorized)
	}))
	defer server.Close()
	attachPrivateLocalTestListener(t, server)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL)

	err := bindLocalServingStore(stateDir, store, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "handshake failed") {
		t.Fatalf("wrong-daemon binding error = %v", err)
	}
	if protected.Load() != 0 {
		t.Fatalf("binding sent %d protected request(s)", protected.Load())
	}
	if _, statErr := os.Stat(localServingStoreBindingPath(store)); !os.IsNotExist(statErr) {
		t.Fatalf("wrong-daemon binding was published: %v", statErr)
	}
}

func TestBindLocalServingStoreComparesPriorBindingUnderLock(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, ".subrouter", "codex", "accounts")}
	priorState := filepath.Join(home, "prior-state")
	candidateState := filepath.Join(home, "candidate-state")
	for _, state := range []string{priorState, candidateState} {
		if err := os.MkdirAll(filepath.Join(state, "codex", "accounts"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeLocalServingStoreBinding(t, store, priorState)
	path := localServingStoreBindingPath(store)
	prior, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	priorHash := sha256.Sum256(prior)
	candidateStore := accounts.CodexStore{Dir: filepath.Join(candidateState, "codex", "accounts")}
	if _, err := accounts.StoreAuthorityProof(candidateStore.Dir, strings.Repeat("00", 32)); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		writeLocalStoreAuthorityHealth(t, w, request, candidateStore, "enabled")
	}))
	defer server.Close()
	attachPrivateLocalTestListener(t, server)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL)

	wrong := localServingStoreExpectation{SHA256: strings.Repeat("0", 64), Mode: 0o600}
	if err := bindLocalServingStoreIfCurrent(t.Context(), candidateState, store, io.Discard, wrong); err == nil || !strings.Contains(err.Error(), "content mismatch") {
		t.Fatalf("wrong prior expectation error = %v", err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(unchanged, prior) {
		t.Fatalf("wrong expectation changed binding: err=%v", err)
	}
	correct := localServingStoreExpectation{SHA256: fmt.Sprintf("%x", priorHash), Mode: 0o600}
	if err := bindLocalServingStoreIfCurrent(t.Context(), candidateState, store, io.Discard, correct); err != nil {
		t.Fatal(err)
	}
	got, err := localServingStore(store)
	want, canonicalErr := filepath.EvalSymlinks(candidateStore.Dir)
	if err != nil || canonicalErr != nil || got.Dir != want {
		t.Fatalf("candidate binding = %+v err=%v", got, err)
	}
}

func TestParseLocalServingStoreExpectation(t *testing.T) {
	absent, err := parseLocalServingStoreExpectation([]string{"--if-current-absent"})
	if err != nil || !absent.Absent {
		t.Fatalf("absent expectation = %+v err=%v", absent, err)
	}
	digest := strings.Repeat("a", 64)
	current, err := parseLocalServingStoreExpectation([]string{"--if-current-sha256", digest, "--if-current-mode", "600"})
	if err != nil || current.SHA256 != digest || current.Mode != 0o600 {
		t.Fatalf("current expectation = %+v err=%v", current, err)
	}
	for _, args := range [][]string{
		{"--if-current-absent", "--if-current-sha256", digest, "--if-current-mode", "600"},
		{"--if-current-sha256", "short", "--if-current-mode", "600"},
		{"--if-current-sha256", digest},
		{"--if-current-sha256", digest, "--if-current-mode", "644"},
	} {
		if _, err := parseLocalServingStoreExpectation(args); err == nil {
			t.Fatalf("invalid expectation %v unexpectedly succeeded", args)
		}
	}
}

func TestBindLocalServingStoreRejectsUnsafeMutationLock(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, ".subrouter", "codex", "accounts")}
	if err := os.MkdirAll(store.StoreDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(store.StoreDir(), ".local-serving-store.lock")
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := unbindLocalServingStore(store, io.Discard); err == nil || !strings.Contains(err.Error(), "private regular file") {
		t.Fatalf("unsafe serving-store lock error = %v", err)
	}
}

func TestUnbindLocalServingStoreRejectsWritableLockDirectory(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, ".subrouter", "codex", "accounts")}
	if err := os.MkdirAll(store.StoreDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(store.StoreDir(), 0o777); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(store.StoreDir(), 0o700)
	if err := unbindLocalServingStore(store, io.Discard); err == nil || !strings.Contains(err.Error(), "not group/world writable") {
		t.Fatalf("writable serving-store lock directory error = %v", err)
	}
}

func TestCodexLocalLaunchUsesBoundServingStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	stateDir := filepath.Join(home, "candidate-state")
	if err := os.MkdirAll(filepath.Join(stateDir, "codex", "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeLocalServingStoreBinding(t, store, stateDir)
	servingStore := accounts.CodexStore{Dir: filepath.Join(stateDir, "codex", "accounts")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/_subrouter/health" {
			http.NotFound(w, request)
			return
		}
		writeLocalStoreAuthorityHealth(t, w, request, servingStore, "enabled")
	}))
	defer server.Close()
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL+"/v1")

	record := filepath.Join(home, "record")
	bin := filepath.Join(home, "codex-fake")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + shellQuote(record) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_CODEX_BIN", bin)
	if err := codex([]string{"exec", "prompt"}); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(record); err != nil || !strings.Contains(string(body), "model_providers.subrouter.base_url") {
		t.Fatalf("Codex launch record = %q err=%v", body, err)
	}
}

func writeLocalServingStoreBinding(t *testing.T, store accounts.CodexStore, stateDir string) {
	t.Helper()
	if err := os.MkdirAll(store.StoreDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	accountsDir, err := filepath.EvalSymlinks(filepath.Join(stateDir, "codex", "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.StoreAuthorityProof(accountsDir, strings.Repeat("00", 32)); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"schema":%q,"accounts_dir":%q}`+"\n", localServingStoreSchemaV1, accountsDir)
	if err := os.WriteFile(localServingStoreBindingPath(store), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
