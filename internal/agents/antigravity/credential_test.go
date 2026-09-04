package antigravity

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

var reference = time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

// The CLI self-updates in the background and has persisted two different
// expiry encodings: the Go client writes golang.org/x/oauth2.Token, whose
// expiry is RFC 3339 under "expiry"; the earlier Node client wrote
// "expiry_date" as epoch milliseconds. Both must read correctly, because
// misreading an expiry either burns a refresh on every request or hands an
// upstream a dead token.
func TestParseCredentialAcceptsBothExpiryEncodings(t *testing.T) {
	want := time.Date(2026, 8, 19, 13, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		body string
	}{
		{
			name: "oauth2.Token RFC 3339 expiry",
			body: `{"access_token":"at","refresh_token":"rt","token_type":"Bearer","expiry":"2026-08-19T13:00:00Z"}`,
		},
		{
			name: "node epoch-millisecond expiry_date",
			body: `{"access_token":"at","refresh_token":"rt","token_type":"Bearer","expiry_date":1787144400000}`,
		},
		{
			name: "epoch milliseconds as a string",
			body: `{"access_token":"at","refresh_token":"rt","token_type":"Bearer","expiry_date":"1787144400000"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			credential, err := ParseCredential([]byte(tc.body), "test", reference)
			if err != nil {
				t.Fatalf("parse failed: %v", err)
			}
			if !credential.ExpiresAt.Equal(want) {
				t.Fatalf("ExpiresAt = %s, want %s", credential.ExpiresAt, want)
			}
			if credential.AccessToken != "at" || credential.RefreshToken != "rt" {
				t.Fatalf("tokens did not round-trip: %+v", credential)
			}
		})
	}
}

// expires_in is relative, so it only means anything against a clock.
func TestParseCredentialTreatsPersistedRelativeExpiryAsUnknown(t *testing.T) {
	credential, err := ParseCredential([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600}`), "test", reference)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if !credential.ExpiresAt.IsZero() || !credential.NeedsRefresh(reference) {
		t.Fatalf("persisted relative expiry = %s, want unknown/refresh-required", credential.ExpiresAt)
	}
}

// An unparseable expiry must fail loudly. Falling back to a zero value would
// silently mark every credential expired and refresh on every single request.
func TestParseCredentialRejectsAnUnreadableExpiry(t *testing.T) {
	_, err := ParseCredential([]byte(`{"access_token":"at","refresh_token":"rt","expiry":"tomorrow"}`), "test", reference)
	if err == nil {
		t.Fatal("a non-RFC-3339 expiry must be an error, not a silent zero")
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
	body := []byte(`{"access_token":"ya29.secret-value","refresh_token":"1//secret-refresh"}` + "bplist00")
	_, err := ParseCredential(body, "antigravity keychain", reference)
	if err == nil {
		t.Fatal("trailing bytes must not decode")
	}
	message := err.Error()
	for _, want := range []string{unreadableCredentialPhrase, "from antigravity keychain", "trailing_kind=binary-plist"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q is missing %q", message, want)
		}
	}
	for _, secret := range []string{"ya29.secret-value", "1//secret-refresh"} {
		if strings.Contains(message, secret) {
			t.Fatalf("error leaked a secret: %q", message)
		}
	}
}

func TestNeedsRefreshUsesTheSameLeadAsTheCLI(t *testing.T) {
	live := CredentialInfo{AccessToken: "at", ExpiresAt: reference.Add(30 * time.Minute)}
	if live.NeedsRefresh(reference) {
		t.Fatal("a token with 30 minutes left does not need a refresh")
	}
	soon := CredentialInfo{AccessToken: "at", ExpiresAt: reference.Add(4 * time.Minute)}
	if !soon.NeedsRefresh(reference) {
		t.Fatal("a token inside the five-minute lead must be refreshed before use")
	}
	unknown := CredentialInfo{AccessToken: "at"}
	if !unknown.NeedsRefresh(reference) {
		t.Fatal("a credential with no stated expiry must be refreshed rather than trusted")
	}
	empty := CredentialInfo{ExpiresAt: reference.Add(time.Hour)}
	if !empty.NeedsRefresh(reference) {
		t.Fatal("a credential with no access token always needs a refresh")
	}
}

