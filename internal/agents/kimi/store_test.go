package kimi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
	"github.com/manaflow-ai/subrouter/internal/oauthdevice"
)

var reference = time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

func TestParseCredentialReadsTheCLIFileShape(t *testing.T) {
	credential, err := ParseCredential([]byte(
		`{"access_token":"at","refresh_token":"rt","expires_at":1787144400,"scope":"kimi-code","token_type":"Bearer","expires_in":900}`),
		"test", reference)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if credential.AccessToken != "at" || credential.RefreshToken != "rt" {
		t.Fatal("credential tokens did not round-trip")
	}
	if want := time.Unix(1787144400, 0).UTC(); !credential.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %s, want %s", credential.ExpiresAt, want)
	}
	if credential.Scope != "kimi-code" || credential.TokenType != "Bearer" {
		t.Fatalf("scope/token_type = %q/%q", credential.Scope, credential.TokenType)
	}
}

// expires_in is relative, so it only means anything against a clock.
func TestParseCredentialFallsBackToRelativeExpiry(t *testing.T) {
	credential, err := ParseCredential([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":900}`), "test", reference)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if want := reference.Add(900 * time.Second); !credential.ExpiresAt.Equal(want) {
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

func TestUnreadableExpiryDoesNotLeakItsValue(t *testing.T) {
	secret := "token-like-secret-value"
	_, err := ParseCredential([]byte(`{"access_token":"at","expires_at":"`+secret+`"}`), "test", reference)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("invalid expiry error must be redacted")
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
	_, err := ParseCredential(body, "kimi-code.json", reference)
	if err == nil {
		t.Fatal("trailing bytes must not decode")
	}
	message := err.Error()
	for _, want := range []string{unreadableCredentialPhrase, "from kimi-code.json", "trailing_kind=binary-plist"} {
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
	// The access token lives 900 seconds, so "inside the lead" is a third of
	// its life.
	soon := CredentialInfo{AccessToken: "at", ExpiresAt: reference.Add(4 * time.Minute)}
	if !soon.NeedsRefresh(reference) {
		t.Fatal("a token inside the five-minute lead must be refreshed before use")
	}
	unknown := CredentialInfo{AccessToken: "at"}
	if !unknown.NeedsRefresh(reference) {
		t.Fatal("a credential with no stated expiry must be refreshed rather than trusted")
	}
}

// writeCredentialFile seeds a CLI credential file for a test store.
func writeCredentialFile(t *testing.T, payload string) Store {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, "credentials", "kimi-code.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "device_id"), []byte("official-cli-device-id\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Store{Path: path}
}

func TestListAccountsReportsSignedOutWhenTheFileIsAbsent(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "kimi-code.json")}
	accounts, err := store.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("got %d accounts, want none", len(accounts))
	}
}

func TestListAccountsSurfacesTheCLICredential(t *testing.T) {
	store := writeCredentialFile(t, credentialFileJSON("at", "rt", time.Now().Add(time.Hour)))
	accounts, err := store.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("got %d accounts, want 1", len(accounts))
	}
	acct := accounts[0]
	if acct.Provider != account.ProviderKimi || acct.AuthMode != account.AuthModeOAuth {
		t.Fatalf("account identity = %s/%s, want kimi/oauth", acct.Provider, acct.AuthMode)
	}
	if acct.Token != "at" {
		t.Fatal("account did not carry the expected access token")
	}
}

