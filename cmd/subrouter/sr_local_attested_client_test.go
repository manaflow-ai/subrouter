package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/proxy"
)

type localAttestationConnectionKey struct{}

func TestLocalStoreAttestedClientAttestsEveryConnectionBeforeRequest(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	initializeLocalDataTestStore(t, store)
	var nextConnection atomic.Int64
	var mu sync.Mutex
	requests := map[int64][]string{}
	var healthAuthorization atomic.Value

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		connectionID, _ := request.Context().Value(localAttestationConnectionKey{}).(int64)
		mu.Lock()
		requests[connectionID] = append(requests[connectionID], request.URL.Path)
		mu.Unlock()
		switch request.URL.Path {
		case "/_subrouter/health", proxy.StoreHandshakePath:
			if authorization := request.Header.Get("Authorization"); authorization != "" {
				healthAuthorization.Store(authorization)
			}
			writeLocalStoreAuthorityHealth(t, w, request, store, "enabled")
		case "/_subrouter/accounts":
			if request.Header.Get("Authorization") != "Bearer after-attestation" {
				http.Error(w, "missing test authorization", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Connection", "close")
			_, _ = io.WriteString(w, `[]`)
		default:
			http.NotFound(w, request)
		}
	}))
	server.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, localAttestationConnectionKey{}, nextConnection.Add(1))
	}
	server.Start()
	defer server.Close()
	attachPrivateLocalTestListener(t, server)

	client, err := newLocalDataClient(server.Client(), server.URL, store)
	if err != nil {
		t.Fatal(err)
	}
	transport := client.Transport.(*http.Transport)
	for iteration := 0; iteration < 2; iteration++ {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/_subrouter/accounts", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer after-attestation")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		transport.CloseIdleConnections()
	}

	if authorization, _ := healthAuthorization.Load().(string); authorization != "" {
		t.Fatalf("store attestation carried Authorization %q", authorization)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("connection request sequences = %#v, want two connections", requests)
	}
	for connectionID, sequence := range requests {
		if got := strings.Join(sequence, ","); got != proxy.StoreHandshakePath+",/_subrouter/accounts" {
			t.Fatalf("connection %d request sequence = %q, want handshake before protected request", connectionID, got)
		}
	}
}

func TestLocalStoreAttestedClientFailsClosedWhenSocketBecomesUnsafe(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	initializeLocalDataTestStore(t, store)
	var protectedRequests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == proxy.StoreHandshakePath {
			writeLocalStoreAuthorityHealth(t, w, request, store, "enabled")
			return
		}
		protectedRequests.Add(1)
		if request.Header.Get("Authorization") != "Bearer protected" {
			http.Error(w, "missing credential", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Connection", "close")
		_, _ = io.WriteString(w, `[]`)
	}))
	server.Start()
	defer server.Close()
	socket := attachPrivateLocalTestListener(t, server)

	client, err := newLocalDataClient(server.Client(), server.URL, store)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/_subrouter/accounts", nil)
	request.Header.Set("Authorization", "Bearer protected")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	client.Transport.(*http.Transport).CloseIdleConnections()
	if err := os.Chmod(socket, 0o666); err != nil {
		t.Fatal(err)
	}
	request, _ = http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/_subrouter/accounts", nil)
	request.Header.Set("Authorization", "Bearer protected")
	if _, err := client.Do(request); err == nil || !strings.Contains(err.Error(), "mode-0600") {
		t.Fatalf("unsafe-socket error = %v, want private socket rejection", err)
	}
	if protectedRequests.Load() != 1 {
		t.Fatalf("protected requests = %d, want only the pre-replacement request", protectedRequests.Load())
	}
}

func TestLocalStoreAttestedClientRejectsUnsafeSocketBeforeCredential(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	initializeLocalDataTestStore(t, store)
	var protectedRequests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		protectedRequests.Add(1)
		_, _ = io.WriteString(w, `[]`)
	}))
	server.Start()
	defer server.Close()
	socket := attachPrivateLocalTestListener(t, server)
	if err := os.Chmod(socket, 0o666); err != nil {
		t.Fatal(err)
	}

	client, err := newLocalDataClient(server.Client(), server.URL, store)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/_subrouter/accounts", nil)
	request.Header.Set("Authorization", "Bearer must-not-be-sent")
	if _, err := client.Do(request); err == nil || !strings.Contains(err.Error(), "mode-0600") {
		t.Fatalf("unsafe socket error = %v", err)
	}
	if protectedRequests.Load() != 0 {
		t.Fatalf("non-persistent listener received %d protected request(s)", protectedRequests.Load())
	}
}