func TestStoreCachesRefreshedCredentialUntilItExpires(t *testing.T) {
	now := time.Now().UTC()
	reads := 0
	refreshes := 0
	store := &Store{
		readCredential: func(context.Context, time.Time) (CredentialInfo, bool, error) {
			reads++
			return CredentialInfo{AccessToken: "expired", RefreshToken: "refresh", ExpiresAt: now.Add(-time.Minute)}, true, nil
		},
		refreshCredential: func(_ context.Context, _ *http.Client, credential CredentialInfo, _ time.Time) (CredentialInfo, error) {
			refreshes++
			credential.AccessToken = "fresh"
			credential.ExpiresAt = now.Add(time.Hour)
			return credential, nil
		},
	}

	first, err := store.RefreshAccount(t.Context(), http.DefaultClient, account.Account{Token: "expired"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.RefreshAccount(t.Context(), http.DefaultClient, first)
	if err != nil {
		t.Fatal(err)
	}
	if first.Token != "fresh" || second.Token != "fresh" {
		t.Fatalf("tokens = %q, %q, want cached fresh token", first.Token, second.Token)
	}
	if reads != 2 || refreshes != 1 {
		t.Fatalf("keychain reads=%d refreshes=%d, want two authoritative reads and one refresh", reads, refreshes)
	}
}

// Once an in-process refresh rotates the refresh token, the unchanged
// keychain value is stale authority. ListAccounts must not resurrect it even
// after the cached access token enters its refresh window.
func TestListAccountsPreservesLocallyRotatedRefreshToken(t *testing.T) {
	now := time.Now().UTC()
	keychain := CredentialInfo{
		AccessToken: "keychain-access", RefreshToken: "keychain-refresh",
		ExpiresAt: now.Add(-time.Hour),
	}
	store := &Store{
		readCredential: func(context.Context, time.Time) (CredentialInfo, bool, error) {
			return keychain, true, nil
		},
		refreshCredential: func(_ context.Context, _ *http.Client, credential CredentialInfo, _ time.Time) (CredentialInfo, error) {
			if credential.RefreshToken != "keychain-refresh" {
				t.Fatalf("refresh input token = %q", credential.RefreshToken)
			}
			credential.AccessToken = "process-access"
			credential.RefreshToken = "process-rotated-refresh"
			// Keep the cached token inside the refresh window to prove that
			// freshness cannot authorize restoring the stale keychain token.
			credential.ExpiresAt = now.Add(time.Minute)
			return credential, nil
		},
	}

	refreshed, err := store.RefreshAccount(t.Context(), http.DefaultClient, account.Account{Token: "stale"})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := store.ListAccounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || refreshed.Token != "process-access" || listed[0].Token != "process-access" {
		t.Fatalf("refreshed=%+v listed=%+v, want locally refreshed access token", refreshed, listed)
	}
	if store.cached.RefreshToken != "process-rotated-refresh" {
		t.Fatalf("cached refresh token = %q, stale keychain token was resurrected", store.cached.RefreshToken)
	}
}

func TestStoreReplacesCachedCredentialWhenKeychainAccountChanges(t *testing.T) {
	now := time.Now().UTC()
	keychain := CredentialInfo{AccessToken: "account-a", RefreshToken: "refresh-a", ExpiresAt: now.Add(time.Hour)}
	store := &Store{readCredential: func(context.Context, time.Time) (CredentialInfo, bool, error) {
		return keychain, true, nil
	}}
	first, err := store.ListAccounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	keychain = CredentialInfo{AccessToken: "account-b", RefreshToken: "refresh-b", ExpiresAt: now.Add(time.Hour)}
	second, err := store.ListAccounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if first[0].Token != "account-a" || second[0].Token != "account-b" {
		t.Fatalf("tokens = %q then %q, want keychain account switch", first[0].Token, second[0].Token)
	}
}

func TestStoreRejectsCachedCredentialAfterKeychainLogout(t *testing.T) {
	now := time.Now().UTC()
	signedIn := true
	store := &Store{readCredential: func(context.Context, time.Time) (CredentialInfo, bool, error) {
		if !signedIn {
			return CredentialInfo{}, false, nil
		}
		return CredentialInfo{AccessToken: "account-a", RefreshToken: "refresh-a", ExpiresAt: now.Add(time.Hour)}, true, nil
	}}
	accounts, err := store.ListAccounts(t.Context())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("initial accounts = %+v err=%v", accounts, err)
	}
	signedIn = false
	if _, err := store.RefreshAccount(t.Context(), http.DefaultClient, accounts[0]); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("refresh after logout error = %v, want missing credential", err)
	}
	accounts, err = store.ListAccounts(t.Context())
	if err != nil || len(accounts) != 0 {
		t.Fatalf("accounts after logout = %+v err=%v", accounts, err)
	}
}