func TestManagedAccountsAreIsolatedFromTheKimiCLICredential(t *testing.T) {
	root := t.TempDir()
	cliPath := filepath.Join(root, "cli", "kimi-code.json")
	if err := os.MkdirAll(filepath.Dir(cliPath), 0o700); err != nil {
		t.Fatal(err)
	}
	cliBody := credentialFileJSON("cli-access", "cli-refresh", time.Now().Add(time.Hour))
	if err := os.WriteFile(cliPath, []byte(cliBody), 0o600); err != nil {
		t.Fatal(err)
	}
	store := Store{Path: cliPath, ManagedDir: filepath.Join(root, "managed")}
	for _, label := range []string{"work", "personal"} {
		if _, err := store.SaveManagedCredential(label, CredentialInfo{
			AccessToken: label + "-access", RefreshToken: label + "-refresh",
			ExpiresAt: time.Now().Add(time.Hour), TokenType: "Bearer",
		}); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := store.ListAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 3 {
		t.Fatalf("listed %d Kimi accounts, want CLI plus two managed profiles", len(listed))
	}
	want := map[string]string{
		"kimi-code": "cli-access", "kimi-subscription:work": "work-access", "kimi-subscription:personal": "personal-access",
	}
	for _, acct := range listed {
		if want[acct.ID] != acct.Token {
			t.Fatalf("account %q carried the wrong isolated credential", acct.ID)
		}
	}
	removed, ok, err := store.RemoveManagedAccount("work")
	if err != nil || !ok || removed.ID != "kimi-subscription:work" {
		t.Fatal("managed account removal did not return the requested account")
	}
	cliAfter, err := os.ReadFile(cliPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(cliAfter) != cliBody {
		t.Fatal("managed account operations rewrote the Kimi CLI credential")
	}
}

func TestListAccountsKeepsHealthyManagedProfilesWhenOneIsUnreadable(t *testing.T) {
	managedDir := t.TempDir()
	store := Store{Path: filepath.Join(t.TempDir(), "unused-cli.json"), ManagedDir: managedDir}
	if _, err := store.SaveManagedCredential("healthy", CredentialInfo{
		AccessToken: "healthy-access", RefreshToken: "healthy-refresh", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	filename, err := managedFilename("kimi-subscription:broken")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedDir, filename), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}

	listed, err := store.ListAccounts(context.Background())
	if err == nil || !strings.Contains(err.Error(), "kimi-subscription:broken") {
		t.Fatalf("partial list error = %v, want broken profile identified", err)
	}
	if len(listed) != 1 || listed[0].ID != "kimi-subscription:healthy" || listed[0].Token != "healthy-access" {
		t.Fatalf("healthy profile count/identity was not preserved (count=%d)", len(listed))
	}
}

func TestListAccountsKeepsManagedProfilesWhenCLICredentialIsUnreadable(t *testing.T) {
	root := t.TempDir()
	cliPath := filepath.Join(root, "cli", "kimi-code.json")
	if err := os.MkdirAll(filepath.Dir(cliPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cliPath, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := Store{Path: cliPath, ManagedDir: filepath.Join(root, "managed")}
	if _, err := store.SaveManagedCredential("healthy", CredentialInfo{
		AccessToken: "healthy-access", RefreshToken: "healthy-refresh", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	listed, err := store.ListAccounts(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Kimi CLI credential") {
		t.Fatalf("partial CLI credential error = %v", err)
	}
	if len(listed) != 1 || listed[0].ID != "kimi-subscription:healthy" || listed[0].Token != "healthy-access" {
		t.Fatalf("managed profile was not preserved (count=%d)", len(listed))
	}
}

func TestManagedAccountLabelRejectsControlCharacters(t *testing.T) {
	for _, label := range []string{
		"unsafe\x1b[31m",
		"spoof\u202ereversed",
		"zero\u200bwidth",
		"line\u2028separator",
		"paragraph\u2029separator",
	} {
		if _, err := ManagedAccountID(label); err == nil {
			t.Fatalf("terminal-control label %q must be rejected", label)
		}
	}
}

func TestManagedAccountLabelRejectsReservedPrefixAndOversizedValues(t *testing.T) {
	for _, label := range []string{"kimi-subscription:work", "KIMI-SUBSCRIPTION:work", strings.Repeat("x", maxManagedLabelBytes+1)} {
		if _, err := ManagedAccountID(label); err == nil {
			t.Fatalf("managed label %q must be rejected", label)
		}
	}
}

func TestManagedAccountRemovalSeparatesLabelsFromCanonicalIDs(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "unused-cli.json"), ManagedDir: t.TempDir()}
	if _, err := store.SaveManagedCredential("work", CredentialInfo{
		AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RemoveManagedAccount("kimi-subscription:work"); err == nil {
		t.Fatal("label-based removal accepted a canonical account ID")
	}
	if credential, ok, err := store.ReadManagedCredential("work", time.Now()); err != nil || !ok || credential.AccessToken != "access" {
		t.Fatal("rejected label-based removal deleted the managed profile")
	}
	removed, ok, err := store.RemoveManagedAccountID("kimi-subscription:work")
	if err != nil || !ok || removed.ID != "kimi-subscription:work" {
		t.Fatal("canonical ID removal did not remove the requested managed profile")
	}
}

func TestManagedAccountRemovalDeletesAnUnreadableCredential(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "unused-cli.json"), ManagedDir: t.TempDir()}
	filename, err := managedFilename("kimi-subscription:broken")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.ManagedDir, filename)
	if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	removed, ok, err := store.RemoveManagedAccount("broken")
	if err != nil || !ok || removed.ID != "kimi-subscription:broken" {
		t.Fatalf("remove unreadable credential: removed=%+v ok=%v err=%v", removed, ok, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unreadable credential remains after removal: %v", err)
	}
}

func TestListAccountsRoutesOnlyCanonicalManagedFilename(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "unused-cli.json"), ManagedDir: t.TempDir()}
	if _, err := store.SaveManagedCredential("work", CredentialInfo{
		AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	canonicalName, err := managedFilename("kimi-subscription:work")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(store.ManagedDir, canonicalName))
	if err != nil {
		t.Fatal(err)
	}
	aliasName := base64.RawURLEncoding.EncodeToString([]byte("WORK")) + ".json"
	aliasPath := filepath.Join(store.ManagedDir, aliasName)
	if err := os.WriteFile(aliasPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	listed, err := store.ListAccounts(t.Context())
	if err == nil || len(listed) != 1 || listed[0].ID != "kimi-subscription:work" {
		t.Fatalf("routed accounts = %+v, err = %v", listed, err)
	}
	ids, err := store.AccountInventoryIDs(t.Context())
	if err != nil || len(ids) != 2 || ids[0] != "kimi-subscription:work" || ids[1] != "kimi-subscription:work" {
		t.Fatalf("durable inventory = %v, err = %v", ids, err)
	}
	if _, err := os.Stat(aliasPath); err != nil {
		t.Fatalf("noncanonical alias was not preserved: %v", err)
	}
}

func TestValidateManagedAuthorizationRejectsUnusableTokens(t *testing.T) {
	valid := CredentialInfo{
		AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour), OAuthDeviceID: "device-id",
	}
	tests := []struct {
		name   string
		mutate func(*CredentialInfo)
	}{
		{name: "missing access", mutate: func(c *CredentialInfo) { c.AccessToken = "" }},
		{name: "missing refresh", mutate: func(c *CredentialInfo) { c.RefreshToken = "" }},
		{name: "missing expiry", mutate: func(c *CredentialInfo) { c.ExpiresAt = time.Time{} }},
		{name: "expired", mutate: func(c *CredentialInfo) { c.ExpiresAt = time.Now().Add(-time.Minute) }},
		{name: "missing device", mutate: func(c *CredentialInfo) { c.OAuthDeviceID = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			credential := valid
			test.mutate(&credential)
			if err := validateManagedAuthorization(credential, time.Now()); err == nil {
				t.Fatal("unusable managed authorization was accepted")
			}
		})
	}
	if err := validateManagedAuthorization(valid, time.Now()); err != nil {
		t.Fatalf("valid managed authorization rejected: %v", err)
	}
}

func TestManagedAccountIDCanonicalizesCaseAndPreservesDisplayLabel(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "unused-cli.json"), ManagedDir: t.TempDir()}
	first, err := store.SaveManagedCredential("Work", CredentialInfo{
		AccessToken: "first", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.SaveManagedCredential("work", CredentialInfo{
		AccessToken: "second", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "kimi-subscription:work" || second.ID != first.ID {
		t.Fatalf("canonical IDs = %q and %q", first.ID, second.ID)
	}
	listed, err := store.ListAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != "kimi-subscription:work" || listed[0].Label != "work" || listed[0].Token != "second" {
		t.Fatal("case-variant login created an ambiguous managed profile")
	}
}

func TestSignInManagedUsesDeviceFlowAndLeavesCLIFileUntouched(t *testing.T) {
	root := t.TempDir()
	cliPath := filepath.Join(root, "kimi-code.json")
	cliBody := credentialFileJSON("cli-access", "cli-refresh", time.Now().Add(time.Hour))
	if err := os.WriteFile(cliPath, []byte(cliBody), 0o600); err != nil {
		t.Fatal(err)
	}
	var tokenCalls int
	var deviceID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Msh-Platform") != "kimi_cli" || r.Header.Get("X-Msh-Version") != "subrouter" {
			http.Error(w, "missing Kimi public-client identity", http.StatusUnauthorized)
			return
		}
		if got := r.Header.Get("X-Msh-Device-Id"); got == "" {
			http.Error(w, "missing Kimi device identity", http.StatusUnauthorized)
			return
		} else if deviceID == "" {
			deviceID = got
		} else if got != deviceID {
			http.Error(w, "device identity changed during OAuth", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/device":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code": "device", "user_code": "ABCD-1234",
				"verification_uri": "https://example.test/activate", "interval": 1, "expires_in": 60,
			})
		case "/token":
			tokenCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "managed-access", "refresh_token": "managed-refresh",
				"token_type": "Bearer", "expires_in": 900,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	restore := oauthConfig
	oauthConfig = oauthdevice.Config{ClientID: "test-client", DeviceAuthURL: server.URL + "/device", TokenURL: server.URL + "/token"}
	t.Cleanup(func() { oauthConfig = restore })

	store := Store{Path: cliPath, ManagedDir: filepath.Join(root, "managed")}
	var out strings.Builder
	acct, err := store.SignInManaged(context.Background(), server.Client(), "second", &out)
	if err != nil {
		t.Fatal(err)
	}
	if acct.ID != "kimi-subscription:second" || acct.Token != "managed-access" || tokenCalls != 1 {
		t.Fatal("managed sign-in did not return the isolated account")
	}
	credential, ok, err := store.ReadManagedCredential("second", time.Now())
	if err != nil || !ok {
		t.Fatalf("read managed credential: ok=%v err=%v", ok, err)
	}
	if credential.OAuthDeviceID == "" || credential.OAuthDeviceID != deviceID {
		t.Fatal("managed sign-in did not persist its OAuth device identity")
	}
	if !strings.Contains(out.String(), "ABCD-1234") {
		t.Fatal("sign-in instructions omitted the user code")
	}
	cliAfter, err := os.ReadFile(cliPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(cliAfter) != cliBody {
		t.Fatal("managed sign-in rewrote the Kimi CLI credential")
	}
}

func TestSignInManagedRejectsOversizedLabelBeforeDeviceAuthorization(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer server.Close()
	restore := oauthConfig
	oauthConfig = oauthdevice.Config{ClientID: "test-client", DeviceAuthURL: server.URL, TokenURL: server.URL}
	t.Cleanup(func() { oauthConfig = restore })

	store := Store{Path: filepath.Join(t.TempDir(), "unused-cli.json"), ManagedDir: t.TempDir()}
	if _, err := store.SignInManaged(context.Background(), server.Client(), strings.Repeat("x", maxManagedLabelBytes+1), io.Discard); err == nil {
		t.Fatal("oversized label must fail before interactive authorization")
	}
	if requests != 0 {
		t.Fatalf("device authorization received %d requests for an invalid label", requests)
	}
}

func TestReadLocalCredentialAnchorsRelativeExpiryToFileTime(t *testing.T) {
	store := writeCredentialFile(t, `{"access_token":"at","refresh_token":"rt","expires_in":900}`)
	issuedAt := reference.Add(-time.Hour)
	if err := os.Chtimes(store.Path, issuedAt, issuedAt); err != nil {
		t.Fatal(err)
	}
	credential, ok, err := store.ReadLocalCredential(reference)
	if err != nil || !ok {
		t.Fatalf("read failed: ok=%v err=%v", ok, err)
	}
	if !credential.ExpiresAt.Equal(issuedAt.Add(15*time.Minute)) || !credential.NeedsRefresh(reference) {
		t.Fatalf("relative expiry was not anchored to the stable file time")
	}
}

// credentialFileJSON renders a CLI credential file expiring at the given time.
func credentialFileJSON(accessToken, refreshToken string, expiresAt time.Time) string {
	return `{"access_token":"` + accessToken + `","refresh_token":"` + refreshToken +
		`","expires_at":` + strconv.FormatInt(expiresAt.Unix(), 10) + `,"token_type":"Bearer"}`
}

func stubOAuthConfig(t *testing.T, tokenURL string) {
	t.Helper()
	restore := oauthConfig
	oauthConfig = oauthdevice.Config{ClientID: "test-client", TokenURL: tokenURL}
	t.Cleanup(func() { oauthConfig = restore })
}

func TestRefreshAccountKeepsAFreshCredential(t *testing.T) {
	store := writeCredentialFile(t, credentialFileJSON("at", "rt", time.Now().Add(time.Hour)))
	if err := os.Remove(filepath.Join(filepath.Dir(filepath.Dir(store.Path)), "device_id")); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("a credential with life left must not be refreshed")
	}))
	defer server.Close()
	stubOAuthConfig(t, server.URL)

	refreshed, err := store.RefreshAccount(context.Background(), server.Client(), account.Account{ID: accountID, Provider: account.ProviderKimi})
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if refreshed.Token != "at" {
		t.Fatal("refresh changed the still-valid access token")
	}
}

// The refresh must write the rotated tokens back over the CLI's file, or the
// CLI keeps a dead refresh token and gets logged out.
func TestRefreshAccountRefreshesAndWritesBack(t *testing.T) {
	store := writeCredentialFile(t,
		strings.TrimSuffix(credentialFileJSON("stale", "rt", time.Now().Add(-time.Hour)), "}")+`,"scope":"kimi-code"}`)
	var gotForm string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Msh-Device-Id"); got != "official-cli-device-id" {
			http.Error(w, "local CLI refresh did not reuse its device identity", http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("X-Msh-Platform"); got != "kimi_code_cli" {
			http.Error(w, "local CLI refresh used the wrong platform", http.StatusBadRequest)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		gotForm = r.Form.Encode()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-access",
			"refresh_token": "rotated-refresh",
			"expires_in":    900,
			"token_type":    "Bearer",
		})
	}))
	defer server.Close()
	stubOAuthConfig(t, server.URL)

	expired := account.Account{ID: accountID, Provider: account.ProviderKimi, Token: "stale"}
	refreshed, err := store.RefreshAccount(context.Background(), server.Client(), expired)
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if refreshed.Token != "fresh-access" {
		t.Fatal("refresh did not return the new access token")
	}
	for _, want := range []string{"grant_type=refresh_token", "refresh_token=rt", "client_id=test-client"} {
		if !strings.Contains(gotForm, want) {
			t.Fatalf("request form %q is missing %q", gotForm, want)
		}
	}

	// The CLI's file must now hold the rotated pair.
	credential, ok, err := store.ReadLocalCredential(time.Now())
	if err != nil || !ok {
		t.Fatalf("re-read failed: ok=%v err=%v", ok, err)
	}
	if credential.AccessToken != "fresh-access" || credential.RefreshToken != "rotated-refresh" {
		t.Fatal("written credential did not contain the rotated pair")
	}
	if credential.ExpiresAt.IsZero() {
		t.Fatal("written credential lost its expiry")
	}
	written, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(written), "oauth_device_id") {
		t.Fatal("local CLI refresh copied the external device identity into its credential JSON")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(store.Path)), "device-id")); !os.IsNotExist(err) {
		t.Fatalf("local CLI refresh created a managed device identity: %v", err)
	}
}