func TestLocalDataClientRejectsPrivateSocketReplacementAfterBinding(t *testing.T) {
	// This test deliberately supplies its own binding and socket. Do not let a
	// developer/CI daemon's explicit state authority redirect the client before
	// it reaches the replacement-identity assertion.
	t.Setenv("SUBROUTER_STATE_DIR", "")
	t.Setenv("SUBROUTER_LOCAL_DATA_SOCKET", "")
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, "state", "codex", "accounts")}
	initializeLocalDataTestStore(t, store)
	socketDir, err := os.MkdirTemp(privateSocketTempRoot(t), "sr-replace-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "data.sock")
	legitimate, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := localDataSocketIdentity(socket)
	if err != nil {
		t.Fatal(err)
	}
	bindingPath := localServingStoreBindingPath(store)
	if err := os.MkdirAll(filepath.Dir(bindingPath), 0o700); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(localServingStoreBinding{
		Schema: localServingStoreSchema, AccountsDir: store.Dir,
		LocalDataSocket: socket, LocalDataSocketIdentity: identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bindingPath, append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := legitimate.Close(); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(socket)
	var received atomic.Int32
	// Some Linux filesystems can immediately reuse the removed socket inode.
	// Prefer a distinct replacement identity; if the filesystem reuses it,
	// retain a deliberately stale binding identity so the same fail-closed
	// invariant is tested without depending on inode-allocation luck.
	replacement, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	replacementIdentity, identityErr := localDataSocketIdentity(socket)
	if identityErr != nil {
		t.Fatal(identityErr)
	}
	if replacementIdentity == identity {
		payload = bytes.Replace(payload, []byte(identity), []byte(identity+":stale"), 1)
		if err := os.WriteFile(bindingPath, payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	defer replacement.Close()
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		received.Add(1)
		http.Error(w, "credential sink", http.StatusUnauthorized)
	})}
	go func() { _ = server.Serve(replacement) }()
	defer server.Close()

	client, err := newLocalDataClient(&http.Client{}, "http://127.0.0.1:31415", store)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:31415/_subrouter/accounts", nil)
	request.Header.Set("Authorization", "Bearer must-not-leak")
	if _, err := client.Do(request); err == nil || !strings.Contains(err.Error(), "identity does not match") {
		t.Fatalf("replacement error = %v, want binding identity rejection", err)
	}
	if received.Load() != 0 {
		t.Fatalf("replacement received %d request(s)", received.Load())
	}
}

func TestLocalDataClientUsesBoundServingStoreForHandshake(t *testing.T) {
	home := t.TempDir()
	bindingStore := accounts.CodexStore{Dir: filepath.Join(home, "cli", "codex", "accounts")}
	servingStore := accounts.CodexStore{Dir: filepath.Join(home, "candidate", "codex", "accounts")}
	initializeLocalDataTestStore(t, bindingStore)
	initializeLocalDataTestStore(t, servingStore)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == proxy.StoreHandshakePath {
			writeLocalStoreAuthorityHealth(t, w, request, servingStore, "enabled")
			return
		}
		if request.URL.Path == "/_subrouter/accounts" {
			_, _ = io.WriteString(w, `[]`)
			return
		}
		http.NotFound(w, request)
	}))
	defer server.Close()
	attachPrivateLocalTestListener(t, server)

	client, err := newLocalDataClientWithStoreResolvers(
		server.Client(), server.URL,
		func() (accounts.CodexStore, error) { return bindingStore, nil },
		func() (accounts.CodexStore, error) { return servingStore, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(server.URL + "/_subrouter/accounts")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("accounts status = %d", response.StatusCode)
	}
}

