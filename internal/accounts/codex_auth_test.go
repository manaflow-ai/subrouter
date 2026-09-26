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
	"path/filepath"
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

func TestRefreshStoredIfExpiredLeavesActiveAuthAloneWhenSyncDisabled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := CodexStore{Dir: t.TempDir(), DisableActiveAuthSync: true}
	stale := storedOAuthAccount("founders@example.com", "old", time.Now().Add(-time.Hour))
	stale.OAuthCredentialOrigin = CodexOAuthOriginIsolatedServerLogin
	if err := store.SaveStored(stale); err != nil {
		t.Fatal(err)
	}
	interactive := storedOAuthAccount("founders@example.com", "interactive", time.Now().Add(-time.Hour))
	if err := WriteActiveCodexAuth(interactive.Auth); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return refreshResponse("new", "founders@example.com", time.Now().Add(time.Hour)), nil
	})}

	refreshed, _, err := store.RefreshStoredIfExpired(context.Background(), client, stale)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Auth.Tokens.RefreshToken != "new-refresh" {
		t.Fatalf("stored refresh token = %q, want new-refresh", refreshed.Auth.Tokens.RefreshToken)
	}
	active, ok, err := ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing active auth")
	}
	if active.Tokens.RefreshToken != "interactive-refresh" {
		t.Fatalf("active refresh token = %q, want interactive-refresh", active.Tokens.RefreshToken)
	}
}

func TestRefreshStoredIfExpiredDoesNotSyncIsolatedCredentialToActiveAuth(t *testing.T) {
	for _, origin := range []CodexOAuthCredentialOrigin{
		CodexOAuthOriginIsolatedServerLogin,
		CodexOAuthOriginServerAttested,
	} {
		t.Run(string(origin), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			store := CodexStore{Dir: t.TempDir()}
			stale := storedOAuthAccount("founders@example.com", "stored-old", time.Now().Add(-time.Hour))
			stale.OAuthCredentialOrigin = origin
			if err := store.SaveStored(stale); err != nil {
				t.Fatal(err)
			}
			interactive := storedOAuthAccount("founders@example.com", "interactive", time.Now().Add(time.Hour))
			if err := WriteActiveCodexAuth(interactive.Auth); err != nil {
				t.Fatal(err)
			}
			activePath := DefaultCodexAuthPath()
			before, err := os.ReadFile(activePath)
			if err != nil {
				t.Fatal(err)
			}

			client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return refreshResponse("stored-new", "founders@example.com", time.Now().Add(time.Hour)), nil
			})}
			refreshed, didRefresh, err := store.RefreshStoredIfExpired(context.Background(), client, stale)
			if err != nil {
				t.Fatal(err)
			}
			if !didRefresh || refreshed.Auth.Tokens.RefreshToken != "stored-new-refresh" {
				t.Fatalf("stored credential was not refreshed: didRefresh=%v account=%#v", didRefresh, refreshed)
			}

			after, err := os.ReadFile(activePath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Fatalf("active auth changed while refreshing %s credential\nbefore: %s\nafter:  %s", origin, before, after)
			}
		})
	}
}

func TestRefreshStoredDoesNotRotateSharedInteractiveCredentialWhenSyncDisabled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir(), DisableActiveAuthSync: true}
	stale := storedOAuthAccount("founders@example.com", "shared", time.Now().Add(-time.Hour))
	if err := store.SaveStored(stale); err != nil {
		t.Fatal(err)
	}
	if err := WriteActiveCodexAuth(stale.Auth); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return refreshResponse("new", "founders@example.com", time.Now().Add(time.Hour)), nil
	})}

	_, refreshed, err := store.RefreshStored(context.Background(), client, stale)
	var unisolated *CodexUnisolatedCredentialError
	if !errors.As(err, &unisolated) {
		t.Fatalf("refresh error = %v, want CodexUnisolatedCredentialError", err)
	}
	if refreshed {
		t.Fatal("shared credential was refreshed")
	}
	if calls.Load() != 0 {
		t.Fatalf("refresh calls = %d, want 0", calls.Load())
	}
}

