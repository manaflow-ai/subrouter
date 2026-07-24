package accounts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRefreshStoredIfExpiredUsesFreshTokenWrittenByWinner(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir()}
	stale := storedOAuthAccount("founders@example.com", "old", time.Now().Add(-time.Hour))
	fresh := storedOAuthAccount("founders@example.com", "new", time.Now().Add(time.Hour))
	if err := store.SaveStored(fresh); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponse(http.StatusInternalServerError, `{}`), nil
	})}

	got, refreshed, err := store.RefreshStoredIfExpired(context.Background(), client, stale)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed {
		t.Fatal("expected refresh to be skipped")
	}
	if calls.Load() != 0 {
		t.Fatalf("refresh calls = %d, want 0", calls.Load())
	}
	if got.Auth.Tokens.RefreshToken != "new-refresh" {
		t.Fatalf("refresh token = %q, want new-refresh", got.Auth.Tokens.RefreshToken)
	}
}

func TestRefreshStoredIfExpiredSerializesConcurrentRefresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir()}
	stale := storedOAuthAccount("founders@example.com", "old", time.Now().Add(-time.Hour))
	if err := store.SaveStored(stale); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return refreshResponse("new", "founders@example.com", time.Now().Add(time.Hour)), nil
	})}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	results := make(chan StoredCodexAccount, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, _, err := store.RefreshStoredIfExpired(context.Background(), client, stale)
			if err != nil {
				errs <- err
				return
			}
			results <- got
		}()
	}
	wg.Wait()
	close(errs)
	close(results)
	for err := range errs {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls.Load())
	}
	for got := range results {
		if got.Auth.Tokens.RefreshToken != "new-refresh" {
			t.Fatalf("refresh token = %q, want new-refresh", got.Auth.Tokens.RefreshToken)
		}
	}
}

func TestRefreshStoredIfExpiredRecoversFromRefreshTokenReuseAfterExternalWrite(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir()}
	stale := storedOAuthAccount("founders@example.com", "old", time.Now().Add(-time.Hour))
	if err := store.SaveStored(stale); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		fresh := storedOAuthAccount("founders@example.com", "new", time.Now().Add(time.Hour))
		writeStoredCodexAccountFile(t, store, fresh)
		return jsonResponse(http.StatusUnauthorized, `{"error":{"code":"refresh_token_reused"}}`), nil
	})}

	got, refreshed, err := store.RefreshStoredIfExpired(context.Background(), client, stale)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed {
		t.Fatal("expected recovery without reporting this process as refreshed")
	}
	if got.Auth.Tokens.RefreshToken != "new-refresh" {
		t.Fatalf("refresh token = %q, want new-refresh", got.Auth.Tokens.RefreshToken)
	}
}

func TestRefreshStoredIfExpiredReturnsProviderRefreshError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir()}
	stale := storedOAuthAccount("founders@example.com", "old", time.Now().Add(-time.Hour))
	if err := store.SaveStored(stale); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusUnauthorized, `{"error":{"message":"invalidated","type":"invalid_request_error","code":"refresh_token_invalidated"}}`), nil
	})}

	_, _, err := store.RefreshStoredIfExpired(context.Background(), client, stale)
	if err == nil {
		t.Fatal("expected refresh error")
	}
	var refreshErr *CodexAuthRefreshError
	if !errors.As(err, &refreshErr) {
		t.Fatalf("error type = %T, want CodexAuthRefreshError", err)
	}
	if refreshErr.StatusCode != http.StatusUnauthorized || refreshErr.ProviderCode != "refresh_token_invalidated" || refreshErr.ProviderType != "invalid_request_error" {
		t.Fatalf("unexpected refresh error: %+v", refreshErr)
	}
}