func TestLocalDataSocketForExplicitStateUsesSupervisorDefault(t *testing.T) {
	stateDir, err := os.MkdirTemp(privateSocketTempRoot(t), "sr-state-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	t.Setenv("SUBROUTER_STATE_DIR", stateDir)
	socket := filepath.Join(stateDir, "local-data.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := localDataSocketForStore(accounts.CodexStore{Dir: filepath.Join(stateDir, "codex", "accounts")})
	if err != nil {
		t.Fatal(err)
	}
	if got != socket {
		t.Fatalf("explicit-state socket = %q, want %q", got, socket)
	}
}

func TestReadyLocalServingServerDoesNotLoadOrSendAdminToken(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, "accounts")}
	initializeLocalDataTestStore(t, store)
	var unexpectedAuthorization atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if authorization := request.Header.Get("Authorization"); authorization != "" {
			unexpectedAuthorization.Store(authorization)
		}
		switch request.URL.Path {
		case "/_subrouter/health", proxy.StoreHandshakePath:
			writeLocalStoreAuthorityHealth(t, w, request, store, "disabled")
		case "/_subrouter/accounts":
			_, _ = io.WriteString(w, `[]`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	attachPrivateLocalTestListener(t, server)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL)
	t.Setenv("SUBROUTER_ADMIN_TOKEN", "environment-admin-must-not-load")
	t.Setenv("SUBROUTER_ADMIN_TOKEN_FILE", filepath.Join(home, "missing-admin-token"))
	if err := defaultSRServerStore(store).update(func(file *srServerFile) error {
		file.Servers = []srServerConfig{{Name: "matching", URL: server.URL, AdminToken: "registry-admin-must-not-load"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	runner := srRunner{store: store, client: server.Client(), out: io.Discard, errOut: io.Discard}
	local, err := runner.readyLocalServingServer(t.Context(), func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if local.AdminToken != "" {
		t.Fatal("ready local server retained an administrator token")
	}
	if _, err := runner.fetchServerAccounts(t.Context(), local); err != nil {
		t.Fatal(err)
	}
	if authorization, _ := unexpectedAuthorization.Load().(string); authorization != "" {
		t.Fatalf("local control request sent Authorization %q", authorization)
	}
}

func TestLocalServingStoreAuthorityRemovesAPIPathForHealth(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/_subrouter/health" {
			http.NotFound(w, request)
			return
		}
		writeLocalStoreAuthorityHealth(t, w, request, store, "disabled")
	}))
	defer server.Close()

	runner := srRunner{store: store, client: server.Client()}
	authority, err := runner.localServingStoreAuthorityForStore(
		t.Context(), srServerConfig{URL: server.URL + "/v1"}, store,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !authority.storeMatches {
		t.Fatal("serving-store authority did not match")
	}
}

func writeLocalStoreAuthorityHealth(t *testing.T, w http.ResponseWriter, request *http.Request, store accounts.CodexStore, importState string) {
	t.Helper()
	if request.URL.Path == proxy.StoreHandshakePath {
		nonce := request.Header.Get(accounts.StoreHandshakeNonceHeader)
		verified, err := accounts.VerifyStoreHandshakeRequest(store.Dir, nonce, request.Header.Get(accounts.StoreHandshakeRequestHeader))
		if err != nil || !verified {
			http.NotFound(w, request)
			return
		}
		authorityID, err := accounts.StoreAuthorityID(store.Dir)
		if err != nil {
			t.Fatal(err)
		}
		proof, err := accounts.ExistingStoreHandshakeResponseProof(store.Dir, nonce)
		if err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"account_store_id": authorityID, "account_store_proof": proof})
		return
	}
	authorityID, err := accounts.StoreAuthorityID(store.Dir)
	if err != nil {
		t.Error(err)
		http.Error(w, "authority failure", http.StatusInternalServerError)
		return
	}
	proof := ""
	if challenge := request.Header.Get(accounts.StoreAuthorityChallengeHeader); challenge != "" {
		proof, err = accounts.StoreAuthorityProof(store.Dir, challenge)
		if err != nil {
			t.Error(err)
			http.Error(w, "proof failure", http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"ok":true,"account_import":%q,"account_store_id":%q,"account_store_proof":%q}`, importState, authorityID, proof)
}

func attachPrivateLocalTestListener(t *testing.T, server *httptest.Server, stores ...accounts.CodexStore) string {
	t.Helper()
	if len(stores) > 1 {
		t.Fatal("attach private local test listener accepts at most one account store")
	}
	if len(stores) == 1 {
		initializeLocalDataTestStore(t, stores[0])
	}
	directory, err := os.MkdirTemp(privateSocketTempRoot(t), "sr-local-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "local-data.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		listener.Close()
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_LOCAL_DATA_SOCKET", socket)
	if len(stores) == 1 {
		handler := server.Config.Handler
		store := stores[0]
		go func() {
			_ = http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path == proxy.StoreHandshakePath {
					writeLocalStoreAuthorityHealth(t, w, request, store, "enabled")
					return
				}
				handler.ServeHTTP(w, request)
			}))
		}()
	} else {
		go func() { _ = server.Config.Serve(listener) }()
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(socket)
	})
	return socket
}

func initializeLocalDataTestStore(t *testing.T, store accounts.CodexStore) {
	t.Helper()
	// StoreAuthorityProof initializes the parent authority directory, while
	// serving-store attestation also requires the accounts directory itself to
	// already exist. Create both explicitly so the invariant is portable across
	// macOS and Linux.
	if err := os.MkdirAll(store.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.StoreAuthorityProof(store.Dir, strings.Repeat("00", 32)); err != nil {
		t.Fatal(err)
	}
}