func TestServingStoreNeverRefreshesInteractiveCredentialButPersistsManagedRefresh(t *testing.T) {
	store := writeCredentialFile(t, credentialFileJSON("cli-stale", "cli-refresh", time.Now().Add(-time.Hour)))
	store.ManagedDir = t.TempDir()
	serving := store.ForServing()
	managed, err := serving.SaveManagedCredential("work", CredentialInfo{
		AccessToken: "managed-stale", RefreshToken: "managed-refresh", ExpiresAt: time.Now().Add(-time.Hour),
		OAuthDeviceID: "managed-device",
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := serving.ListAccounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != managed.ID {
		t.Fatalf("serving accounts = %+v, want only managed profile %q", listed, managed.ID)
	}

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("X-Msh-Device-Id") != "managed-device" {
			http.Error(w, "wrong device", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "managed-fresh", "refresh_token": "managed-rotated", "expires_in": 900,
		})
	}))
	defer server.Close()
	stubOAuthConfig(t, server.URL)

	_, err = serving.RefreshAccount(t.Context(), server.Client(), account.Account{
		ID: accountID, Provider: account.ProviderKimi, AuthMode: account.AuthModeOAuth,
	})
	if err == nil || !strings.Contains(err.Error(), "not routable") {
		t.Fatalf("interactive serving refresh error = %v", err)
	}
	if requests != 0 {
		t.Fatalf("interactive serving refresh made %d HTTP request(s)", requests)
	}
	after, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("serving refresh changed the interactive Kimi CLI credential bytes")
	}

	if _, err := serving.RefreshAccount(t.Context(), server.Client(), managed); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("managed refresh requests = %d, want 1", requests)
	}
	stored, ok, err := serving.ReadManagedCredential("work", time.Now())
	if err != nil || !ok {
		t.Fatalf("read managed credential: ok=%v err=%v", ok, err)
	}
	if stored.AccessToken != "managed-fresh" || stored.RefreshToken != "managed-rotated" {
		t.Fatalf("managed credential did not persist rotation: %+v", stored)
	}
}