func TestRefreshStoredIfExpiredCachesTerminalRefreshError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir()}
	stale := storedOAuthAccount("founders@example.com", "old", time.Now().Add(-time.Hour))
	if err := store.SaveStored(stale); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponse(http.StatusUnauthorized, `{"error":{"message":"Your refresh token has already been used to generate a new access token. Please try signing in again.","type":"invalid_request_error","code":"refresh_token_reused"}}`), nil
	})}

	if _, _, err := store.RefreshStoredIfExpired(context.Background(), client, stale); err == nil {
		t.Fatal("expected first refresh error")
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh calls after first attempt = %d, want 1", calls.Load())
	}
	stored, ok, err := store.FindStored("founders@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || stored.Auth.RefreshFailure == nil {
		t.Fatal("missing cached refresh failure")
	}
	if stored.Auth.RefreshFailure.ProviderCode != "refresh_token_reused" {
		t.Fatalf("cached provider code = %q, want refresh_token_reused", stored.Auth.RefreshFailure.ProviderCode)
	}
	if len(stored.Breadcrumbs) != 1 {
		t.Fatalf("breadcrumbs = %d, want 1", len(stored.Breadcrumbs))
	}
	failureCrumb := stored.Breadcrumbs[0]
	if failureCrumb.Event != "refresh_terminal_failure" || failureCrumb.ProviderCode != "refresh_token_reused" || failureCrumb.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unexpected failure breadcrumb: %+v", failureCrumb)
	}
	if failureCrumb.RefreshFP != codexTokenFingerprint("old-refresh") || failureCrumb.OldRefreshFP != codexTokenFingerprint("old-refresh") {
		t.Fatalf("unexpected failure breadcrumb fingerprints: %+v", failureCrumb)
	}

	if _, _, err := store.RefreshStoredIfExpired(context.Background(), client, stale); err == nil {
		t.Fatal("expected cached refresh error")
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh calls after cached attempt = %d, want 1", calls.Load())
	}
}

func TestRefreshStoredIfExpiredDoesNotCacheTransientRefreshError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir()}
	stale := storedOAuthAccount("founders@example.com", "old", time.Now().Add(-time.Hour))
	if err := store.SaveStored(stale); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return jsonResponse(http.StatusInternalServerError, `{}`), nil
		}
		return refreshResponse("new", "founders@example.com", time.Now().Add(time.Hour)), nil
	})}

	if _, _, err := store.RefreshStoredIfExpired(context.Background(), client, stale); err == nil {
		t.Fatal("expected transient refresh error")
	}
	stored, ok, err := store.FindStored("founders@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing stored account")
	}
	if stored.Auth.RefreshFailure != nil {
		t.Fatalf("unexpected cached refresh failure: %+v", stored.Auth.RefreshFailure)
	}

	got, _, err := store.RefreshStoredIfExpired(context.Background(), client, stale)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("refresh calls = %d, want 2", calls.Load())
	}
	if got.Auth.Tokens.RefreshToken != "new-refresh" {
		t.Fatalf("refresh token = %q, want new-refresh", got.Auth.Tokens.RefreshToken)
	}
}

func TestRefreshStoredIfExpiredSyncsActiveAuthWhenActiveAccountRefreshes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := CodexStore{Dir: t.TempDir()}
	stale := storedOAuthAccount("founders@example.com", "old", time.Now().Add(-time.Hour))
	if err := store.SaveStored(stale); err != nil {
		t.Fatal(err)
	}
	if err := WriteActiveCodexAuth(stale.Auth); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return refreshResponse("new", "founders@example.com", time.Now().Add(time.Hour)), nil
	})}

	if _, _, err := store.RefreshStoredIfExpired(context.Background(), client, stale); err != nil {
		t.Fatal(err)
	}
	active, ok, err := ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing active auth")
	}
	if active.Tokens.RefreshToken != "new-refresh" {
		t.Fatalf("active refresh token = %q, want new-refresh", active.Tokens.RefreshToken)
	}
}

