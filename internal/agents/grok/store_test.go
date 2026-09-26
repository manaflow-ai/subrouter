package grok

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

var reference = time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

func TestDefaultStoreUsesConfiguredStateRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", root)
	want := filepath.Join(root, "grok", "oauth.json")
	if got := DefaultStore().credentialPath(); got != want {
		t.Fatalf("credential path = %q, want %q", got, want)
	}
}

func TestParseCredentialReadsTheStoredShape(t *testing.T) {
	credential, err := ParseCredential([]byte(
		`{"access_token":"at","refresh_token":"rt","expires_at":1787144400,"scope":"`+oauthScope+`","token_type":"Bearer","email":"dev@example.com"}`),
		"test", reference)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if credential.AccessToken != "at" || credential.RefreshToken != "rt" {
		t.Fatalf("tokens did not round-trip: %+v", credential)
	}
	if want := time.Unix(1787144400, 0).UTC(); !credential.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %s, want %s", credential.ExpiresAt, want)
	}
	if credential.Email != "dev@example.com" {
		t.Fatalf("Email = %q, want the stored identity", credential.Email)
	}
}

// expires_in is relative, so it only means anything against a clock.
func TestParseCredentialFallsBackToRelativeExpiry(t *testing.T) {
	credential, err := ParseCredential([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600}`), "test", reference)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if want := reference.Add(3600 * time.Second); !credential.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %s, want %s", credential.ExpiresAt, want)
	}
}

// An unparseable expiry must fail loudly. Falling back to a zero value would
// silently mark every credential expired and refresh on every single request.
func TestParseCredentialRejectsAnUnreadableExpiry(t *testing.T) {
	_, err := ParseCredential([]byte(`{"access_token":"at","refresh_token":"rt","expires_at":"tomorrow"}`), "test", reference)
	if err == nil {
		t.Fatal("a non-numeric expires_at must be an error, not a silent zero")
	}
	if !strings.Contains(err.Error(), unreadableCredentialPhrase) {
		t.Fatalf("error %q should carry the unreadable-credential phrase so the proxy classifies it terminal", err)
	}
}

func TestParseCredentialRejectsABlobWithNoTokens(t *testing.T) {
	if _, err := ParseCredential([]byte(`{"token_type":"Bearer"}`), "test", reference); err == nil {
		t.Fatal("a credential with neither token must be rejected")
	}
}

// The decode error must name its source and shape, and must not echo the blob.
func TestParseCredentialReportsShapeWithoutLeaking(t *testing.T) {
	body := []byte(`{"access_token":"eyJ.secret-value","refresh_token":"eyJ.secret-refresh"}` + "bplist00")
	_, err := ParseCredential(body, "oauth.json", reference)
	if err == nil {
		t.Fatal("trailing bytes must not decode")
	}
	message := err.Error()
	for _, want := range []string{unreadableCredentialPhrase, "from oauth.json", "trailing_kind=binary-plist"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q is missing %q", message, want)
		}
	}
	for _, secret := range []string{"eyJ.secret-value", "eyJ.secret-refresh"} {
		if strings.Contains(message, secret) {
			t.Fatalf("error leaked a secret: %q", message)
		}
	}
}

func TestNeedsRefresh(t *testing.T) {
	live := CredentialInfo{AccessToken: "at", ExpiresAt: reference.Add(10 * time.Minute)}
	if live.NeedsRefresh(reference) {
		t.Fatal("a token with 10 minutes left does not need a refresh")
	}
	soon := CredentialInfo{AccessToken: "at", ExpiresAt: reference.Add(4 * time.Minute)}
	if !soon.NeedsRefresh(reference) {
		t.Fatal("a token inside the five-minute lead must be refreshed before use")
	}
	unknown := CredentialInfo{AccessToken: "at"}
	if !unknown.NeedsRefresh(reference) {
		t.Fatal("a credential with no stated expiry must be refreshed rather than trusted")
	}
}

// writeCredentialFile seeds a credential file for a test store.
func writeCredentialFile(t *testing.T, payload string) Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oauth.json")
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	return Store{Path: path}
}

func credentialFileJSON(accessToken, refreshToken string, expiresAt time.Time) string {
	return `{"access_token":"` + accessToken + `","refresh_token":"` + refreshToken +
		`","expires_at":` + strconv.FormatInt(expiresAt.Unix(), 10) + `,"token_type":"Bearer"}`
}

// stubDiscovery points the endpoint cache at a test server, bypassing the real
// OIDC document (and its x.ai host validation, which a loopback stub cannot
// satisfy).
func stubDiscovery(t *testing.T, deviceURL, tokenURL string) {
	t.Helper()
	discoveryMu.Lock()
	restore := discoveryCache
	discoveryCache = &discovery{DeviceAuthURL: deviceURL, TokenURL: tokenURL}
	discoveryMu.Unlock()
	t.Cleanup(func() {
		discoveryMu.Lock()
		discoveryCache = restore
		discoveryMu.Unlock()
	})
}

func TestListAccountsReportsSignedOutWhenTheFileIsAbsent(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "oauth.json")}
	accounts, err := store.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("got %+v, want no accounts without a credential file", accounts)
	}
}

