package accounts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCodexStoreRawAuthForFindsStoredAccount(t *testing.T) {
	dir := t.TempDir()
	writeStoredAccount(t, dir, "b@example.com", `{"auth_mode":"chatgpt","tokens":{"access_token":"token"},"last_refresh":"now"}`)
	store := CodexStore{Dir: dir}

	account, rawAuth, err := store.rawAuthFor("b@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != "b@example.com" {
		t.Fatalf("account ID = %q, want b@example.com", account.ID)
	}
	var auth map[string]any
	if err := json.Unmarshal(rawAuth, &auth); err != nil {
		t.Fatal(err)
	}
	if auth["last_refresh"] != "now" {
		t.Fatalf("last_refresh = %v, want now", auth["last_refresh"])
	}
}

func TestSwitchActiveDowngradesIsolatedOAuthOriginBeforeExport(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir()}
	account := StoredCodexAccount{
		Email:                 "isolated@example.com",
		OAuthCredentialOrigin: CodexOAuthOriginIsolatedServerLogin,
		Auth: CodexAuthFile{AuthMode: "chatgpt", Tokens: &CodexTokens{
			AccessToken: "access", RefreshToken: "refresh", IDToken: "id",
		}},
	}
	if err := store.SaveStored(account); err != nil {
		t.Fatal(err)
	}
	activated, err := store.SwitchActiveStored(account.Email)
	if err != nil {
		t.Fatal(err)
	}
	if activated.OAuthCredentialOrigin != CodexOAuthOriginInteractiveImport {
		t.Fatalf("activated OAuth origin = %q, want interactive import", activated.OAuthCredentialOrigin)
	}
	if activated.Auth.Tokens == nil || activated.Auth.Tokens.RefreshToken != "refresh" {
		t.Fatalf("activated account = %#v, want exact stored OAuth chain", activated)
	}
	stored, ok, err := store.FindStored(account.Email)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("stored account disappeared")
	}
	if stored.OAuthCredentialOrigin != CodexOAuthOriginInteractiveImport {
		t.Fatalf("OAuth origin = %q, want interactive import", stored.OAuthCredentialOrigin)
	}
	if len(stored.Breadcrumbs) == 0 || stored.Breadcrumbs[len(stored.Breadcrumbs)-1].Event != "credential_exported_to_active" {
		t.Fatalf("breadcrumbs = %#v", stored.Breadcrumbs)
	}
}

func TestSwitchActiveDowngradesServerAttestedOAuthOriginBeforeExport(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir()}
	account := StoredCodexAccount{
		Email:                 "attested@example.com",
		OAuthCredentialOrigin: CodexOAuthOriginServerAttested,
		Auth: CodexAuthFile{AuthMode: "chatgpt", Tokens: &CodexTokens{
			AccessToken: "access", RefreshToken: "refresh", IDToken: "id",
		}},
	}
	if err := store.SaveStored(account); err != nil {
		t.Fatal(err)
	}
	activated, err := store.SwitchActiveStored(account.Email)
	if err != nil {
		t.Fatal(err)
	}
	if activated.OAuthCredentialOrigin != CodexOAuthOriginInteractiveImport {
		t.Fatalf("activated OAuth origin = %q, want interactive import", activated.OAuthCredentialOrigin)
	}
}

func TestSwitchActiveRestoresIsolatedOriginWhenActiveWriteFails(t *testing.T) {
	root := t.TempDir()
	blockedCodexHome := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blockedCodexHome, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", blockedCodexHome)
	store := CodexStore{Dir: t.TempDir()}
	account := StoredCodexAccount{
		Email:                 "isolated@example.com",
		OAuthCredentialOrigin: CodexOAuthOriginIsolatedServerLogin,
		Auth: CodexAuthFile{AuthMode: "chatgpt", Tokens: &CodexTokens{
			AccessToken: "access", RefreshToken: "refresh", IDToken: "id",
		}},
	}
	if err := store.SaveStored(account); err != nil {
		t.Fatal(err)
	}
	if err := store.SwitchActive(account.Email); err == nil {
		t.Fatal("switch unexpectedly succeeded")
	}
	stored, ok, err := store.FindStored(account.Email)
	if err != nil || !ok {
		t.Fatalf("stored account = %#v, found = %v, err = %v", stored, ok, err)
	}
	if stored.OAuthCredentialOrigin != CodexOAuthOriginIsolatedServerLogin {
		t.Fatalf("OAuth origin = %q, want isolated server login", stored.OAuthCredentialOrigin)
	}
}

func TestCodexStoreListIgnoresHiddenAccountArtifacts(t *testing.T) {
	dir := t.TempDir()
	writeStoredAccount(t, dir, "b@example.com", `{"auth_mode":"chatgpt","tokens":{"access_token":"token"},"last_refresh":"now"}`)
	if err := os.WriteFile(filepath.Join(dir, "._b@example.com.json"), []byte{0, 1, 2}, 0o600); err != nil {
		t.Fatal(err)
	}

	accounts, err := (CodexStore{Dir: dir}).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	if accounts[0].ID != "b@example.com" {
		t.Fatalf("account ID = %q, want b@example.com", accounts[0].ID)
	}
}

func TestWriteCodexActiveAuthPreservesOAuthPayloadAndBacksUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"auth_mode":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	err := writeCodexActiveAuth(path, json.RawMessage(`{"auth_mode":"chatgpt","tokens":{"access_token":"token"},"last_refresh":"now"}`))
	if err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var active map[string]any
	if err := json.Unmarshal(body, &active); err != nil {
		t.Fatal(err)
	}
	if active["last_refresh"] != "now" {
		t.Fatalf("last_refresh = %v, want now", active["last_refresh"])
	}
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Fatalf("missing backup: %v", err)
	}
}

func TestWriteCodexActiveAuthStripsEmptyAPIKeyTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".codex", "auth.json")
	err := writeCodexActiveAuth(path, json.RawMessage(`{"auth_mode":"apikey","OPENAI_API_KEY":"sk-test","tokens":{"id_token":""}}`))
	if err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var active map[string]any
	if err := json.Unmarshal(body, &active); err != nil {
		t.Fatal(err)
	}
	if active["OPENAI_API_KEY"] != "sk-test" {
		t.Fatalf("OPENAI_API_KEY = %v, want sk-test", active["OPENAI_API_KEY"])
	}
	if _, ok := active["tokens"]; ok {
		t.Fatal("tokens should be stripped for API-key auth")
	}
}

func writeStoredAccount(t *testing.T, dir, email, rawAuth string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, email+".json")
	body := []byte(`{"email":` + quoteJSON(email) + `,"addedAt":"2026-04-28T00:00:00Z","auth":` + rawAuth + `}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func quoteJSON(value string) string {
	body, _ := json.Marshal(value)
	return string(body)
}