// The current CLI wraps the JSON payload in the keychain as
// "go-keyring-base64:" plus base64, and nests the oauth2.Token under "token"
// next to an "auth_method" marker. Both the wrapped-nested shape and the older
// flat shape must read, because the CLI self-updates in the background.
func TestParseCredentialUnwrapsTheKeyringEnvelope(t *testing.T) {
	inner := `{"access_token":"at","refresh_token":"rt","token_type":"Bearer","expiry":"2026-08-19T13:00:00Z"}`
	envelope := `{"token":` + inner + `,"auth_method":"oauth"}`
	wrapped := "go-keyring-base64:" + base64.StdEncoding.EncodeToString([]byte(envelope))
	credential, err := ParseCredential([]byte(wrapped), "antigravity keychain", reference)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if credential.AccessToken != "at" || credential.RefreshToken != "rt" {
		t.Fatalf("tokens did not round-trip: %+v", credential)
	}
	if want := time.Date(2026, 8, 19, 13, 0, 0, 0, time.UTC); !credential.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %s, want %s", credential.ExpiresAt, want)
	}
}

func TestEncodeCredentialRoundTripsCurrentKeyringEnvelope(t *testing.T) {
	want := CredentialInfo{AccessToken: "access", RefreshToken: "refresh", IDToken: "id", TokenType: "Bearer", Scope: "scope", ExpiresAt: time.Date(2026, 9, 3, 1, 2, 3, 0, time.UTC)}
	body, err := EncodeCredential(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseCredential(body, "test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken || got.IDToken != want.IDToken || got.TokenType != want.TokenType || got.Scope != want.Scope || !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

// stubOAuthClients fixes the candidate list for a refresh test and clears the
// working-pair cache, restoring both afterwards.
func stubOAuthClients(t *testing.T, clients ...oauthClient) {
	t.Helper()
	restore := oauthClientsForRefresh
	oauthClientsForRefresh = func() []oauthClient { return clients }
	workingClient.Store(nil)
	t.Cleanup(func() {
		oauthClientsForRefresh = restore
		workingClient.Store(nil)
	})
}

func stubTokenURL(t *testing.T, server *httptest.Server) {
	t.Helper()
	restore := oauthTokenURL
	oauthTokenURL = server.URL
	t.Cleanup(func() { oauthTokenURL = restore })
}

func TestRefreshCredentialExchangesTheRefreshToken(t *testing.T) {
	var gotForm string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm failed: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotForm = r.Form.Encode()
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q, want form encoding", r.Header.Get("Content-Type"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fresh-access",
			"expires_in":   3599,
			"token_type":   "Bearer",
			"id_token":     "fresh-id",
		})
	}))
	defer server.Close()
	stubTokenURL(t, server)
	stubOAuthClients(t, oauthClient{id: "test-client-id", secret: "test-client-secret"})

	refreshed, err := RefreshCredential(context.Background(), server.Client(),
		CredentialInfo{AccessToken: "stale", RefreshToken: "rt", IDToken: "old-id"}, reference)
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if refreshed.AccessToken != "fresh-access" {
		t.Fatalf("AccessToken = %q, want the refreshed value", refreshed.AccessToken)
	}
	// Google does not rotate the refresh token on every exchange, so an absent
	// one in the response must not blank the stored value.
	if refreshed.RefreshToken != "rt" {
		t.Fatalf("RefreshToken = %q, want the existing token preserved", refreshed.RefreshToken)
	}
	if refreshed.IDToken != "fresh-id" {
		t.Fatalf("IDToken = %q, want the refreshed value", refreshed.IDToken)
	}
	if want := reference.Add(3599 * time.Second); !refreshed.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %s, want %s", refreshed.ExpiresAt, want)
	}
	for _, want := range []string{"grant_type=refresh_token", "refresh_token=rt", "client_id="} {
		if !strings.Contains(gotForm, want) {
			t.Fatal("refresh request form is missing an expected field")
		}
	}
}

