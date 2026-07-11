package claude

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"io"
)

func TestStoreCreateSetRemoveProfile(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	instancePath, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"session-env", "todos", "logs", "file-history", "shell-snapshots", "debug", ".anthropic"} {
		if _, err := os.Stat(filepath.Join(instancePath, dir)); err != nil {
			t.Fatalf("missing instance dir %s: %v", dir, err)
		}
	}
	if active := store.ActiveProfile(); active != "work" {
		t.Fatalf("active = %q, want work", active)
	}
	if err := store.SetActiveProfile("work"); err != nil {
		t.Fatal(err)
	}
	removed, err := store.RemoveProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("profile was not removed")
	}
	if profiles := store.ListProfiles(); len(profiles) != 0 {
		t.Fatalf("profiles = %d, want 0", len(profiles))
	}
}

func TestRegisterProfileAllowsEmailName(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	_, dir, err := store.CreateTempInstance()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterProfile("person@example.com", dir); err != nil {
		t.Fatal(err)
	}
	profile, ok := store.FindProfile("person@example.com")
	if !ok {
		t.Fatal("profile not found")
	}
	if profile.Dir != dir {
		t.Fatalf("dir = %q, want %q", profile.Dir, dir)
	}
}

func TestClaudeConfigDirPrefersCodexAccountsAlias(t *testing.T) {
	home := t.TempDir()
	store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(store.Dir, filepath.Join(home, ".codex-accounts")); err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(home, ".codex-accounts", "claude", "work")
	if got := store.ClaudeConfigDir("work"); got != want {
		t.Fatalf("ClaudeConfigDir = %q, want %q", got, want)
	}
}

func TestClaudeConfigDirFallsBackWhenAliasMissing(t *testing.T) {
	home := t.TempDir()
	store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	instancePath, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}

	if got := store.ClaudeConfigDir("work"); got != filepath.Clean(instancePath) {
		t.Fatalf("ClaudeConfigDir = %q, want %q", got, filepath.Clean(instancePath))
	}
}

func TestCredentialPayloadIsSingleLine(t *testing.T) {
	// macOS `security find-generic-password -w` hex-encodes any keychain value
	// containing a newline on read, so the persisted payload must be one line.
	body, err := credentialPayload(CredentialInfo{AccessToken: "tok", RefreshToken: "refresh", SubscriptionType: "pro"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.ContainsRune(body, '\n') {
		t.Fatalf("credentialPayload must be single-line (no newline); got %q", body)
	}
	cred, err := parseCredentialPayload(body)
	if err != nil || cred == nil {
		t.Fatalf("round-trip parse: cred=%v err=%v", cred, err)
	}
	if cred.AccessToken != "tok" || cred.RefreshToken != "refresh" {
		t.Fatalf("round-trip credential = %+v, want accessToken=tok refreshToken=refresh", cred)
	}
}

func TestParseCredentialPayloadHealsHexEncoded(t *testing.T) {
	// A credential written by an older build (indented JSON) that macOS
	// `security -w` returned hex-encoded on read must still be readable.
	indented := []byte("{\n  \"claudeAiOauth\": {\n    \"accessToken\": \"tok\",\n    \"refreshToken\": \"refresh\"\n  }\n}")
	hexed := []byte(hex.EncodeToString(indented))
	cred, err := parseCredentialPayload(hexed)
	if err != nil {
		t.Fatalf("heal+parse hex-encoded credential: %v", err)
	}
	if cred == nil || cred.AccessToken != "tok" || cred.RefreshToken != "refresh" {
		t.Fatalf("healed credential = %+v, want accessToken=tok refreshToken=refresh", cred)
	}
}

func TestHealHexEncodedCredentialNoOpOnPlainJSON(t *testing.T) {
	// Real credential JSON starts with the byte '{' (not the ASCII "7b"), so the
	// heal must never mutate it.
	for name, in := range map[string][]byte{
		"compact":       []byte(`{"claudeAiOauth":{"accessToken":"tok"}}`),
		"indented":      []byte("{\n  \"claudeAiOauth\": {}\n}"),
		"leading-space": []byte(`  {"claudeAiOauth":{}}`),
	} {
		if got := healHexEncodedCredential(in); !bytes.Equal(got, in) {
			t.Fatalf("%s: heal mutated plain JSON: %q -> %q", name, in, got)
		}
	}
}

func TestHealHexEncodedCredentialGuardsMalformed(t *testing.T) {
	// Inputs that pass the "7b"/"5b" prefix filter but are not valid hex must be
	// returned unchanged (fail-closed; downstream json.Unmarshal reports it).
	for _, in := range [][]byte{
		[]byte("7"),    // too short
		[]byte("7b0"),  // odd length
		[]byte("7bzz"), // non-hex body
	} {
		if got := healHexEncodedCredential(in); !bytes.Equal(got, in) {
			t.Fatalf("guard: %q was mutated to %q", in, got)
		}
	}
}

func TestReadCredentialFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"tok","refreshToken":"refresh","subscriptionType":"pro"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	credential, ok := readCredentialFile(dir)
	if !ok {
		t.Fatal("credential not read")
	}
	if credential.AccessToken != "tok" {
		t.Fatalf("access token = %q, want tok", credential.AccessToken)
	}
	if credential.RefreshToken != "refresh" {
		t.Fatalf("refresh token = %q, want refresh", credential.RefreshToken)
	}
}

func TestRefreshCredentialPostsClaudeOAuthRefresh(t *testing.T) {
	originalURL := oauthTokenURL
	defer func() { oauthTokenURL = originalURL }()

	var sawRequest bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRequest = true
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("anthropic-beta"); got != oauthBetaHeader {
			t.Fatalf("anthropic-beta = %q, want %q", got, oauthBetaHeader)
		}
		if got := r.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Fatalf("Content-Type = %q, want JSON", got)
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if got := payload["client_id"]; got != oauthClientID {
			t.Fatalf("client_id = %q, want %q", got, oauthClientID)
		}
		if got := payload["grant_type"]; got != "refresh_token" {
			t.Fatalf("grant_type = %q, want refresh_token", got)
		}
		if got := payload["refresh_token"]; got != "old-refresh" {
			t.Fatalf("refresh_token = %q, want old-refresh", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"expires_in":    3600,
		})
	}))
	defer server.Close()
	oauthTokenURL = server.URL

	got, err := RefreshCredential(context.Background(), server.Client(), CredentialInfo{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawRequest {
		t.Fatal("refresh endpoint was not called")
	}
	if got.AccessToken != "new-access" || got.RefreshToken != "new-refresh" {
		t.Fatalf("tokens = %q/%q, want new-access/new-refresh", got.AccessToken, got.RefreshToken)
	}
	if got.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("ExpiresAt = %d, want future", got.ExpiresAt)
	}
}