func TestRefreshStoredRejectsFreshUnisolatedCredentialBeforeUse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{
		Dir: t.TempDir(), DisableActiveAuthSync: true, RequireIsolatedOAuth: true,
	}
	fresh := storedOAuthAccount("founders@example.com", "legacy", time.Now().Add(time.Hour))
	if err := store.SaveStored(fresh); err != nil {
		t.Fatal(err)
	}

	_, refreshed, err := store.RefreshStoredIfExpired(context.Background(), nil, fresh)
	var unisolated *CodexUnisolatedCredentialError
	if !errors.As(err, &unisolated) {
		t.Fatalf("refresh error = %v, want CodexUnisolatedCredentialError", err)
	}
	if refreshed {
		t.Fatal("fresh unisolated credential was accepted")
	}
}

func TestRefreshStoredAcceptsIsolatedCredentialWhenInteractiveAuthIsUnreadable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{
		Dir: t.TempDir(), DisableActiveAuthSync: true, RequireIsolatedOAuth: true,
	}
	fresh := storedOAuthAccount("founders@example.com", "isolated", time.Now().Add(time.Hour))
	fresh.OAuthCredentialOrigin = CodexOAuthOriginIsolatedServerLogin
	if err := store.SaveStored(fresh); err != nil {
		t.Fatal(err)
	}
	activePath := DefaultCodexAuthPath()
	if err := os.MkdirAll(filepath.Dir(activePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(activePath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, refreshed, err := store.RefreshStoredIfExpired(context.Background(), nil, fresh)
	if err != nil {
		t.Fatalf("isolated credential rejected because interactive auth is unreadable: %v", err)
	}
	if refreshed {
		t.Fatal("fresh isolated credential unexpectedly refreshed")
	}
	if got.Auth.Tokens == nil || got.Auth.Tokens.RefreshToken != fresh.Auth.Tokens.RefreshToken {
		t.Fatalf("credential changed: got %#v", got.Auth.Tokens)
	}
}

func TestRefreshStoredAcceptsServerAttestedCredential(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{
		Dir: t.TempDir(), DisableActiveAuthSync: true, RequireIsolatedOAuth: true,
	}
	fresh := storedOAuthAccount("founders@example.com", "attested", time.Now().Add(time.Hour))
	fresh.OAuthCredentialOrigin = CodexOAuthOriginServerAttested
	if err := store.SaveStored(fresh); err != nil {
		t.Fatal(err)
	}
	got, refreshed, err := store.RefreshStoredIfExpired(context.Background(), nil, fresh)
	if err != nil {
		t.Fatalf("server-attested credential rejected: %v", err)
	}
	if refreshed || got.OAuthCredentialOrigin != CodexOAuthOriginServerAttested {
		t.Fatalf("server-attested credential changed unexpectedly: refreshed=%v account=%#v", refreshed, got)
	}
}

func TestRefreshStoredReloadsFreshCredentialBeforeIsolationDecision(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{
		Dir: t.TempDir(), DisableActiveAuthSync: true, RequireIsolatedOAuth: true,
	}
	staleView := storedOAuthAccount("founders@example.com", "isolated", time.Now().Add(time.Hour))
	staleView.OAuthCredentialOrigin = CodexOAuthOriginIsolatedServerLogin
	if err := store.SaveStored(staleView); err != nil {
		t.Fatal(err)
	}
	exported := staleView
	exported.OAuthCredentialOrigin = CodexOAuthOriginInteractiveImport
	if err := store.SaveStored(exported); err != nil {
		t.Fatal(err)
	}

	_, refreshed, err := store.RefreshStoredIfExpired(context.Background(), nil, staleView)
	var unisolated *CodexUnisolatedCredentialError
	if !errors.As(err, &unisolated) {
		t.Fatalf("refresh error = %v, want CodexUnisolatedCredentialError", err)
	}
	if refreshed {
		t.Fatal("stale isolated view was accepted after stored export")
	}
}

func TestRefreshStoredDoesNotTrustDifferentTokenGenerationWithoutIsolatedProvenance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir(), DisableActiveAuthSync: true, RequireIsolatedOAuth: true}
	stored := storedOAuthAccount("founders@example.com", "older-generation", time.Now().Add(-time.Hour))
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	interactive := storedOAuthAccount("founders@example.com", "newer-generation", time.Now().Add(time.Hour))
	if err := WriteActiveCodexAuth(interactive.Auth); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return refreshResponse("new", "founders@example.com", time.Now().Add(time.Hour)), nil
	})}

	_, _, err := store.RefreshStored(context.Background(), client, stored)
	var unisolated *CodexUnisolatedCredentialError
	if !errors.As(err, &unisolated) {
		t.Fatalf("refresh error = %v, want CodexUnisolatedCredentialError", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("refresh calls = %d, want 0", calls.Load())
	}
}