func TestRefreshCredentialDoesNotReplaySecretsAcrossRedirects(t *testing.T) {
	var sinkCalls int
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		sinkCalls++
	}))
	defer sink.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if err := request.ParseForm(); err != nil {
			t.Error("refresh request form could not be parsed")
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		if request.Form.Get("refresh_token") != "refresh-secret" || request.Form.Get("client_secret") != "client-secret" {
			t.Error("refresh request omitted an expected credential field")
			http.Error(w, "invalid credentials", http.StatusBadRequest)
			return
		}
		http.Redirect(w, request, sink.URL, http.StatusPermanentRedirect)
	}))
	defer redirector.Close()
	stubTokenURL(t, redirector)
	stubOAuthClients(t, oauthClient{id: "client-id", secret: "client-secret"})

	_, err := RefreshCredential(t.Context(), redirector.Client(), CredentialInfo{RefreshToken: "refresh-secret"}, reference)
	if err == nil || !strings.Contains(err.Error(), "308") {
		t.Fatalf("refresh error = %v, want rejected redirect", err)
	}
	if sinkCalls != 0 {
		t.Fatalf("redirect sink received %d credential-bearing requests", sinkCalls)
	}
}

// A rotated refresh token must replace the stored one, or the next refresh
// presents a token Google has already retired.
func TestRefreshCredentialAdoptsARotatedRefreshToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-access",
			"refresh_token": "rotated",
			"expires_in":    3600,
		})
	}))
	defer server.Close()
	stubTokenURL(t, server)
	stubOAuthClients(t, oauthClient{id: "test-client-id", secret: "test-client-secret"})

	refreshed, err := RefreshCredential(context.Background(), server.Client(),
		CredentialInfo{RefreshToken: "original"}, reference)
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if refreshed.RefreshToken != "rotated" {
		t.Fatalf("RefreshToken = %q, want the rotated token", refreshed.RefreshToken)
	}
}

// A revoked or reused grant comes back as invalid_grant, which the proxy
// already classifies as terminal.
func TestRefreshCredentialSurfacesInvalidGrant(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`))
	}))
	defer server.Close()
	stubTokenURL(t, server)
	stubOAuthClients(t, oauthClient{id: "test-client-id", secret: "test-client-secret"})

	_, err := RefreshCredential(context.Background(), server.Client(), CredentialInfo{RefreshToken: "dead"}, reference)
	if err == nil {
		t.Fatal("a 400 invalid_grant must be an error")
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("error %q must carry invalid_grant so the proxy marks the account for re-auth", err)
	}
}

func TestRefreshCredentialRejectsAMissingRefreshToken(t *testing.T) {
	if _, err := RefreshCredential(context.Background(), nil, CredentialInfo{AccessToken: "at"}, reference); err == nil {
		t.Fatal("a credential with no refresh token cannot be refreshed")
	}
}

// A 200 with no access token is a protocol violation, not a success.
func TestRefreshCredentialRejectsAnEmptyAccessToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"expires_in":3600}`))
	}))
	defer server.Close()
	stubTokenURL(t, server)
	stubOAuthClients(t, oauthClient{id: "test-client-id", secret: "test-client-secret"})

	if _, err := RefreshCredential(context.Background(), server.Client(), CredentialInfo{RefreshToken: "rt"}, reference); err == nil {
		t.Fatal("a 200 with no access token must be rejected")
	}
}