func TestListAccountsSurfacesTheStoredCredential(t *testing.T) {
	store := writeCredentialFile(t, credentialFileJSON("at", "rt", time.Now().Add(time.Hour)))
	accounts, err := store.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("got %d accounts, want 1", len(accounts))
	}
	acct := accounts[0]
	if acct.Provider != account.ProviderGrok || acct.AuthMode != account.AuthModeOAuth {
		t.Fatalf("account identity = %s/%s, want grok/oauth", acct.Provider, acct.AuthMode)
	}
	if acct.Token != "at" {
		t.Fatalf("Token = %q, want the access token", acct.Token)
	}
}

func TestSaveCredentialPersistsACompleteFreshCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grok", "oauth.json")
	store := Store{Path: path}
	want := CredentialInfo{
		AccessToken: "access", RefreshToken: "refresh", IDToken: "identity",
		ExpiresAt: time.Now().Add(time.Hour), TokenType: "Bearer", Email: "dev@example.com",
	}
	savedAccount, err := store.SaveCredential(want)
	if err != nil {
		t.Fatal(err)
	}
	if savedAccount.Email != want.Email || savedAccount.Token != want.AccessToken || savedAccount.AuthMode != account.AuthModeOAuth {
		t.Fatalf("saved account = %+v", savedAccount)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("credential permissions = %o, want 600", got)
	}
	stored, ok, err := store.ReadLocalCredential(time.Now())
	if err != nil || !ok {
		t.Fatalf("read after save: ok=%v err=%v", ok, err)
	}
	if stored.AccessToken != want.AccessToken || stored.RefreshToken != want.RefreshToken || stored.Email != want.Email {
		t.Fatalf("stored credential does not match the input: %+v", stored)
	}
}

func TestSaveCredentialRejectsIncompleteOrExpiredCredentials(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "oauth.json")}
	tests := []struct {
		name       string
		credential CredentialInfo
	}{
		{name: "missing access", credential: CredentialInfo{RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour)}},
		{name: "missing refresh", credential: CredentialInfo{AccessToken: "access", ExpiresAt: time.Now().Add(time.Hour)}},
		{name: "expired", credential: CredentialInfo{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Minute)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := store.SaveCredential(test.credential); err == nil {
				t.Fatal("invalid credential was persisted")
			}
		})
	}
	if _, err := os.Stat(store.credentialPath()); !os.IsNotExist(err) {
		t.Fatalf("invalid input created a credential file: %v", err)
	}
}