func TestListAccountsReadsProfilesWithCredentials(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	instancePath, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instancePath, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"tok","subscriptionType":"max","expiresAt":4102444800000}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := store.ListAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("accounts = %d, want 1", len(got))
	}
	account := got[0]
	if account.ID != "work" {
		t.Fatalf("ID = %q, want work", account.ID)
	}
	if account.Provider != accounts.ProviderClaude {
		t.Fatalf("Provider = %q, want claude", account.Provider)
	}
	if account.AuthMode != accounts.AuthModeOAuth {
		t.Fatalf("AuthMode = %q, want oauth", account.AuthMode)
	}
	if account.Token != "tok" {
		t.Fatalf("Token = %q, want tok", account.Token)
	}
	if account.Source != filepath.Clean(instancePath) {
		t.Fatalf("Source = %q, want %q", account.Source, filepath.Clean(instancePath))
	}
}

// Anthropic rejects subscription-OAuth Messages calls that do not look like
// Claude Code with a headerless 429 regardless of quota (verified live
// 2026-07-04 on a fresh Max 20x account). The probe must carry the Claude Code
// system prompt, beta tag, and client identity, or every account's fable pool
// reads as exhausted.
func TestFetchFableUsageWindowsSendsClaudeCodeShape(t *testing.T) {
	var gotBody string
	var gotBeta, gotUA, gotXApp string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotBeta = r.Header.Get("anthropic-beta")
		gotUA = r.Header.Get("User-Agent")
		gotXApp = r.Header.Get("x-app")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d_Oi-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d_Oi-Utilization", "0.0")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"message"}`))
	}))
	defer server.Close()
	restore := messagesURL
	messagesURL = server.URL + "/v1/messages"
	defer func() { messagesURL = restore }()

	windows, err := FetchFableUsageWindows(context.Background(), server.Client(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, "You are Claude Code") {
		t.Fatalf("probe body missing Claude Code system prompt: %s", gotBody)
	}
	if !strings.Contains(gotBeta, "claude-code-") || !strings.Contains(gotBeta, oauthBetaHeader) {
		t.Fatalf("probe beta header = %q, want claude-code tag plus oauth beta", gotBeta)
	}
	if !strings.Contains(gotUA, "claude-cli") || gotXApp != "cli" {
		t.Fatalf("probe client identity = UA %q x-app %q, want claude-cli/cli", gotUA, gotXApp)
	}
	if len(windows) != 1 || windows[0].Feature != FableFeature || windows[0].UsedPercent != 0 {
		t.Fatalf("windows = %+v, want one fresh fable window", windows)
	}
}

// A headerless 429 must return no windows (unknown), never a synthesized
// exhausted window.
func TestFetchFableUsageWindowsHeaderless429ReturnsNoWindows(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`))
	}))
	defer server.Close()
	restore := messagesURL
	messagesURL = server.URL + "/v1/messages"
	defer func() { messagesURL = restore }()

	windows, err := FetchFableUsageWindows(context.Background(), server.Client(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 0 {
		t.Fatalf("windows = %+v, want none for a headerless 429", windows)
	}
}
