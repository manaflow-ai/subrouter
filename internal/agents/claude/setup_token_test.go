package claude

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testSetupToken = "sk-ant-oat01-TESTTOKENTESTTOKENTESTTOKENTESTTOKENTESTTOKENTESTTOKENTESTTOKENTESTTOKENTESTTOKENTESTTOK-FAKEFAKEAA"

func TestValidateSetupTokenShape(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  string
	}{
		{"valid", testSetupToken, ""},
		{"empty", "", "empty"},
		{"api key", "sk-ant-api03-" + strings.Repeat("x", 60), "API key"},
		{"wrong prefix", "ghp_" + strings.Repeat("x", 60), "must start with"},
		{"short", "sk-ant-oat01-abc", "unexpected length"},
		{"whitespace", "sk-ant-oat01-" + strings.Repeat("x", 30) + " " + strings.Repeat("x", 30), "whitespace"},
	}
	for _, tc := range cases {
		err := ValidateSetupToken(tc.token)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want containing %q", tc.name, err, tc.want)
		}
	}
}

func TestSetupTokenCredentialRecordsOneYearExpiryAndScope(t *testing.T) {
	issued := time.Date(2026, 9, 2, 22, 0, 0, 0, time.UTC)
	credential := SetupTokenCredential(testSetupToken, issued)
	if !credential.LongLived() {
		t.Fatal("setup token credential must be long-lived")
	}
	expiresAt, ok := credential.ExpiresAtTime()
	if !ok || !expiresAt.Equal(issued.Add(SetupTokenLifetime)) {
		t.Fatalf("expiresAt = %v ok=%v, want %v", expiresAt, ok, issued.Add(SetupTokenLifetime))
	}
	if len(credential.Scopes) != 1 || credential.Scopes[0] != SetupTokenScope {
		t.Fatalf("scopes = %v, want [%s]", credential.Scopes, SetupTokenScope)
	}
	if err := credential.validateAt(issued.Add(24 * time.Hour)); err != nil {
		t.Fatalf("fresh setup token must validate: %v", err)
	}
	if err := credential.validateAt(issued.Add(SetupTokenLifetime)); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired setup token validate error = %v, want expired", err)
	}
	oauth := CredentialInfo{AccessToken: "access", RefreshToken: "refresh"}
	if oauth.LongLived() {
		t.Fatal("a refreshable credential is not long-lived")
	}
	if err := oauth.Validate(); err != nil {
		t.Fatalf("refreshable credential must validate regardless of expiry: %v", err)
	}
	if err := (CredentialInfo{AccessToken: "access"}).Validate(); err == nil {
		t.Fatal("a refresh-less credential without an expiry must not validate")
	}
}

func TestCredentialEqualComparesScopes(t *testing.T) {
	a := CredentialInfo{AccessToken: "x", ExpiresAt: 1, Scopes: []string{"user:inference"}}
	b := CredentialInfo{AccessToken: "x", ExpiresAt: 1, Scopes: []string{"user:inference"}}
	if !a.Equal(b) {
		t.Fatal("identical credentials must be equal")
	}
	b.Scopes = []string{"user:profile"}
	if a.Equal(b) {
		t.Fatal("different scopes must not be equal")
	}
}

func TestImportProfileCredentialAcceptsSetupTokenAndPreservesScopes(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	credential := SetupTokenCredential(testSetupToken, time.Now())
	if err := store.ImportProfileCredential("work", credential); err != nil {
		t.Fatalf("import setup token: %v", err)
	}
	profile, ok := store.FindProfile("work")
	if !ok {
		t.Fatal("profile was not registered")
	}
	body, err := os.ReadFile(filepath.Join(store.InstancePath(profile.Name), ".credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"accessToken"`, `"scopes"`, `"user:inference"`, `"expiresAt"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("credential file missing %s", want)
		}
	}
	if strings.Contains(string(body), "refreshToken") {
		t.Error("credential file must not carry an empty refreshToken")
	}
	stored, err := store.ReadCredential(context.Background(), store.ClaudeConfigDir(profile.Name))
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Equal(credential) {
		t.Fatalf("stored credential = %+v, want %+v", *stored, credential)
	}

	expired := SetupTokenCredential(testSetupToken, time.Now().Add(-SetupTokenLifetime-time.Hour))
	if err := store.ImportProfileCredential("stale", expired); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired setup token import error = %v, want expired", err)
	}
	if err := store.ImportProfileCredential("bare", CredentialInfo{AccessToken: "access"}); err == nil {
		t.Fatal("refresh-less credential without expiry must be rejected")
	}
}