func TestRefreshAccountKeepsAFreshCredential(t *testing.T) {
	store := writeCredentialFile(t, credentialFileJSON("at", "rt", time.Now().Add(time.Hour)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a credential with life left must not be refreshed")
		http.Error(w, "unexpected refresh", http.StatusInternalServerError)
	}))
	defer server.Close()
	stubDiscovery(t, server.URL, server.URL)

	refreshed, err := store.RefreshAccount(context.Background(), server.Client(), account.Account{ID: accountID, Provider: account.ProviderGrok})
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if refreshed.Token != "at" {
		t.Fatalf("Token = %q, want the still-valid access token", refreshed.Token)
	}
}

// The refresh must write the rotated tokens back over the credential file, or
// a restart resurrects a dead refresh token.
func TestRefreshAccountRefreshesAndWritesBack(t *testing.T) {
	store := writeCredentialFile(t, credentialFileJSON("stale", "rt", time.Now().Add(-time.Hour)))
	transactionCalled := false
	store.RefreshTransaction = func(_ context.Context, mutate func() error) error {
		transactionCalled = true
		return mutate()
	}
	var gotForm string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error("refresh request form could not be parsed")
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		gotForm = r.Form.Encode()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-access",
			"refresh_token": "rotated-refresh",
			"expires_in":    3600,
			"token_type":    "Bearer",
		})
	}))
	defer server.Close()
	stubDiscovery(t, server.URL, server.URL)

	expired := account.Account{ID: accountID, Provider: account.ProviderGrok, Token: "stale"}
	refreshed, err := store.RefreshAccount(context.Background(), server.Client(), expired)
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if refreshed.Token != "fresh-access" {
		t.Fatalf("Token = %q, want the refreshed value", refreshed.Token)
	}
	if !transactionCalled {
		t.Fatal("refresh bypassed the configured cross-process transaction")
	}
	for _, want := range []string{"grant_type=refresh_token", "refresh_token=rt", "client_id=" + oauthClientID} {
		if !strings.Contains(gotForm, want) {
			t.Fatal("refresh request form is missing an expected field")
		}
	}

	credential, ok, err := store.ReadLocalCredential(time.Now())
	if err != nil || !ok {
		t.Fatalf("re-read failed: ok=%v err=%v", ok, err)
	}
	if credential.AccessToken != "fresh-access" || credential.RefreshToken != "rotated-refresh" {
		t.Fatalf("written credential = %+v, want the rotated pair", credential)
	}
	if credential.ExpiresAt.IsZero() {
		t.Fatal("written credential lost its expiry")
	}
}

func TestRemoveCredentialDeletesTheStoredSubscription(t *testing.T) {
	store := writeCredentialFile(t, credentialFileJSON("access", "refresh", time.Now().Add(time.Hour)))
	removed, ok, err := store.RemoveCredential()
	if err != nil || !ok {
		t.Fatalf("remove: ok=%v err=%v", ok, err)
	}
	if removed.ID != accountID || removed.Provider != account.ProviderGrok {
		t.Fatalf("removed account = %+v", removed)
	}
	if _, err := os.Stat(store.credentialPath()); !os.IsNotExist(err) {
		t.Fatalf("credential remains after removal: %v", err)
	}
	if _, ok, err := store.RemoveCredential(); err != nil || ok {
		t.Fatalf("second removal: ok=%v err=%v", ok, err)
	}
}

func TestRemoveCredentialDeletesAnUnreadableCredential(t *testing.T) {
	store := writeCredentialFile(t, "not-json")
	removed, ok, err := store.RemoveCredential()
	if err != nil || !ok {
		t.Fatalf("remove unreadable credential: ok=%v err=%v", ok, err)
	}
	if removed.ID != accountID || removed.Provider != account.ProviderGrok {
		t.Fatalf("generic removed account = %+v", removed)
	}
	if _, err := os.Stat(store.credentialPath()); !os.IsNotExist(err) {
		t.Fatalf("unreadable credential remains after removal: %v", err)
	}
}