func TestImportActiveClearsIsolatedServerLoginOrigin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir()}
	isolated := storedOAuthAccount("founders@example.com", "isolated", time.Now().Add(time.Hour))
	isolated.OAuthCredentialOrigin = CodexOAuthOriginIsolatedServerLogin
	if err := store.SaveStored(isolated); err != nil {
		t.Fatal(err)
	}
	interactive := storedOAuthAccount("founders@example.com", "interactive", time.Now().Add(time.Hour))
	if err := WriteActiveCodexAuth(interactive.Auth); err != nil {
		t.Fatal(err)
	}

	imported, existed, err := store.ImportActive()
	if err != nil {
		t.Fatal(err)
	}
	if !existed {
		t.Fatal("existing account was not replaced")
	}
	if imported.OAuthCredentialOrigin != CodexOAuthOriginInteractiveImport {
		t.Fatalf("OAuth origin = %q, want interactive import", imported.OAuthCredentialOrigin)
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

func TestSyncActiveToStoreBeforeSaveStopsBeforeCredentialMutation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir()}
	stored := storedOAuthAccount("founders@example.com", "stored", time.Now().Add(time.Hour))
	stored.OAuthCredentialOrigin = CodexOAuthOriginIsolatedServerLogin
	interactive := storedOAuthAccount("founders@example.com", "interactive", time.Now().Add(time.Hour))
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	if err := WriteActiveCodexAuth(interactive.Auth); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("publish generation")
	err := store.SyncActiveToStoreBeforeSave(func() error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("sync error = %v, want %v", err, wantErr)
	}
	got, found, err := store.FindStored(stored.Email)
	if err != nil || !found {
		t.Fatalf("stored account found = %v, err = %v", found, err)
	}
	if got.OAuthCredentialOrigin != CodexOAuthOriginIsolatedServerLogin {
		t.Fatalf("stored OAuth origin = %q, want isolated server login", got.OAuthCredentialOrigin)
	}
	if got.Auth.Tokens == nil || got.Auth.Tokens.RefreshToken != stored.Auth.Tokens.RefreshToken {
		t.Fatal("stored credential changed before publication")
	}
}

