package accounts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A caller that disconnects after the token endpoint rotated the single-use
// refresh token must not lose the new pair.
func TestRefreshStoredPersistsRotatedPairWhenCallerCancelsMidRefresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	originalURL := codexOAuthTokenURL
	defer func() { codexOAuthTokenURL = originalURL }()

	store := CodexStore{Dir: t.TempDir()}
	stale := storedOAuthAccount("cancel@example.com", "old", time.Now().Add(-time.Hour))
	if err := store.SaveStored(stale); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		// Hold the response until the client has either abandoned the
		// connection (caller cancellation reached the transport) or clearly
		// kept waiting for it.
		select {
		case <-r.Context().Done():
		case <-time.After(300 * time.Millisecond):
		}
		body, _ := json.Marshal(map[string]string{
			"access_token":  testCodexJWT("cancel@example.com", "new-access", time.Now().Add(time.Hour)),
			"refresh_token": "new-refresh",
			"id_token":      testCodexJWT("cancel@example.com", "new-id", time.Now().Add(time.Hour)),
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()
	codexOAuthTokenURL = server.URL

	_, _, refreshErr := store.RefreshStoredIfExpired(ctx, server.Client(), stale)

	got, ok, err := store.FindStored(stale.Email)
	if err != nil || !ok {
		t.Fatalf("stored account found = %v, err = %v", ok, err)
	}
	if got.Auth.Tokens.RefreshToken != "new-refresh" {
		t.Fatalf("stored refresh token = %q (refresh err: %v); want the rotated new-refresh persisted", got.Auth.Tokens.RefreshToken, refreshErr)
	}
}

// A failed write must never leave the user without an auth.json.
func TestWriteCodexActiveAuthNeverLeavesAuthFileMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	previous := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"previous"}}`)
	if err := os.WriteFile(path, previous, 0o600); err != nil {
		t.Fatal(err)
	}
	// A leftover directory at the legacy fixed temp name makes a fixed-name
	// temp write fail.
	if err := os.Mkdir(path+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}

	writeErr := writeCodexActiveAuth(path, json.RawMessage(`{"auth_mode":"chatgpt","tokens":{"access_token":"next"}}`))

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("auth.json unreadable after write (write err: %v): %v", writeErr, err)
	}
	var active CodexAuthFile
	if err := json.Unmarshal(body, &active); err != nil {
		t.Fatalf("auth.json is not valid JSON: %v", err)
	}
	if writeErr == nil {
		if active.Tokens == nil || active.Tokens.AccessToken != "next" {
			t.Fatalf("auth.json = %s, want the new credential after a successful write", body)
		}
		backup, err := os.ReadFile(path + ".bak")
		if err != nil {
			t.Fatalf("missing backup: %v", err)
		}
		if string(backup) != string(previous) {
			t.Fatalf("backup = %s, want previous auth.json", backup)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if name := entry.Name(); name != "auth.json" && name != "auth.json.bak" && name != "auth.json.tmp" &&
			filepath.Ext(name) != ".lock" {
			t.Fatalf("leftover file %q after write", name)
		}
	}
}

// A background refresh of account A that read ~/.codex/auth.json before the
// user switched to B must not overwrite B with A's refreshed tokens.
func TestRefreshSyncDoesNotOverwriteConcurrentSwitch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir()}
	stale := storedOAuthAccount("a@example.com", "a-old", time.Now().Add(-time.Hour))
	other := storedOAuthAccount("b@example.com", "b", time.Now().Add(time.Hour))
	if err := store.SaveStored(stale); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStored(other); err != nil {
		t.Fatal(err)
	}
	if err := WriteActiveCodexAuth(stale.Auth); err != nil {
		t.Fatal(err)
	}

	switchDone := make(chan error, 1)
	afterActiveCodexAuthSyncRead = func() {
		afterActiveCodexAuthSyncRead = nil
		go func() {
			_, err := store.SwitchActiveStored(other.Email)
			switchDone <- err
		}()
		// Give the switch every chance to land in the read-compare-write
		// window. With the window closed it blocks until the sync finishes.
		select {
		case err := <-switchDone:
			switchDone <- err
		case <-time.After(500 * time.Millisecond):
		}
	}
	defer func() { afterActiveCodexAuthSyncRead = nil }()

	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return refreshResponse("a-new", "a@example.com", time.Now().Add(time.Hour)), nil
	})}
	if _, _, err := store.RefreshStoredIfExpired(context.Background(), client, stale); err != nil {
		t.Fatal(err)
	}
	if err := <-switchDone; err != nil {
		t.Fatal(err)
	}

	active, ok, err := ReadActiveCodexAuth()
	if err != nil || !ok {
		t.Fatalf("active auth ok = %v, err = %v", ok, err)
	}
	if active.Tokens == nil || active.Tokens.RefreshToken != "b-refresh" {
		got := ""
		if active.Tokens != nil {
			got = active.Tokens.RefreshToken
		}
		t.Fatalf("active refresh token = %q; want b-refresh (the account the user switched to)", got)
	}
}