// A refresh response that omits the refresh token must not blank the stored
// one.
func TestRefreshAccountKeepsAnUnrotatedRefreshToken(t *testing.T) {
	store := writeCredentialFile(t, credentialFileJSON("stale", "rt", time.Now().Add(-time.Hour)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh-access", "expires_in": 3600})
	}))
	defer server.Close()
	stubDiscovery(t, server.URL, server.URL)

	if _, err := store.RefreshAccount(context.Background(), server.Client(), account.Account{ID: accountID}); err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	credential, _, err := store.ReadLocalCredential(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if credential.RefreshToken != "rt" {
		t.Fatalf("RefreshToken = %q, want the existing token preserved", credential.RefreshToken)
	}
}

func TestRefreshAccountUsesProviderLifetimeWhenResponseOmitsExpiresIn(t *testing.T) {
	wantExpiry := time.Now().Add(2 * time.Minute).Truncate(time.Second)
	store := writeCredentialFile(t, credentialFileJSON("stale", "rt", wantExpiry))
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		// Deliberately omit expires_in: this is the provider response shape
		// which previously erased the stored absolute expiry.
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh-access"})
	}))
	defer server.Close()
	stubDiscovery(t, server.URL, server.URL)

	if _, err := store.RefreshAccount(t.Context(), server.Client(), account.Account{ID: accountID}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RefreshAccount(t.Context(), server.Client(), account.Account{ID: accountID}); err != nil {
		t.Fatal(err)
	}
	credential, ok, err := store.ReadLocalCredential(time.Now())
	if err != nil || !ok {
		t.Fatalf("read refreshed credential: ok=%v err=%v", ok, err)
	}
	if !credential.ExpiresAt.After(time.Now().Add(refreshLead)) {
		t.Fatalf("ExpiresAt = %s, want fallback outside the refresh lead", credential.ExpiresAt)
	}
	if requests != 1 {
		t.Fatalf("refresh requests = %d, want one across two calls", requests)
	}
	if got := refreshedExpiry(reference.Add(2*time.Hour), time.Time{}, reference); !got.Equal(reference.Add(2 * time.Hour)) {
		t.Fatalf("longer prior expiry = %s, want it preserved", got)
	}
}

func TestRefreshAccountReportsCommittedRotationWhenTransactionTeardownFails(t *testing.T) {
	store := writeCredentialFile(t, credentialFileJSON("stale", "old-refresh", time.Now().Add(-time.Hour)))
	teardownFailure := errors.New("injected transaction teardown failure")
	store.RefreshTransaction = func(_ context.Context, mutate func() error) error {
		if err := mutate(); err != nil {
			return err
		}
		return teardownFailure
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fresh", "refresh_token": "rotated", "expires_in": 3600,
		})
	}))
	defer server.Close()
	stubDiscovery(t, server.URL, server.URL)

	refreshed, didRefresh, err := store.RefreshAccountIfNeeded(
		t.Context(), server.Client(), account.Account{ID: accountID, Provider: account.ProviderGrok, Token: "stale"},
	)
	if !errors.Is(err, teardownFailure) {
		t.Fatalf("refresh error = %v, want teardown failure", err)
	}
	if !didRefresh || refreshed.Token != "fresh" {
		t.Fatalf("refresh result = account:%+v didRefresh:%v, want committed fresh token", refreshed, didRefresh)
	}
	stored, ok, readErr := store.ReadLocalCredential(time.Now())
	if readErr != nil || !ok || stored.AccessToken != "fresh" || stored.RefreshToken != "rotated" {
		t.Fatalf("durable credential = %+v ok=%v err=%v", stored, ok, readErr)
	}
}