func TestLocalCLIRefreshFailsClosedWithoutItsDeviceIdentity(t *testing.T) {
	store := writeCredentialFile(t, credentialFileJSON("stale", "rt", time.Now().Add(-time.Hour)))
	devicePath := filepath.Join(filepath.Dir(filepath.Dir(store.Path)), "device_id")
	if err := os.Remove(devicePath); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	stubOAuthConfig(t, server.URL)

	_, err := store.RefreshAccount(context.Background(), server.Client(), account.Account{ID: accountID, Provider: account.ProviderKimi})
	if err == nil || !strings.Contains(err.Error(), "run kimi login again") {
		t.Fatalf("missing device identity error = %v", err)
	}
	if requests != 0 {
		t.Fatalf("refresh made %d requests without the bound device identity", requests)
	}
}

func TestLocalCLIRefreshRejectsInvalidDeviceIdentityBeforeNetwork(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "empty", value: "  \n"},
		{name: "oversized", value: strings.Repeat("x", 129)},
		{name: "non-ascii", value: "device-☃"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := writeCredentialFile(t, credentialFileJSON("stale", "rt", time.Now().Add(-time.Hour)))
			devicePath := filepath.Join(filepath.Dir(filepath.Dir(store.Path)), "device_id")
			if err := os.WriteFile(devicePath, []byte(test.value), 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(store.Path)
			if err != nil {
				t.Fatal(err)
			}
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
			defer server.Close()
			stubOAuthConfig(t, server.URL)

			_, err = store.RefreshAccount(context.Background(), server.Client(), account.Account{ID: accountID, Provider: account.ProviderKimi})
			if err == nil || !strings.Contains(err.Error(), "run kimi login again") {
				t.Fatalf("invalid device identity error = %v", err)
			}
			if strings.Contains(err.Error(), test.value) {
				t.Fatal("device identity error leaked the invalid value")
			}
			if requests != 0 {
				t.Fatalf("refresh made %d requests with an invalid device identity", requests)
			}
			after, readErr := os.ReadFile(store.Path)
			if readErr != nil || string(after) != string(before) {
				t.Fatal("failed refresh changed the credential file")
			}
		})
	}
}