// The CLI binary carries more than one OAuth client and does not record which
// one a credential was issued to, so a rejection of the client — not of the
// credential — must advance to the next candidate.
func TestRefreshCredentialTriesTheNextClientOnInvalidClient(t *testing.T) {
	var attempts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm failed: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		attempts = append(attempts, r.Form.Get("client_id"))
		if r.Form.Get("client_secret") == "wrong-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			// What Google actually returns for a known id with a wrong secret.
			_, _ = w.Write([]byte(`{"error":"unauthorized_client","error_description":"Unauthorized"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh-access", "expires_in": 3600})
	}))
	defer server.Close()
	stubTokenURL(t, server)
	stubOAuthClients(t,
		oauthClient{id: "wrong-client", secret: "wrong-secret"},
		oauthClient{id: "right-client", secret: "right-secret"},
	)

	refreshed, err := RefreshCredential(context.Background(), server.Client(), CredentialInfo{RefreshToken: "rt"}, reference)
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if refreshed.AccessToken != "fresh-access" {
		t.Fatalf("AccessToken = %q, want the refreshed value", refreshed.AccessToken)
	}
	if len(attempts) != 2 || attempts[0] != "wrong-client" || attempts[1] != "right-client" {
		t.Fatalf("attempts = %v, want the wrong client tried once then the right one", attempts)
	}

	// The working pair is cached, so the next refresh presents it first
	// instead of re-paying the failed attempt.
	attempts = nil
	if _, err := RefreshCredential(context.Background(), server.Client(), CredentialInfo{RefreshToken: "rt"}, reference); err != nil {
		t.Fatalf("second refresh failed: %v", err)
	}
	if len(attempts) != 1 || attempts[0] != "right-client" {
		t.Fatalf("attempts = %v, want only the cached working client", attempts)
	}
}

// invalid_grant is about the credential, not the client. Retrying it against
// every candidate would multiply a terminal failure into one per client.
func TestRefreshCredentialDoesNotRetryInvalidGrantAcrossClients(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`))
	}))
	defer server.Close()
	stubTokenURL(t, server)
	stubOAuthClients(t,
		oauthClient{id: "first", secret: "first-secret"},
		oauthClient{id: "second", secret: "second-secret"},
	)

	_, err := RefreshCredential(context.Background(), server.Client(), CredentialInfo{RefreshToken: "dead"}, reference)
	if err == nil {
		t.Fatal("a 400 invalid_grant must be an error")
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("error %q must carry invalid_grant so the proxy marks the account for re-auth", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want exactly one: invalid_grant is terminal for the credential", attempts)
	}
}

func TestPrepareManagedCredentialDiscoversClientAcrossInvalidGrant(t *testing.T) {
	var attempts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		attempts = append(attempts, r.Form.Get("client_id"))
		if r.Form.Get("client_id") == "old-client" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "refresh_token": "rotated", "expires_in": 3600})
	}))
	defer server.Close()
	stubTokenURL(t, server)
	stubOAuthClients(t,
		oauthClient{id: "old-client", secret: "old-secret"},
		oauthClient{id: "current-client", secret: "current-secret"},
	)

	credential, err := PrepareManagedCredential(context.Background(), server.Client(), CredentialInfo{RefreshToken: "rt"}, reference)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 || credential.OAuthClientID != "current-client" || credential.OAuthClientSecret != "current-secret" || credential.RefreshToken != "rotated" {
		t.Fatalf("prepared credential attempts=%v client=%q secret=%q refresh=%q", attempts, credential.OAuthClientID, credential.OAuthClientSecret, credential.RefreshToken)
	}
}

func TestRefreshCredentialUsesCachedClientBeforeDiscoveringCandidates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "expires_in": 3600})
	}))
	defer server.Close()
	stubTokenURL(t, server)
	discoveryCalls := 0
	restore := oauthClientsForRefresh
	oauthClientsForRefresh = func() []oauthClient { discoveryCalls++; return nil }
	workingClient.Store(&oauthClient{id: "cached", secret: "cached-secret"})
	t.Cleanup(func() { oauthClientsForRefresh = restore; workingClient.Store(nil) })
	if _, err := RefreshCredential(context.Background(), server.Client(), CredentialInfo{RefreshToken: "rt"}, reference); err != nil {
		t.Fatal(err)
	}
	if discoveryCalls != 0 {
		t.Fatalf("candidate discovery ran %d times with a working cached client", discoveryCalls)
	}
}

// With no installed CLI and no configured client there is nothing to refresh
// with; the error must say so rather than reporting an upstream failure.
func TestRefreshCredentialReportsWhenNoClientIsAvailable(t *testing.T) {
	stubOAuthClients(t)
	_, err := RefreshCredential(context.Background(), nil, CredentialInfo{RefreshToken: "rt"}, reference)
	if err == nil {
		t.Fatal("refresh with no available OAuth client must fail")
	}
	if !strings.Contains(err.Error(), "no Antigravity OAuth client available") {
		t.Fatalf("error %q should name the missing client and the remedy", err)
	}
}