func TestRefreshPathsKeepSetupTokenUsableUntilExpiry(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	credential := SetupTokenCredential(testSetupToken, time.Now())
	if err := store.ImportProfileCredential("work", credential); err != nil {
		t.Fatal(err)
	}
	profile, _ := store.FindProfile("work")
	ctx := context.Background()
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("a setup token must never trigger an OAuth refresh request")
		return nil, nil
	})}

	account, refreshed, err := store.RefreshCredentialIfExpired(ctx, client, profile)
	if err != nil || refreshed || account.Token != testSetupToken {
		t.Fatalf("RefreshCredentialIfExpired = (%q, %v, %v)", account.Token, refreshed, err)
	}
	// Forced status refreshes must not classify a setup token as a dead
	// credential; before this change the "has no refresh token" error was
	// terminal and removed the account from routing.
	account, refreshed, err = store.ForceRefreshCredential(ctx, client, profile)
	if err != nil || refreshed || account.Token != testSetupToken {
		t.Fatalf("ForceRefreshCredential = (%q, %v, %v)", account.Token, refreshed, err)
	}
	if _, _, needsRefresh, err := store.CredentialRefreshState(ctx, profile, time.Now()); err != nil || needsRefresh {
		t.Fatalf("CredentialRefreshState = (needsRefresh=%v, %v)", needsRefresh, err)
	}

	// Once the recorded expiry passes the credential fails closed with the
	// terminal phrase the proxy recognizes.
	afterExpiry := time.UnixMilli(credential.ExpiresAt).Add(time.Minute)
	if _, _, _, err := store.CredentialRefreshState(ctx, profile, afterExpiry); err == nil ||
		!strings.Contains(err.Error(), "no usable credential") ||
		!strings.Contains(err.Error(), "sr claude add work") {
		t.Fatalf("expired CredentialRefreshState error = %v", err)
	}
	if err := longLivedCredentialError("work", &credential, afterExpiry); err == nil {
		t.Fatal("expired long-lived credential must error")
	}
	if err := longLivedCredentialError("work", &CredentialInfo{AccessToken: "a", RefreshToken: "r", ExpiresAt: 1}, afterExpiry); err != nil {
		t.Fatalf("refreshable credential must never hit the long-lived error: %v", err)
	}
}

func TestVerifyAccessTokenClassifiesAnthropicAnswer(t *testing.T) {
	var status int
	var sawAuth, sawBeta string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		sawBeta = r.Header.Get("anthropic-beta")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid token"}}`))
	}))
	defer server.Close()
	previous := messagesURL
	messagesURL = server.URL
	defer func() { messagesURL = previous }()

	for _, tc := range []struct {
		status   int
		rejected bool
		fails    bool
	}{
		{http.StatusOK, false, false},
		{http.StatusTooManyRequests, false, false},
		{http.StatusBadRequest, false, false},
		{http.StatusUnauthorized, true, true},
		{http.StatusForbidden, true, true},
		{http.StatusBadGateway, false, true},
	} {
		status = tc.status
		err := VerifyAccessToken(context.Background(), server.Client(), testSetupToken)
		if (err != nil) != tc.fails {
			t.Errorf("status %d: err = %v, want failure=%v", tc.status, err, tc.fails)
		}
		if errors.Is(err, ErrSetupTokenRejected) != tc.rejected {
			t.Errorf("status %d: rejected = %v, want %v (err %v)", tc.status, errors.Is(err, ErrSetupTokenRejected), tc.rejected, err)
		}
	}
	if sawAuth != "Bearer "+testSetupToken {
		t.Errorf("Authorization = %q", sawAuth)
	}
	if !strings.Contains(sawBeta, oauthBetaHeader) || !strings.Contains(sawBeta, "claude-code-20250219") {
		t.Errorf("anthropic-beta = %q, want Claude Code probe shape", sawBeta)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
