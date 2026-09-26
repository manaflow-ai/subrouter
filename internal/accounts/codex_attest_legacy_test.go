package accounts

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestAttestStoredLegacyOAuthRotatesAndMarksServerAttested(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir(), DisableActiveAuthSync: true, RequireIsolatedOAuth: true}
	legacy := storedOAuthAccount("legacy@example.com", "legacy", time.Now().Add(time.Hour))
	if err := store.SaveStored(legacy); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return refreshResponse("rotated", "legacy@example.com", time.Now().Add(time.Hour)), nil
	})}

	attested, err := store.AttestStoredLegacyOAuth(context.Background(), client, legacy.Email)
	if err != nil {
		t.Fatalf("attest: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls.Load())
	}
	if attested.OAuthCredentialOrigin != CodexOAuthOriginServerAttested {
		t.Fatalf("origin = %q, want %q", attested.OAuthCredentialOrigin, CodexOAuthOriginServerAttested)
	}
	if attested.Auth.Tokens.RefreshToken != "rotated-refresh" {
		t.Fatal("attested record does not carry the rotated refresh token")
	}

	stored, found, err := store.FindStored(legacy.Email)
	if err != nil || !found {
		t.Fatalf("stored account missing: found=%v err=%v", found, err)
	}
	if stored.OAuthCredentialOrigin != CodexOAuthOriginServerAttested || stored.Auth.Tokens.RefreshToken != "rotated-refresh" {
		t.Fatalf("stored record not updated: origin=%q rotated=%v", stored.OAuthCredentialOrigin, stored.Auth.Tokens.RefreshToken == "rotated-refresh")
	}
	// The serving gate must now accept the record without a further refresh.
	if reason, gateErr := store.validateServingCredentialIsolation(stored); gateErr != nil {
		t.Fatalf("serving gate still rejects attested record: %s: %v", reason, gateErr)
	}
}

func TestAttestStoredLegacyOAuthSkipsAlreadyIsolatedRecords(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir(), DisableActiveAuthSync: true, RequireIsolatedOAuth: true}
	for _, origin := range []CodexOAuthCredentialOrigin{CodexOAuthOriginIsolatedServerLogin, CodexOAuthOriginServerAttested} {
		account := storedOAuthAccount(string(origin)+"@example.com", "isolated", time.Now().Add(time.Hour))
		account.OAuthCredentialOrigin = origin
		if err := store.SaveStored(account); err != nil {
			t.Fatal(err)
		}
		client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("isolated record must not be refreshed")
			return nil, nil
		})}
		_, err := store.AttestStoredLegacyOAuth(context.Background(), client, account.Email)
		if !errors.Is(err, ErrCodexCredentialAlreadyIsolated) {
			t.Fatalf("origin %q: err = %v, want ErrCodexCredentialAlreadyIsolated", origin, err)
		}
	}
}

func TestAttestStoredLegacyOAuthRefusesChainSharedWithInteractiveAuth(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir(), DisableActiveAuthSync: true, RequireIsolatedOAuth: true}
	shared := storedOAuthAccount("shared@example.com", "shared", time.Now().Add(time.Hour))
	if err := store.SaveStored(shared); err != nil {
		t.Fatal(err)
	}
	if err := WriteActiveCodexAuth(shared.Auth); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("shared chain must not be rotated")
		return nil, nil
	})}
	_, err := store.AttestStoredLegacyOAuth(context.Background(), client, shared.Email)
	if !errors.Is(err, ErrCodexCredentialSharesActiveAuth) {
		t.Fatalf("err = %v, want ErrCodexCredentialSharesActiveAuth", err)
	}
	stored, _, _ := store.FindStored(shared.Email)
	if stored.OAuthCredentialOrigin != "" || stored.Auth.Tokens.RefreshToken != "shared-refresh" {
		t.Fatalf("shared record was modified: origin=%q unchanged=%v", stored.OAuthCredentialOrigin, stored.Auth.Tokens.RefreshToken == "shared-refresh")
	}
}

func TestAttestStoredLegacyOAuthLeavesRecordUntouchedWhenProviderRejects(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir(), DisableActiveAuthSync: true, RequireIsolatedOAuth: true}
	legacy := storedOAuthAccount("dead@example.com", "dead", time.Now().Add(time.Hour))
	if err := store.SaveStored(legacy); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusBadRequest, `{"error":"invalid_grant"}`), nil
	})}
	_, err := store.AttestStoredLegacyOAuth(context.Background(), client, legacy.Email)
	var refreshErr *CodexAuthRefreshError
	if !errors.As(err, &refreshErr) {
		t.Fatalf("err = %v, want CodexAuthRefreshError", err)
	}
	stored, _, _ := store.FindStored(legacy.Email)
	if stored.OAuthCredentialOrigin != "" || stored.Auth.Tokens.RefreshToken != "dead-refresh" {
		t.Fatalf("rejected record was modified: origin=%q unchanged=%v", stored.OAuthCredentialOrigin, stored.Auth.Tokens.RefreshToken == "dead-refresh")
	}
	if len(stored.Breadcrumbs) != len(legacy.Breadcrumbs) {
		t.Fatalf("rejected record gained breadcrumbs: %d, want %d", len(stored.Breadcrumbs), len(legacy.Breadcrumbs))
	}
}

func TestAttestStoredLegacyOAuthRequiresRotation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir(), DisableActiveAuthSync: true, RequireIsolatedOAuth: true}
	legacy := storedOAuthAccount("static@example.com", "static", time.Now().Add(time.Hour))
	if err := store.SaveStored(legacy); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		// Same prefix produces the same "static-refresh" refresh token.
		return refreshResponse("static", "static@example.com", time.Now().Add(2*time.Hour)), nil
	})}
	_, err := store.AttestStoredLegacyOAuth(context.Background(), client, legacy.Email)
	if err == nil {
		t.Fatal("expected rotation error")
	}
	stored, _, _ := store.FindStored(legacy.Email)
	if stored.OAuthCredentialOrigin != "" {
		t.Fatalf("unrotated record was marked attested: %q", stored.OAuthCredentialOrigin)
	}
}

func TestAttestStoredLegacyOAuthFailsClosedWhenInteractiveAuthIsUnreadable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir(), DisableActiveAuthSync: true, RequireIsolatedOAuth: true}
	legacy := storedOAuthAccount("legacy@example.com", "legacy", time.Now().Add(time.Hour))
	if err := store.SaveStored(legacy); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(DefaultCodexAuthPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DefaultCodexAuthPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("must not rotate when interactive auth cannot be read")
		return nil, nil
	})}
	if _, err := store.AttestStoredLegacyOAuth(context.Background(), client, legacy.Email); err == nil {
		t.Fatal("expected an error when interactive auth is unreadable")
	}
	stored, _, _ := store.FindStored(legacy.Email)
	if stored.OAuthCredentialOrigin != "" {
		t.Fatalf("record was attested despite unreadable interactive auth: %q", stored.OAuthCredentialOrigin)
	}
}