// The binary scan must find every client id and secret the CLI carries and
// pair each id with each secret, because the binary does not record which
// belong together.
func TestOAuthClientsFromBinaryExtractsTheCrossProduct(t *testing.T) {
	// The fixture values are assembled at run time so the source carries no
	// string shaped like a real Google client id or secret; committing one,
	// even a fake, trips push protection.
	idSuffix := ".apps.googleusercontent" + ".com"
	idOne := "1111111111111-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + idSuffix
	idTwo := "2222222222222-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" + idSuffix
	secretOne := "GOCSPX-" + "fakeSecretOne000000000000000"
	secretTwo := "GOCSPX-" + "fakeSecretTwo000000000000000"
	binary := bytes.Join([][]byte{
		[]byte("padding\x00" + idOne + "\x00"),
		[]byte(secretOne),
		[]byte(idTwo),
		[]byte(secretTwo + "\x00tail"),
		[]byte(idOne),
	}, []byte("noise"))
	path := filepath.Join(t.TempDir(), "agy")
	if err := os.WriteFile(path, binary, 0o600); err != nil {
		t.Fatal(err)
	}
	clients := oauthClientsFromBinary(path)
	if len(clients) != 4 {
		t.Fatalf("got %d candidates, want 2 ids x 2 secrets = 4: %+v", len(clients), clients)
	}
	seen := make(map[oauthClient]bool)
	for _, client := range clients {
		seen[client] = true
	}
	for _, want := range []oauthClient{
		{idOne, secretOne},
		{idOne, secretTwo},
		{idTwo, secretOne},
		{idTwo, secretTwo},
	} {
		if !seen[want] {
			t.Fatalf("candidate %+v missing from %+v", want, clients)
		}
	}
}

func TestOAuthClientsFromBinaryHandlesAMissingBinary(t *testing.T) {
	if clients := oauthClientsFromBinary(filepath.Join(t.TempDir(), "absent")); clients != nil {
		t.Fatalf("got %+v, want no candidates for a missing binary", clients)
	}
	if clients := oauthClientsFromBinary(""); clients != nil {
		t.Fatalf("got %+v, want no candidates without a binary path", clients)
	}
}

// An explicitly configured client wins over the binary scan, and a half-set
// pair is ignored rather than presented upstream.
func TestOAuthClientFromEnvRequiresBothValues(t *testing.T) {
	t.Setenv("SUBROUTER_ANTIGRAVITY_CLIENT_ID", "env-id")
	t.Setenv("SUBROUTER_ANTIGRAVITY_CLIENT_SECRET", "env-secret")
	client, ok := oauthClientFromEnv()
	if !ok || client.id != "env-id" || client.secret != "env-secret" {
		t.Fatalf("got %+v, %v; want the configured pair", client, ok)
	}

	t.Setenv("SUBROUTER_ANTIGRAVITY_CLIENT_SECRET", "")
	if _, ok := oauthClientFromEnv(); ok {
		t.Fatal("a client id without a secret must not be used")
	}
}

func TestCredentialDisplayLabelUsesSafeStoredIdentityClaim(t *testing.T) {
	encode := func(claims string) string {
		return "header." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".signature"
	}
	if got := credentialDisplayLabel(CredentialInfo{IDToken: encode(`{"email":"person@example.test"}`)}); got != "person@example.test" {
		t.Fatalf("label = %q", got)
	}
	for _, token := range []string{
		"not-a-jwt",
		encode(`{"email":"not-an-email"}`),
		encode("{\"email\":\"unsafe@example.test\\nspoof\"}"),
		encode("{\"email\":\"unsafe@example.test\\u202espoof\"}"),
		encode("{\"email\":\"unsafe@example.test\\u200dspoof\"}"),
		encode("{\"email\":\"unsafe@example.test\\u2028spoof\"}"),
		encode("{\"email\":\"unsafe@example.test\\u2029spoof\"}"),
	} {
		if got := credentialDisplayLabel(CredentialInfo{IDToken: token}); got != "router agy login" {
			t.Fatalf("unsafe token label = %q", got)
		}
	}
}