func TestLocalCLIRefreshRequiresHomeForArbitraryCredentialPath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "custom-token.json")
	if err := os.WriteFile(path, []byte(credentialFileJSON("stale", "rt", time.Now().Add(-time.Hour))), 0o600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("X-Msh-Device-Id"); got != "explicit-device" {
			http.Error(w, "wrong device identity", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "refresh_token": "rotated", "expires_in": 900})
	}))
	defer server.Close()
	stubOAuthConfig(t, server.URL)

	store := Store{Path: path}
	if _, err := store.RefreshAccount(context.Background(), server.Client(), account.Account{ID: accountID, Provider: account.ProviderKimi}); err == nil || !strings.Contains(err.Error(), "set KimiHome") {
		t.Fatalf("arbitrary path without home error = %v", err)
	}
	if requests != 0 {
		t.Fatal("arbitrary path guessed a device location")
	}
	if err := os.WriteFile(filepath.Join(root, "device_id"), []byte("explicit-device"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.KimiHome = root
	if _, err := store.RefreshAccount(context.Background(), server.Client(), account.Account{ID: accountID, Provider: account.ProviderKimi}); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("explicit home refresh requests = %d, want 1", requests)
	}
}

func TestLocalCLIRefreshRereadsAfterTheOfficialCrossProcessLock(t *testing.T) {
	store := writeCredentialFile(t, credentialFileJSON("stale", "rt", time.Now().Add(-time.Hour)))
	home := filepath.Dir(filepath.Dir(store.Path))
	oauthDir := filepath.Join(home, "oauth")
	if err := os.MkdirAll(filepath.Join(oauthDir, "kimi-code.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	stubOAuthConfig(t, server.URL)

	result := make(chan struct {
		account account.Account
		err     error
	}, 1)
	go func() {
		refreshed, err := store.RefreshAccount(context.Background(), server.Client(), account.Account{ID: accountID, Provider: account.ProviderKimi})
		result <- struct {
			account account.Account
			err     error
		}{refreshed, err}
	}()
	select {
	case got := <-result:
		t.Fatalf("refresh bypassed the official lock: account=%+v err=%v", got.account, got.err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := writeCredential(store.Path, CredentialInfo{
		AccessToken: "peer-access", RefreshToken: "peer-refresh", TokenType: "Bearer", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(oauthDir, "kimi-code.lock")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil || got.account.Token != "peer-access" {
			t.Fatalf("post-lock refresh = %+v err=%v", got.account, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not resume after the peer released the lock")
	}
	if requests != 0 {
		t.Fatalf("post-lock re-read still made %d refresh requests", requests)
	}
}

func TestLocalCLIRefreshRefusesRacyStaleLockTakeover(t *testing.T) {
	store := writeCredentialFile(t, credentialFileJSON("stale", "rt", time.Now().Add(-time.Hour)))
	home := filepath.Dir(filepath.Dir(store.Path))
	lockDir := filepath.Join(home, "oauth", "kimi-code.lock")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-2 * cliRefreshLockStale)
	if err := os.Chtimes(lockDir, stale, stale); err != nil {
		t.Fatal(err)
	}
	_, err := store.RefreshAccount(context.Background(), http.DefaultClient, account.Account{ID: accountID, Provider: account.ProviderKimi})
	if err == nil || !strings.Contains(err.Error(), "appears stale") || !strings.Contains(err.Error(), lockDir) {
		t.Fatalf("stale-lock refresh error = %v, want fail-closed recovery guidance", err)
	}
	if info, err := os.Stat(lockDir); err != nil || !info.IsDir() {
		t.Fatalf("stale lock was removed automatically: %v", err)
	}
}

// If another process replaces the proper-lockfile directory while a refresh is
// in flight, the response was produced without continuous ownership. Do not
// commit its rotated token, and never remove the replacement owner's lock.
func TestLocalCLIRefreshFailsClosedWhenLockOwnershipChanges(t *testing.T) {
	store := writeCredentialFile(t, credentialFileJSON("stale", "rt", time.Now().Add(-time.Hour)))
	before, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Dir(filepath.Dir(store.Path))
	lockDir := filepath.Join(home, "oauth", "kimi-code.lock")
	displacedLock := lockDir + ".displaced"
	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-releaseResponse
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "must-not-be-saved", "refresh_token": "must-not-be-saved", "expires_in": 900,
		})
	}))
	defer server.Close()
	stubOAuthConfig(t, server.URL)

	result := make(chan error, 1)
	go func() {
		_, refreshErr := store.RefreshAccount(context.Background(), server.Client(), account.Account{ID: accountID, Provider: account.ProviderKimi})
		result <- refreshErr
	}()
	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh request did not start")
	}
	if err := os.Rename(lockDir, displacedLock); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	close(releaseResponse)

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "lock ownership changed") {
			t.Fatalf("refresh error = %v, want lost lock ownership", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not stop after lock ownership changed")
	}
	after, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("refresh committed credentials after losing lock ownership")
	}
	if info, err := os.Stat(lockDir); err != nil || !info.IsDir() {
		t.Fatalf("replacement owner's lock was removed: %v", err)
	}
}