func TestRefreshReportsCommittedCredentialWhenDirectorySyncFails(t *testing.T) {
	store := writeCredentialFile(t, credentialFileJSON("stale", "old-refresh", time.Now().Add(-time.Hour)))
	syncFailure := errors.New("injected directory sync failure")
	originalSync := syncCredentialDirectory
	syncCredentialDirectory = func(path string) error {
		if want := filepath.Dir(store.credentialPath()); path != want {
			t.Fatalf("synced directory = %q, want %q", path, want)
		}
		return syncFailure
	}
	t.Cleanup(func() { syncCredentialDirectory = originalSync })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fresh", "refresh_token": "rotated", "expires_in": 3600,
		})
	}))
	defer server.Close()
	stubDiscovery(t, server.URL, server.URL)

	refreshed, didRefresh, err := store.RefreshAccountIfNeeded(
		t.Context(), server.Client(), account.Account{ID: accountID, Provider: account.ProviderGrok, Token: "stale"},
	)
	if !errors.Is(err, syncFailure) {
		t.Fatalf("refresh error = %v, want directory sync failure", err)
	}
	if !didRefresh || refreshed.Token != "fresh" {
		t.Fatalf("refresh result = account:%+v didRefresh:%v, want committed fresh token", refreshed, didRefresh)
	}
	// Rename precedes directory fsync. Surface the durability failure without
	// pretending the old pathname contents remain live in this process.
	credential, ok, readErr := store.ReadLocalCredential(time.Now())
	if readErr != nil || !ok || credential.AccessToken != "fresh" || credential.RefreshToken != "rotated" {
		t.Fatalf("renamed credential = %+v ok=%v err=%v", credential, ok, readErr)
	}
}

// A rejected refresh token comes back as a 401/403, which the proxy classifies
// as terminal for the account.
func TestRefreshAccountSurfacesARejectedRefreshToken(t *testing.T) {
	store := writeCredentialFile(t, credentialFileJSON("stale", "dead", time.Now().Add(-time.Hour)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer server.Close()
	stubDiscovery(t, server.URL, server.URL)

	_, err := store.RefreshAccount(context.Background(), server.Client(), account.Account{ID: accountID})
	if err == nil {
		t.Fatal("a 401 on refresh must be an error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error %q must carry the status so the proxy marks the account for re-auth", err)
	}
}

func TestValidateOAuthEndpoint(t *testing.T) {
	for _, good := range []string{"https://auth.x.ai/oauth/token", "https://x.ai/device"} {
		if _, err := validateOAuthEndpoint(good, "token_endpoint"); err != nil {
			t.Fatalf("%s must validate: %v", good, err)
		}
	}
	for _, bad := range []string{
		"http://auth.x.ai/oauth/token",     // no plaintext token posts
		"https://auth.x.ai.evil.com/token", // suffix lookalike
		"https://evil.com/token",
		"",
	} {
		if _, err := validateOAuthEndpoint(bad, "token_endpoint"); err == nil {
			t.Fatalf("%q must not validate", bad)
		}
	}
}

// A poisoned discovery document must not redirect refresh tokens off x.ai.
func TestDiscoverRejectsEndpointsOffXAI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"device_authorization_endpoint": "https://evil.example.com/device",
			"token_endpoint":                "https://auth.x.ai/oauth/token",
		})
	}))
	defer server.Close()
	stubDiscoveryURL(t, server.URL)

	if _, err := discover(context.Background(), server.Client()); err == nil {
		t.Fatal("an endpoint off x.ai must be rejected")
	}
}

func TestDiscoverResolvesTheDocumentedEndpoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"device_authorization_endpoint": "https://auth.x.ai/device",
			"token_endpoint":                "https://auth.x.ai/oauth/token",
		})
	}))
	defer server.Close()
	stubDiscoveryURL(t, server.URL)

	found, err := discover(context.Background(), server.Client())
	if err != nil {
		t.Fatalf("discover failed: %v", err)
	}
	if found.TokenURL != "https://auth.x.ai/oauth/token" || found.DeviceAuthURL != "https://auth.x.ai/device" {
		t.Fatalf("endpoints = %+v, want the documented pair", found)
	}
}

func stubDiscoveryURL(t *testing.T, url string) {
	t.Helper()
	restoreURL := discoveryURL
	discoveryURL = url
	discoveryMu.Lock()
	restoreCache := discoveryCache
	discoveryCache = nil
	discoveryMu.Unlock()
	t.Cleanup(func() {
		discoveryURL = restoreURL
		discoveryMu.Lock()
		discoveryCache = restoreCache
		discoveryMu.Unlock()
	})
}