func TestRefreshStoredIfExpiredLogsRefreshFingerprints(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir()}
	stale := storedOAuthAccount("founders@example.com", "old", time.Now().Add(-time.Hour))
	if err := store.SaveStored(stale); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previousLogger)

	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return refreshResponse("new", "founders@example.com", time.Now().Add(time.Hour)), nil
	})}

	ctx := WithCodexRefreshReason(context.Background(), "test.refresh")
	if _, _, err := store.RefreshStoredIfExpired(ctx, client, stale); err != nil {
		t.Fatal(err)
	}

	out := logs.String()
	oldRefreshFP := codexTokenFingerprint("old-refresh")
	newRefreshFP := codexTokenFingerprint("new-refresh")
	for _, want := range []string{
		"msg=\"codex oauth refresh start\"",
		"msg=\"codex oauth refresh succeeded\"",
		"reason=test.refresh",
		"refresh_fp=" + oldRefreshFP,
		"old_refresh_fp=" + oldRefreshFP,
		"new_refresh_fp=" + newRefreshFP,
		"host=",
		"pid=",
		"store_dir=",
		"source_path=",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("logs missing %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"old-refresh", "new-refresh"} {
		if strings.Contains(out, secret) {
			t.Fatalf("logs leaked refresh token %q:\n%s", secret, out)
		}
	}

	stored, ok, err := store.FindStored("founders@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing stored account")
	}
	if len(stored.Breadcrumbs) != 1 {
		t.Fatalf("breadcrumbs = %d, want 1", len(stored.Breadcrumbs))
	}
	crumb := stored.Breadcrumbs[0]
	if crumb.Event != "refresh_succeeded" || crumb.Source != "oauth_refresh" || crumb.Reason != "test.refresh" {
		t.Fatalf("unexpected breadcrumb identity: %+v", crumb)
	}
	if crumb.OldRefreshFP != oldRefreshFP || crumb.NewRefreshFP != newRefreshFP || crumb.RefreshFP != newRefreshFP {
		t.Fatalf("unexpected breadcrumb fingerprints: %+v", crumb)
	}
	if crumb.Host == "" || crumb.PID == 0 || crumb.PPID == 0 || crumb.Executable == "" || crumb.StoreDir == "" || crumb.SourcePath == "" {
		t.Fatalf("breadcrumb missing process/store context: %+v", crumb)
	}
	crumbBody, err := json.Marshal(crumb)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"old-refresh", "new-refresh"} {
		if strings.Contains(string(crumbBody), secret) {
			t.Fatalf("breadcrumb leaked refresh token %q:\n%s", secret, string(crumbBody))
		}
	}
}

func TestSyncActiveToStoreDoesNotOverwriteNewerStoredToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir()}
	stale := storedOAuthAccount("founders@example.com", "old", time.Now().Add(-time.Hour))
	fresh := storedOAuthAccount("founders@example.com", "new", time.Now().Add(time.Hour))
	if err := store.SaveStored(fresh); err != nil {
		t.Fatal(err)
	}
	if err := WriteActiveCodexAuth(stale.Auth); err != nil {
		t.Fatal(err)
	}

	if err := store.SyncActiveToStore(); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.FindStored("founders@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("missing stored account")
	}
	if got.Auth.Tokens.RefreshToken != "new-refresh" {
		t.Fatalf("stored refresh token = %q, want new-refresh", got.Auth.Tokens.RefreshToken)
	}
}

type codexRoundTripFunc func(*http.Request) (*http.Response, error)

func (f codexRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func storedOAuthAccount(email, tokenPrefix string, exp time.Time) StoredCodexAccount {
	return StoredCodexAccount{
		Email:   email,
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth: CodexAuthFile{AuthMode: "chatgpt", Tokens: &CodexTokens{
			AccessToken:  testCodexJWT(email, tokenPrefix+"-access", exp),
			RefreshToken: tokenPrefix + "-refresh",
			IDToken:      testCodexJWT(email, tokenPrefix+"-id", exp),
		}},
	}
}

func writeStoredCodexAccountFile(t *testing.T, store CodexStore, account StoredCodexAccount) {
	t.Helper()
	if err := os.MkdirAll(store.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(account, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(account.SourcePath(store), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func refreshResponse(tokenPrefix, email string, exp time.Time) *http.Response {
	body, _ := json.Marshal(map[string]string{
		"access_token":  testCodexJWT(email, tokenPrefix+"-access", exp),
		"refresh_token": tokenPrefix + "-refresh",
		"id_token":      testCodexJWT(email, tokenPrefix+"-id", exp),
	})
	return jsonResponse(http.StatusOK, string(body))
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testCodexJWT(email, jwtID string, exp time.Time) string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(map[string]any{
		"exp": exp.Unix(),
		"iat": time.Now().Add(-time.Minute).Unix(),
		"jti": jwtID,
		"https://api.openai.com/profile": map[string]any{
			"email": email,
		},
	})
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestAccountMismatchMessageIsTerminalRefreshFailure(t *testing.T) {
	account := StoredCodexAccount{
		Auth: CodexAuthFile{
			RefreshFailure: &CodexRefreshFailure{
				ProviderMessage: "Your access token could not be refreshed because you have since logged out or signed in to another account. Please sign in again.",
			},
		},
	}
	if err := terminalStoredRefreshFailure(account); err == nil {
		t.Fatal("expected account-mismatch refresh failure to be terminal")
	}
}