// Releasing the interoperability lock happens after the atomic credential
// write. A release failure is important, but it must not make callers believe
// the old token is still durable or that no rotation was committed.
func TestLocalCLIRefreshReportsCommittedRotationWhenLockReleaseFails(t *testing.T) {
	store := writeCredentialFile(t, credentialFileJSON("stale", "old-refresh", time.Now().Add(-time.Hour)))
	releaseFailure := errors.New("injected post-write lock release failure")
	store.lockLocalCLIRefreshForTest = func(ctx context.Context) (*cliRefreshLock, error) {
		lockCtx, cancel := context.WithCancel(ctx)
		return &cliRefreshLock{ctx: lockCtx, cancel: cancel, releaseErr: releaseFailure}, nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fresh", "refresh_token": "rotated", "expires_in": 900,
		})
	}))
	defer server.Close()
	stubOAuthConfig(t, server.URL)

	refreshed, didRefresh, err := store.RefreshAccountIfNeeded(
		context.Background(), server.Client(), account.Account{ID: accountID, Provider: account.ProviderKimi, Token: "stale"},
	)
	if !errors.Is(err, releaseFailure) {
		t.Fatalf("refresh error = %v, want release failure", err)
	}
	if !didRefresh || refreshed.Token != "fresh" {
		t.Fatalf("refresh result = account:%+v didRefresh:%v, want committed fresh token", refreshed, didRefresh)
	}
	stored, ok, readErr := store.ReadLocalCredential(time.Now())
	if readErr != nil || !ok || stored.AccessToken != "fresh" || stored.RefreshToken != "rotated" {
		t.Fatalf("durable credential = %+v ok=%v err=%v", stored, ok, readErr)
	}
}

func TestManagedRefreshUsesProviderLifetimeWhenResponseOmitsExpiresIn(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "unused-cli.json"), ManagedDir: t.TempDir()}
	wantExpiry := time.Now().Add(2 * time.Minute).Truncate(time.Second)
	acct, err := store.SaveManagedCredential("work", CredentialInfo{
		AccessToken: "stale", RefreshToken: "refresh", ExpiresAt: wantExpiry,
		OAuthDeviceID: "authorized-device",
	})
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		// Deliberately omit expires_in: this response shape used to erase the
		// stored absolute expiry and leave no usable expiry metadata.
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh"})
	}))
	defer server.Close()
	stubOAuthConfig(t, server.URL)

	if _, err := store.RefreshAccount(t.Context(), server.Client(), acct); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RefreshAccount(t.Context(), server.Client(), acct); err != nil {
		t.Fatal(err)
	}
	credential, ok, err := store.ReadManagedCredential("work", time.Now())
	if err != nil || !ok {
		t.Fatalf("read refreshed credential: ok=%v err=%v", ok, err)
	}
	if !credential.ExpiresAt.After(time.Now().Add(refreshLead)) {
		t.Fatalf("ExpiresAt = %s, want fallback outside the refresh lead", credential.ExpiresAt)
	}
	if requests != 1 {
		t.Fatalf("refresh requests = %d, want one across two calls", requests)
	}
	if got := refreshedExpiry(reference.Add(time.Hour), time.Time{}, reference); !got.Equal(reference.Add(time.Hour)) {
		t.Fatalf("longer prior expiry = %s, want it preserved", got)
	}
}