// fakeIDToken builds an unsigned JWT carrying the given email claim.
func fakeIDToken(email string) string {
	header := base64.URLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.URLEncoding.EncodeToString([]byte(`{"email":"` + email + `"}`))
	return header + "." + payload + "."
}

// The sign-in flow must end with a usable credential file: the code shown to
// the person, the poll, and the write all have to line up.
func TestSignInWritesTheCredentialFile(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "oauth.json")}
	var sawDeviceRequest, sawPoll bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error("sign-in request form could not be parsed")
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		switch r.Form.Get("grant_type") {
		case "": // device-code request carries no grant_type
			sawDeviceRequest = true
			if r.Form.Get("client_id") != oauthClientID {
				t.Error("device request used an unexpected client ID")
				http.Error(w, "invalid client", http.StatusBadRequest)
				return
			}
			if r.Form.Get("scope") != oauthScope {
				t.Error("device request used an unexpected scope")
				http.Error(w, "invalid scope", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":      "dc",
				"user_code":        "ABCD-EFGH",
				"verification_uri": "https://auth.x.ai/device",
				"interval":         1,
				"expires_in":       600,
			})
		case "urn:ietf:params:oauth:grant-type:device_code":
			sawPoll = true
			if r.Form.Get("device_code") != "dc" {
				t.Error("device poll used an unexpected device code")
				http.Error(w, "invalid device code", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "signed-in-access",
				"refresh_token": "signed-in-refresh",
				"id_token":      fakeIDToken("dev@example.com"),
				"token_type":    "Bearer",
				"expires_in":    3600,
			})
		default:
			t.Error("sign-in request used an unexpected grant type")
			http.Error(w, "invalid grant", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	stubDiscovery(t, server.URL, server.URL)

	var out strings.Builder
	credential, err := store.SignIn(context.Background(), server.Client(), &out)
	if err != nil {
		t.Fatalf("sign-in failed: %v", err)
	}
	if !sawDeviceRequest || !sawPoll {
		t.Fatalf("flow incomplete: device request=%v poll=%v", sawDeviceRequest, sawPoll)
	}
	if !strings.Contains(out.String(), "ABCD-EFGH") || !strings.Contains(out.String(), "https://auth.x.ai/device") {
		t.Fatalf("output %q must show the code and verification URL", out.String())
	}
	if credential.Email != "dev@example.com" {
		t.Fatalf("Email = %q, want the identity from the ID token", credential.Email)
	}

	stored, ok, err := store.ReadLocalCredential(time.Now())
	if err != nil || !ok {
		t.Fatalf("re-read failed: ok=%v err=%v", ok, err)
	}
	if stored.AccessToken != "signed-in-access" || stored.RefreshToken != "signed-in-refresh" {
		t.Fatalf("stored credential = %+v, want the sign-in tokens", stored)
	}
	info, err := os.Stat(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("credential file mode = %o, want 0600", perm)
	}
}

func TestAccountRefreshStatePreflightsCredentialWithoutMutation(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		expiresAt time.Time
		want      bool
	}{
		{name: "fresh", expiresAt: time.Now().Add(time.Hour), want: false},
		{name: "expiring", expiresAt: time.Now().Add(time.Minute), want: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := Store{Path: filepath.Join(t.TempDir(), "oauth.json")}
			account, err := store.SaveCredential(CredentialInfo{
				AccessToken: "access", RefreshToken: "refresh", ExpiresAt: testCase.expiresAt,
			})
			if err != nil {
				t.Fatal(err)
			}
			current, got, err := store.AccountRefreshState(account, time.Now())
			if err != nil || got != testCase.want {
				t.Fatalf("AccountRefreshState() = %v, %v, want %v, nil", got, err, testCase.want)
			}
			if current.Token != "access" {
				t.Fatalf("current token = %q", current.Token)
			}
		})
	}
}