func TestRefreshStoredIfExpiredBeforeRefreshStopsBeforeTokenRedemption(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}
	account := storedOAuthAccount("founders@example.com", "expired", time.Now().Add(-time.Hour))
	if err := store.SaveStored(account); err != nil {
		t.Fatal(err)
	}
	requests := 0
	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("token endpoint should not be called")
	})}
	wantErr := errors.New("publish generation")
	_, refreshed, err := store.RefreshStoredIfExpiredBeforeRefresh(
		context.Background(), client, account, func() error { return wantErr },
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("refresh error = %v, want %v", err, wantErr)
	}
	if refreshed {
		t.Fatal("refresh reported success despite publication failure")
	}
	if requests != 0 {
		t.Fatalf("token endpoint requests = %d, want zero", requests)
	}
	got, found, err := store.FindStored(account.Email)
	if err != nil || !found {
		t.Fatalf("stored account found = %v, err = %v", found, err)
	}
	if got.Auth.Tokens == nil || got.Auth.Tokens.RefreshToken != account.Auth.Tokens.RefreshToken {
		t.Fatal("stored credential changed before publication")
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
		"exp":                         exp.Unix(),
		"iat":                         time.Now().Add(-time.Minute).Unix(),
		"jti":                         jwtID,
		"https://api.openai.com/auth": map[string]any{"chatgpt_user_id": "user:" + email, "chatgpt_account_id": "workspace:" + email},
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

// A locally added API key must be stored against its provider. Storing one
// without a provider made every non-Codex key look like a Codex account, so it
// was selected for Codex and forwarded to the OpenAI upstream — the key's own
// provider was unreachable from the local CLI entirely.
func TestAddProviderAPIKeyScopesTheAccountToItsProvider(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}

	account, existed, err := store.AddProviderAPIKey(ProviderQwenToken, "tokenplan", "sk-sp-qwen-key")
	if err != nil {
		t.Fatalf("add failed: %v", err)
	}
	if existed {
		t.Fatal("a fresh account should not report as existing")
	}
	if account.Provider != ProviderQwenToken {
		t.Fatalf("Provider = %q, want qwen-token", account.Provider)
	}
	if account.Email != "qwen-token:tokenplan" {
		t.Fatalf("Email = %q, want the provider-prefixed identifier", account.Email)
	}

	// Codex keeps the historical apikey: prefix so existing accounts still
	// resolve.
	codex, _, err := store.AddProviderAPIKey(ProviderCodex, "work", "sk-openai-key")
	if err != nil {
		t.Fatalf("codex add failed: %v", err)
	}
	if codex.Email != "apikey:work" || codex.ProviderOrDefault() != ProviderCodex {
		t.Fatalf("codex account = %+v, want apikey:work on the codex provider", codex)
	}

	// An empty provider defaults to Codex, which is what AddAPIKey relies on.
	legacy, _, err := store.AddAPIKey("legacy", "sk-legacy")
	if err != nil {
		t.Fatalf("legacy add failed: %v", err)
	}
	if legacy.Email != "apikey:legacy" || legacy.ProviderOrDefault() != ProviderCodex {
		t.Fatalf("legacy account = %+v, want the codex shape", legacy)
	}

	// Two providers may share a label without colliding.
	other, _, err := store.AddProviderAPIKey(ProviderQwenAnthropic, "tokenplan", "sk-sp-qwen-key")
	if err != nil {
		t.Fatalf("second provider add failed: %v", err)
	}
	if other.Email == account.Email {
		t.Fatal("the same label under two providers must not collide")
	}
	stored, err := store.ListStored()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 4 {
		t.Fatalf("stored %d accounts, want 4: %+v", len(stored), stored)
	}
}

// Only Codex guarantees an sk- prefix. Other providers issue their own formats,
// so requiring sk- there would reject valid keys — xAI's begin with xai-.
func TestAddProviderAPIKeyValidatesTheKeyPerProvider(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}

	if _, _, err := store.AddProviderAPIKey(ProviderCodex, "work", "not-an-sk-key"); err == nil {
		t.Fatal("a Codex key without the sk- prefix must be rejected")
	}
	if _, _, err := store.AddProviderAPIKey(ProviderCodex, "empty", "  "); err == nil || err.Error() != "API key is required" {
		t.Fatalf("empty Codex key error = %v, want missing-key error", err)
	}
	if _, _, err := store.AddProviderAPIKey(ProviderGrok, "grok", "xai-some-key"); err != nil {
		t.Fatalf("a provider with its own key format must be accepted: %v", err)
	}
	if _, _, err := store.AddProviderAPIKey(ProviderGrok, "empty", ""); err == nil {
		t.Fatal("an empty key must be rejected for every provider")
	}
	if _, _, err := store.AddProviderAPIKey(ProviderGrok, "", "xai-some-key"); err == nil {
		t.Fatal("a missing label must be rejected")
	}
}