func TestManagedRefreshReportsCommittedCredentialWhenDirectorySyncFails(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "unused-cli.json"), ManagedDir: t.TempDir()}
	acct, err := store.SaveManagedCredential("work", CredentialInfo{
		AccessToken: "stale", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Hour),
		OAuthDeviceID: "authorized-device",
	})
	if err != nil {
		t.Fatal(err)
	}
	path, _, _, err := store.accountCredentialPath(acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	syncFailure := errors.New("injected directory sync failure")
	originalSync := syncCredentialDirectory
	syncCredentialDirectory = func(got string) error {
		if want := filepath.Dir(path); got != want {
			t.Fatalf("synced directory = %q, want %q", got, want)
		}
		return syncFailure
	}
	t.Cleanup(func() { syncCredentialDirectory = originalSync })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fresh", "refresh_token": "rotated", "expires_in": 900,
		})
	}))
	defer server.Close()
	stubOAuthConfig(t, server.URL)

	refreshed, didRefresh, err := store.RefreshAccountIfNeeded(t.Context(), server.Client(), acct)
	if !errors.Is(err, syncFailure) {
		t.Fatalf("refresh error = %v, want directory sync failure", err)
	}
	if !didRefresh || refreshed.Token != "fresh" {
		t.Fatalf("refresh result = account:%+v didRefresh:%v, want committed fresh token", refreshed, didRefresh)
	}
	credential, ok, readErr := readCredential(path, time.Now())
	if readErr != nil || !ok || credential.AccessToken != "fresh" || credential.RefreshToken != "rotated" {
		t.Fatalf("renamed credential = %+v ok=%v err=%v", credential, ok, readErr)
	}
}

func TestDefaultStoreHonorsKimiCodeHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KIMI_CODE_HOME", home)
	path := filepath.Join(home, "credentials", "kimi-code.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(credentialFileJSON("at", "rt", time.Now().Add(time.Hour))), 0o600); err != nil {
		t.Fatal(err)
	}
	credential, ok, err := DefaultStore().ReadLocalCredential(time.Now())
	if err != nil || !ok || credential.AccessToken != "at" {
		t.Fatalf("KIMI_CODE_HOME credential: ok=%v credential=%+v err=%v", ok, credential, err)
	}
}

func TestManagedRefreshUsesTheAuthorizationDeviceIdentity(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "unused-cli.json"), ManagedDir: t.TempDir()}
	const authorizedDeviceID = "device-used-during-authorization"
	acct, err := store.SaveManagedCredential("work", CredentialInfo{
		AccessToken: "stale", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Hour),
		OAuthDeviceID: authorizedDeviceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.ManagedDir, "device-id"), []byte("different-server-device"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Msh-Device-Id"); got != authorizedDeviceID {
			http.Error(w, "wrong device identity", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fresh", "refresh_token": "rotated", "expires_in": 900,
		})
	}))
	defer server.Close()
	stubOAuthConfig(t, server.URL)

	if _, err := store.RefreshAccount(context.Background(), server.Client(), acct); err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.ReadManagedCredential("work", time.Now())
	if err != nil || !ok {
		t.Fatalf("read refreshed credential: ok=%v err=%v", ok, err)
	}
	if stored.OAuthDeviceID != authorizedDeviceID {
		t.Fatal("refresh did not retain the authorization device identity")
	}
}

func TestManagedRefreshNeverBorrowsTheLocalCLIDeviceIdentity(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "device_id"), []byte("host-cli-device"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := Store{Path: filepath.Join(home, "credentials", "kimi-code.json"), ManagedDir: t.TempDir()}
	acct, err := store.SaveManagedCredential("work", CredentialInfo{
		AccessToken: "stale", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	stubOAuthConfig(t, server.URL)

	_, err = store.RefreshAccount(context.Background(), server.Client(), acct)
	if err == nil || !strings.Contains(err.Error(), "sign in again") {
		t.Fatalf("managed credential without bound device error = %v", err)
	}
	if requests != 0 {
		t.Fatal("managed refresh borrowed the host CLI device identity")
	}
}

func TestManagedRefreshTransactionPreventsRemovalFromBeingResurrected(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "unused-cli.json"), ManagedDir: t.TempDir()}
	acct, err := store.SaveManagedCredential("work", CredentialInfo{
		AccessToken: "stale", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Hour),
		OAuthDeviceID: "device-used-during-authorization",
	})
	if err != nil {
		t.Fatal(err)
	}
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(refreshStarted)
		<-releaseRefresh
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fresh", "refresh_token": "new-refresh", "expires_in": 900,
		})
	}))
	defer server.Close()
	stubOAuthConfig(t, server.URL)

	var transaction sync.Mutex
	store.RefreshTransaction = func(_ context.Context, mutate func() error) error {
		transaction.Lock()
		defer transaction.Unlock()
		return mutate()
	}
	refreshDone := make(chan error, 1)
	go func() {
		_, refreshErr := store.RefreshAccount(context.Background(), server.Client(), acct)
		refreshDone <- refreshErr
	}()
	<-refreshStarted
	removeDone := make(chan error, 1)
	go func() {
		transaction.Lock()
		defer transaction.Unlock()
		_, _, removeErr := store.RemoveManagedAccount("work")
		removeDone <- removeErr
	}()
	select {
	case err := <-removeDone:
		t.Fatalf("removal bypassed the in-flight refresh transaction: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseRefresh)
	if err := <-refreshDone; err != nil {
		t.Fatal(err)
	}
	if err := <-removeDone; err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.ReadManagedCredential("work", time.Now()); err != nil || ok {
		t.Fatalf("removed profile was resurrected (ok=%v err=%v)", ok, err)
	}
}

// A refresh response that omits the refresh token must not blank the stored
// one.
func TestRefreshAccountKeepsAnUnrotatedRefreshToken(t *testing.T) {
	store := writeCredentialFile(t, credentialFileJSON("stale", "rt", time.Now().Add(-time.Hour)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh-access", "expires_in": 900})
	}))
	defer server.Close()
	stubOAuthConfig(t, server.URL)

	if _, err := store.RefreshAccount(context.Background(), server.Client(), account.Account{ID: accountID}); err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	credential, _, err := store.ReadLocalCredential(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if credential.RefreshToken != "rt" {
		t.Fatal("existing refresh token was not preserved")
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
	stubOAuthConfig(t, server.URL)

	_, err := store.RefreshAccount(context.Background(), server.Client(), account.Account{ID: accountID})
	if err == nil {
		t.Fatal("a 401 on refresh must be an error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error %q must carry the status so the proxy marks the account for re-auth", err)
	}
}

func TestFetchUsageParsesRemainingAndUsedShapes(t *testing.T) {
	now := time.Now()
	reset := now.Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)
	tests := []struct {
		name       string
		detailJSON string
		wantUsed   float64
	}{
		{name: "current remaining shape", detailJSON: `"remaining":"75","limit":"100"`, wantUsed: 25},
		{name: "legacy used shape", detailJSON: `"used":"40","limit":"100"`, wantUsed: 40},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotAuthorization := ""
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuthorization = r.Header.Get("Authorization")
				_, _ = fmt.Fprintf(w, `{
					"usage":{%s,"resetTime":%q},
					"limits":[{"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"},"detail":{%s,"resetTime":%q}}]
				}`, tc.detailJSON, reset, tc.detailJSON, reset)
			}))
			defer server.Close()
			oldURL := usageURL
			usageURL = server.URL
			t.Cleanup(func() { usageURL = oldURL })

			plan, windows, err := (Store{}).FetchUsage(context.Background(), server.Client(), account.Account{Token: "access"})
			if err != nil {
				t.Fatal(err)
			}
			if gotAuthorization != "Bearer access" {
				t.Fatal("usage request did not carry the expected bearer credential")
			}
			if plan != "subscription" || len(windows) != 2 {
				t.Fatalf("plan/windows = %q/%+v", plan, windows)
			}
			if windows[0].Name != "weekly" || windows[0].UsedPercent != tc.wantUsed {
				t.Fatalf("weekly = %+v, want %.0f%%", windows[0], tc.wantUsed)
			}
			if windows[1].Name != "5h" || windows[1].LimitWindowSeconds != int64((5*time.Hour)/time.Second) || windows[1].UsedPercent != tc.wantUsed {
				t.Fatalf("short window = %+v, want 5h %.0f%%", windows[1], tc.wantUsed)
			}
			if windows[0].ResetAfterSeconds <= 0 || windows[1].ResetAfterSeconds <= 0 {
				t.Fatalf("reset times were not preserved: %+v", windows)
			}
		})
	}
}

func TestFetchUsageDoesNotRedirectBearerCredential(t *testing.T) {
	targetRequests := 0
	targetAuthorization := ""
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests++
		targetAuthorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	oldURL := usageURL
	usageURL = redirect.URL
	t.Cleanup(func() { usageURL = oldURL })

	_, _, err := (Store{}).FetchUsage(context.Background(), redirect.Client(), account.Account{Token: "access"})
	if err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("redirect response must be rejected, got %v", err)
	}
	if targetRequests != 0 || targetAuthorization != "" {
		t.Fatal("redirect target received the Kimi usage request or bearer credential")
	}
}

func TestAccountRefreshStatePreflightsManagedCredentialWithoutMutation(t *testing.T) {
	store := Store{ManagedDir: t.TempDir()}
	for _, testCase := range []struct {
		name      string
		expiresAt time.Time
		want      bool
	}{
		{name: "fresh", expiresAt: time.Now().Add(time.Hour), want: false},
		{name: "expiring", expiresAt: time.Now().Add(time.Minute), want: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			account, err := store.SaveManagedCredential(testCase.name, CredentialInfo{
				AccessToken: "access-" + testCase.name, RefreshToken: "refresh-" + testCase.name,
				OAuthDeviceID: "0123456789abcdef", ExpiresAt: testCase.expiresAt,
			})
			if err != nil {
				t.Fatal(err)
			}
			current, got, err := store.AccountRefreshState(account, time.Now())
			if err != nil || got != testCase.want {
				t.Fatalf("AccountRefreshState() = %v, %v, want %v, nil", got, err, testCase.want)
			}
			if current.Token != "access-"+testCase.name {
				t.Fatalf("current token = %q", current.Token)
			}
		})
	}
}
