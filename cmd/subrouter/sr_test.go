package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	baseaccount "github.com/manaflow-ai/subrouter/account"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	agentgrok "github.com/manaflow-ai/subrouter/internal/agents/grok"
	agentkimi "github.com/manaflow-ai/subrouter/internal/agents/kimi"
	agentqwen "github.com/manaflow-ai/subrouter/internal/agents/qwen"
	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
	"github.com/mattn/go-runewidth"
)

type fakeKimiUsageStore struct {
	accounts []baseaccount.Account
	plan     string
	windows  []accounts.UsageWindow
	err      error
	fetches  *int
}

type fakeGrokStore struct {
	authorized bool
	saved      bool
	removed    bool
	refreshDid bool
	refreshes  int
	preflights int
	credential agentgrok.CredentialInfo
	account    baseaccount.Account
}

func (s *fakeGrokStore) Authorize(context.Context, *http.Client, io.Writer) (agentgrok.CredentialInfo, error) {
	s.authorized = true
	return s.credential, nil
}

func (s *fakeGrokStore) SaveCredential(credential agentgrok.CredentialInfo) (baseaccount.Account, error) {
	s.saved = true
	s.credential = credential
	return s.account, nil
}

func (s *fakeGrokStore) RemoveCredential() (baseaccount.Account, bool, error) {
	if s.account.ID == "" {
		return baseaccount.Account{}, false, nil
	}
	removed := s.account
	s.account = baseaccount.Account{}
	s.removed = true
	return removed, true, nil
}

func (s *fakeGrokStore) ListAccounts(context.Context) ([]baseaccount.Account, error) {
	if s.account.ID == "" {
		return nil, nil
	}
	return []baseaccount.Account{s.account}, nil
}

func (s *fakeGrokStore) RefreshAccount(_ context.Context, _ *http.Client, account baseaccount.Account) (baseaccount.Account, error) {
	return account, nil
}

func (s *fakeGrokStore) RefreshAccountIfNeeded(_ context.Context, _ *http.Client, account baseaccount.Account) (baseaccount.Account, bool, error) {
	s.refreshes++
	return account, s.refreshDid, nil
}

func (s *fakeGrokStore) AccountRefreshState(account baseaccount.Account, _ time.Time) (baseaccount.Account, bool, error) {
	s.preflights++
	return account, s.refreshDid, nil
}

func TestGrokSignInAuthorizesThenPublishesWithoutPrintingIdentityOrTokens(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	fake := &fakeGrokStore{
		credential: agentgrok.CredentialInfo{
			AccessToken: "secret-access", RefreshToken: "secret-refresh",
			ExpiresAt: time.Now().Add(time.Hour), Email: "private@example.com",
		},
		account: baseaccount.Account{
			ID: "grok-subscription", Provider: baseaccount.ProviderGrok,
			AuthMode: baseaccount.AuthModeOAuth, Token: "secret-access",
		},
	}
	var out bytes.Buffer
	runner := srRunner{store: store, out: &out, grok: fake}
	if err := runner.grokSignIn(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !fake.authorized || !fake.saved {
		t.Fatalf("authorize=%v saved=%v, want both lifecycle stages", fake.authorized, fake.saved)
	}
	if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); err != nil {
		t.Fatalf("Grok add did not publish account generation: %v", err)
	}
	message := out.String()
	for _, secret := range []string{"secret-access", "secret-refresh", "private@example.com"} {
		if strings.Contains(message, secret) {
			t.Fatalf("public success text leaked %q: %q", secret, message)
		}
	}
	if !strings.Contains(message, "Added Grok subscription account") {
		t.Fatalf("success text = %q", message)
	}
}

func TestRemoteAndHostedGrokAddAreExplicitlyUnsupported(t *testing.T) {
	runner := srRunner{program: "sr", out: io.Discard}
	remoteErr := runner.runRemoteAccountCommand(t.Context(), srServerConfig{Name: "test", URL: "https://example.invalid"}, []string{"add", "grok"})
	if remoteErr == nil || !strings.Contains(remoteErr.Error(), "Grok subscription import is not available yet") || !strings.Contains(remoteErr.Error(), "sr remote use local") {
		t.Fatalf("remote Grok error = %v", remoteErr)
	}
	hostedErr := runner.hostedAccountAdd(t.Context(), nil, []string{"grok"})
	if hostedErr == nil || !strings.Contains(hostedErr.Error(), "hosted Grok subscription accounts are not supported yet") || !strings.Contains(hostedErr.Error(), "sr remote use local") {
		t.Fatalf("hosted Grok error = %v", hostedErr)
	}
}

func TestGrokRemovePublishesAccountGeneration(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	fake := &fakeGrokStore{account: baseaccount.Account{
		ID: "grok-subscription", Provider: baseaccount.ProviderGrok, AuthMode: baseaccount.AuthModeOAuth,
	}}
	var out bytes.Buffer
	runner := srRunner{store: store, out: &out, grok: fake}
	if err := runner.remove(t.Context(), "grok-subscription"); err != nil {
		t.Fatal(err)
	}
	if !fake.removed {
		t.Fatal("Grok credential was not removed")
	}
	if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); err != nil {
		t.Fatalf("Grok removal did not publish account generation: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Removed account: grok-subscription") {
		t.Fatalf("removal output = %q", got)
	}
}

func TestGrokRemoveAcceptsDisplayedEmail(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	fake := &fakeGrokStore{account: baseaccount.Account{
		ID: "grok-subscription", Provider: baseaccount.ProviderGrok,
		AuthMode: baseaccount.AuthModeOAuth, Email: "shown@example.com",
		Label: "Grok (shown@example.com)",
	}}
	runner := srRunner{store: store, out: io.Discard, grok: fake}
	if err := runner.remove(t.Context(), "shown@example.com"); err != nil {
		t.Fatal(err)
	}
	if !fake.removed {
		t.Fatal("displayed Grok email did not remove the subscription account")
	}
}

func TestGrokRemoveRejectsEmailSharedWithStoredAccount(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email: "shared@example.com",
		Auth:  accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "codex-key"},
	}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeGrokStore{account: baseaccount.Account{
		ID: "grok-subscription", Provider: baseaccount.ProviderGrok,
		AuthMode: baseaccount.AuthModeOAuth, Email: "shared@example.com",
	}}
	runner := srRunner{store: store, out: io.Discard, grok: fake}
	err := runner.remove(t.Context(), "shared@example.com")
	if err == nil || !strings.Contains(err.Error(), "matches both") || !strings.Contains(err.Error(), "grok-subscription") {
		t.Fatalf("ambiguous removal error = %v", err)
	}
	if fake.removed {
		t.Fatal("ambiguous selector removed the Grok credential")
	}
	if _, ok, err := store.FindStored("shared@example.com"); err != nil || !ok {
		t.Fatalf("ambiguous selector removed stored account: ok=%v err=%v", ok, err)
	}
}

type refreshingKimiUsageStore struct {
	fetchedToken   string
	needsRefresh   bool
	refreshes      int
	preflights     int
	preflightToken string
}

type committedRefreshErrorRefresher struct {
	refreshed baseaccount.Account
	err       error
}

func (s committedRefreshErrorRefresher) RefreshAccountIfNeeded(context.Context, *http.Client, baseaccount.Account) (baseaccount.Account, bool, error) {
	return s.refreshed, true, s.err
}

type partialKimiUsageStore struct{}

func (partialKimiUsageStore) ListAccounts(context.Context) ([]baseaccount.Account, error) {
	return []baseaccount.Account{{
		ID: "kimi-subscription:healthy", Provider: baseaccount.ProviderKimi, AuthMode: baseaccount.AuthModeOAuth, Label: "healthy", Token: "access",
	}}, errors.New("managed profile is unreadable")
}

func (partialKimiUsageStore) FetchUsage(context.Context, *http.Client, baseaccount.Account) (string, []accounts.UsageWindow, error) {
	return "subscription", []accounts.UsageWindow{{Name: "weekly", UsedPercent: 1}}, nil
}

func (*refreshingKimiUsageStore) ListAccounts(context.Context) ([]baseaccount.Account, error) {
	return []baseaccount.Account{{
		ID: "kimi-subscription:work", Provider: baseaccount.ProviderKimi, AuthMode: baseaccount.AuthModeOAuth, Token: "stale",
	}}, nil
}

func (s *refreshingKimiUsageStore) RefreshAccountIfNeeded(_ context.Context, _ *http.Client, acct baseaccount.Account) (baseaccount.Account, bool, error) {
	s.refreshes++
	acct.Token = "fresh"
	return acct, true, nil
}

func (s *refreshingKimiUsageStore) AccountRefreshState(acct baseaccount.Account, _ time.Time) (baseaccount.Account, bool, error) {
	s.preflights++
	if s.preflightToken != "" {
		acct.Token = s.preflightToken
	}
	return acct, s.needsRefresh, nil
}

func (s *refreshingKimiUsageStore) FetchUsage(_ context.Context, _ *http.Client, acct baseaccount.Account) (string, []accounts.UsageWindow, error) {
	s.fetchedToken = acct.Token
	return "subscription", []accounts.UsageWindow{{Name: "weekly", UsedPercent: 1}}, nil
}

func TestLocalKimiRemovalPublishesAccountGeneration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", root)
	store := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
	if _, err := agentkimi.DefaultStore().SaveManagedCredential("work", agentkimi.CredentialInfo{
		AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runner := srRunner{store: store, out: &out}
	if err := runner.kimiCommand(t.Context(), []string{"remove", "work"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); err != nil {
		t.Fatalf("Kimi removal did not publish the shared account generation: %v", err)
	}
	if _, ok, err := agentkimi.DefaultStore().ReadManagedCredential("work", time.Now()); err != nil || ok {
		t.Fatalf("Kimi profile remains after removal (ok=%v err=%v)", ok, err)
	}
}

func TestGenericRemoveStillRemovesKimiAPIKeyAccounts(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	store := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
	apiKey, _, err := store.AddAPIKeyForProvider("work", "test-kimi-key", accounts.ProviderKimi)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runner := srRunner{store: store, out: &out}
	if err := runner.remove(t.Context(), apiKey.Email); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.FindStored(apiKey.Email); err != nil || ok {
		t.Fatalf("Kimi API key remains after generic removal (ok=%v err=%v)", ok, err)
	}
}

func TestLocalKimiStatusRefreshesBeforeFetchingUsage(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	store := &refreshingKimiUsageStore{needsRefresh: true}
	runner := srRunner{
		store: accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")},
		kimi:  store,
	}
	rows, err := runner.fetchUsageRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if store.fetchedToken != "fresh" {
		t.Fatal("local Kimi status fetched usage with the stale access token")
	}
	if store.preflights != 1 || store.refreshes != 1 {
		t.Fatalf("Kimi refresh preflights=%d refreshes=%d, want 1/1", store.preflights, store.refreshes)
	}
	found := false
	for _, row := range rows {
		if row.email == "kimi-subscription:work" && row.err == nil {
			found = true
		}
	}
	if !found {
		t.Fatal("refreshed Kimi status row is missing")
	}
}

func TestStatusRefreshPublishesAndReturnsCommittedAccountWithPostCommitError(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	original := baseaccount.Account{ID: "kimi-code", Provider: baseaccount.ProviderKimi, AuthMode: baseaccount.AuthModeOAuth, Token: "stale"}
	committed := original
	committed.Token = "fresh"
	postCommitErr := errors.New("post-commit lock release failed")
	runner := srRunner{store: store, client: http.DefaultClient}

	got, err := runner.refreshStatusOAuthAccount(t.Context(), original, committedRefreshErrorRefresher{
		refreshed: committed,
		err:       postCommitErr,
	})
	if !errors.Is(err, postCommitErr) {
		t.Fatalf("status refresh error = %v, want post-commit error", err)
	}
	if got.Token != "fresh" {
		t.Fatalf("status refresh returned token %q, want committed token", got.Token)
	}
	if _, statErr := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); statErr != nil {
		t.Fatalf("committed refresh was not published: %v", statErr)
	}
}

func TestLocalProviderStatusDoesNotPublishFreshOAuthCredentials(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))

	kimi := &refreshingKimiUsageStore{preflightToken: "current-kimi"}
	grok := &fakeGrokStore{account: baseaccount.Account{
		ID: "grok-subscription", Provider: baseaccount.ProviderGrok,
		AuthMode: baseaccount.AuthModeOAuth, Token: "fresh-grok",
	}}
	if _, err := (srRunner{store: store, kimi: kimi, grok: grok}).fetchUsageRows(t.Context()); err != nil {
		t.Fatal(err)
	}
	if kimi.preflights != 1 || kimi.refreshes != 0 {
		t.Fatalf("Kimi preflights=%d refreshes=%d, want 1/0", kimi.preflights, kimi.refreshes)
	}
	if kimi.fetchedToken != "current-kimi" {
		t.Fatalf("Kimi status used %q, want the credential returned by preflight", kimi.fetchedToken)
	}
	if grok.preflights != 1 || grok.refreshes != 0 {
		t.Fatalf("Grok preflights=%d refreshes=%d, want 1/0", grok.preflights, grok.refreshes)
	}
	if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); !os.IsNotExist(err) {
		t.Fatalf("fresh provider status published an account generation: %v", err)
	}
}

func TestLocalClaudeStatusRefreshPublishesBeforeUsageAndReloadsAccount(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	store := accounts.DefaultCodexStore()
	claudeStore := agentclaude.DefaultStore()
	profile := seedClaudeStatusCredential(t, claudeStore, "primary", agentclaude.CredentialInfo{
		AccessToken: "stale-access", RefreshToken: "stale-refresh",
		ExpiresAt: time.Now().Add(-time.Hour).UnixMilli(), SubscriptionType: "pro",
	})

	ref, err := proxy.OpenAccountRef(store, claudeStore, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if got := accountToken(ref.All(), accounts.ProviderClaude, profile.Name); got != "stale-access" {
		t.Fatalf("initial Claude token = %q, want stale-access", got)
	}

	var refreshCalls, usageCalls atomic.Int32
	client := &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/v1/oauth/token":
			refreshCalls.Add(1)
			return srJSONResponse(req, http.StatusOK, `{"access_token":"fresh-access","refresh_token":"fresh-refresh","expires_in":3600}`), nil
		case "/api/oauth/usage":
			usageCalls.Add(1)
			if got := req.Header.Get("Authorization"); got != "Bearer fresh-access" {
				return nil, fmt.Errorf("Claude usage authorization = %q, want refreshed access token", got)
			}
			if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); err != nil {
				return nil, fmt.Errorf("Claude refresh was not published before usage: %w", err)
			}
			lockCtx, cancel := context.WithTimeout(req.Context(), time.Second)
			defer cancel()
			if err := proxy.PublishAccountDiskMutation(lockCtx, store.StoreDir(), func() (bool, error) {
				return false, nil
			}); err != nil {
				return nil, fmt.Errorf("Claude usage ran while account transaction remained locked: %w", err)
			}
			return srJSONResponse(req, http.StatusOK, `{"seven_day_oauth_apps":{"utilization":0.1,"resets_at":""}}`), nil
		default:
			return nil, fmt.Errorf("unexpected Claude status request %s %s", req.Method, req.URL)
		}
	})}

	rows, err := (srRunner{store: store, client: client}).fetchUsageRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if row := usageRowFor(rows, accounts.ProviderClaude, profile.Name); row == nil || row.err != nil {
		t.Fatalf("refreshed Claude status row = %+v", row)
	}
	if refreshCalls.Load() != 1 || usageCalls.Load() != 1 {
		t.Fatalf("Claude status calls refresh=%d usage=%d, want 1/1", refreshCalls.Load(), usageCalls.Load())
	}
	daemon := (proxy.Server{AccountRef: ref, AdminToken: "test-admin"}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/_subrouter/accounts", nil)
	request.Header.Set("Authorization", "Bearer test-admin")
	response := httptest.NewRecorder()
	daemon.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("daemon account lookup status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := accountToken(ref.All(), accounts.ProviderClaude, profile.Name); got != "fresh-access" {
		t.Fatalf("generation-triggered daemon reload token = %q, want fresh-access", got)
	}
}

func TestLocalClaudeStatusDoesNotPublishFreshCredential(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	store := accounts.DefaultCodexStore()
	claudeStore := agentclaude.DefaultStore()
	profile := seedClaudeStatusCredential(t, claudeStore, "fresh", agentclaude.CredentialInfo{
		AccessToken: "current-access", RefreshToken: "current-refresh",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), SubscriptionType: "max",
	})

	client := &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/api/oauth/usage" {
			return nil, fmt.Errorf("fresh Claude credential made unexpected request %s %s", req.Method, req.URL)
		}
		return srJSONResponse(req, http.StatusOK, `{"seven_day_oauth_apps":{"utilization":0.1,"resets_at":""}}`), nil
	})}
	rows, err := (srRunner{store: store, client: client}).fetchUsageRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if row := usageRowFor(rows, accounts.ProviderClaude, profile.Name); row == nil || row.err != nil {
		t.Fatalf("fresh Claude status row = %+v", row)
	}
	if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); !os.IsNotExist(err) {
		t.Fatalf("fresh Claude status published an account generation: %v", err)
	}
}

func TestLocalClaudeStatusPublicationFailurePreventsRefresh(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	store := accounts.DefaultCodexStore()
	claudeStore := agentclaude.DefaultStore()
	profile := seedClaudeStatusCredential(t, claudeStore, "blocked", agentclaude.CredentialInfo{
		AccessToken: "stale-access", RefreshToken: "stale-refresh",
		ExpiresAt: time.Now().Add(-time.Hour).UnixMilli(),
	})
	if err := os.MkdirAll(filepath.Join(store.StoreDir(), ".account-generation"), 0o700); err != nil {
		t.Fatal(err)
	}

	var requests atomic.Int32
	client := &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, fmt.Errorf("credential request ran after publication failure: %s", req.URL)
	})}
	rows, err := (srRunner{store: store, client: client}).fetchUsageRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if row := usageRowFor(rows, accounts.ProviderClaude, profile.Name); row == nil || row.err == nil {
		t.Fatalf("Claude publication failure row = %+v", row)
	}
	if requests.Load() != 0 {
		t.Fatalf("Claude publication failure made %d credential requests, want 0", requests.Load())
	}
	credential, err := claudeStore.ReadCredential(t.Context(), claudeStore.ClaudeConfigDir(profile.Name))
	if err != nil {
		t.Fatal(err)
	}
	if credential == nil || credential.AccessToken != "stale-access" || credential.RefreshToken != "stale-refresh" {
		t.Fatalf("credential changed after publication failure: %+v", credential)
	}
}

func seedClaudeStatusCredential(t *testing.T, store agentclaude.Store, name string, credential agentclaude.CredentialInfo) agentclaude.Profile {
	t.Helper()
	if _, err := store.CreateProfile(name); err != nil {
		t.Fatal(err)
	}
	configDir := store.ClaudeConfigDir(name)
	if err := os.WriteFile(filepath.Join(configDir, ".credentials.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteCredential(t.Context(), configDir, credential); err != nil {
		t.Fatal(err)
	}
	profile, ok := store.FindProfile(name)
	if !ok {
		t.Fatalf("Claude profile %q was not created", name)
	}
	return profile
}

func srJSONResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func usageRowFor(rows []srUsageRow, provider accounts.Provider, accountID string) *srUsageRow {
	for i := range rows {
		if rows[i].provider == provider && rows[i].email == accountID {
			return &rows[i]
		}
	}
	return nil
}

func accountToken(all []baseaccount.Account, provider accounts.Provider, accountID string) string {
	for _, account := range all {
		if account.Provider == provider && account.ID == accountID {
			return account.Token
		}
	}
	return ""
}

func TestLocalKimiStatusSeparatesPartialSourceErrorFromHealthyAccount(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", root)
	runner := srRunner{
		store: accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")},
		kimi:  partialKimiUsageStore{},
	}
	rows, err := runner.fetchUsageRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var sourceErrors, healthy int
	for _, row := range rows {
		switch row.email {
		case "kimi":
			if row.err != nil && row.displayAccount == "credential source" && !row.active {
				sourceErrors++
			}
		case "kimi-subscription:healthy":
			if row.err == nil {
				healthy++
			}
		}
	}
	if sourceErrors != 1 || healthy != 1 {
		t.Fatalf("partial Kimi rows = source-errors:%d healthy:%d", sourceErrors, healthy)
	}
}

func TestKimiListPrintsHealthyProfilesAlongsidePartialWarning(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	store := agentkimi.DefaultStore()
	if _, err := store.SaveManagedCredential("healthy", agentkimi.CredentialInfo{
		AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	brokenName := base64.RawURLEncoding.EncodeToString([]byte("broken")) + ".json"
	brokenPath := filepath.Join(root, "state", "kimi", brokenName)
	if err := os.WriteFile(brokenPath, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	runner := srRunner{store: accounts.CodexStore{Dir: filepath.Join(root, "state", "codex", "accounts")}, out: &out, errOut: &errOut}
	if err := runner.kimiCommand(t.Context(), []string{"list"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "kimi-subscription:healthy") {
		t.Fatal("Kimi list hid the healthy managed profile")
	}
	if !strings.Contains(errOut.String(), "Warning: some Kimi credentials are unavailable") {
		t.Fatal("Kimi list did not surface the partial credential error")
	}
}

func TestKimiListMarksInteractiveCLICredentialNotRouted(t *testing.T) {
	root := t.TempDir()
	kimiHome := filepath.Join(root, "kimi-home")
	t.Setenv("KIMI_CODE_HOME", kimiHome)
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	path := filepath.Join(kimiHome, "credentials", "kimi-code.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"access_token":"cli-access","refresh_token":"cli-refresh","expires_at":4102444800}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runner := srRunner{store: accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}, out: &out}
	if err := runner.kimiCommand(t.Context(), []string{"list"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "kimi-code (Kimi CLI; not routed; Kimi Code)") {
		t.Fatalf("Kimi list did not mark the interactive credential CLI-only:\n%s", out.String())
	}
}

func TestFetchUsageRowsExcludesKimiCLICredentialWithoutRefreshingIt(t *testing.T) {
	root := t.TempDir()
	kimiHome := filepath.Join(root, "kimi-home")
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("KIMI_CODE_HOME", kimiHome)
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	path := filepath.Join(kimiHome, "credentials", "kimi-code.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	before := []byte(`{"access_token":"cli-stale","refresh_token":"cli-refresh","expires_at":1}`)
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	client := &http.Client{Transport: srRoundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("unexpected network request")
	})}
	runner := srRunner{store: accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}, client: client}
	rows, err := runner.fetchUsageRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.provider == accounts.ProviderKimi && row.email == "kimi-code" {
			t.Fatalf("status exposed non-routable interactive Kimi credential: %+v", row)
		}
	}
	if requests != 0 {
		t.Fatalf("status made %d request(s) for the CLI-only credential", requests)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("status changed the interactive Kimi CLI credential bytes")
	}
	var out bytes.Buffer
	printKimiCLIOnlyStatusHint(&out, nil)
	if !strings.Contains(out.String(), "plain 'kimi' login is direct") || !strings.Contains(out.String(), "sr kimi") {
		t.Fatalf("status hint is not actionable: %q", out.String())
	}
	out.Reset()
	printKimiCLIOnlyStatusHint(&out, []srUsageRow{{
		email: "kimi-code", provider: accounts.ProviderKimi, authMode: accounts.AuthModeOAuth,
	}})
	if !strings.Contains(out.String(), "Plain 'kimi' uses the local direct login") || !strings.Contains(out.String(), "sr kimi") || strings.Contains(out.String(), "sr kimi login") {
		t.Fatalf("status did not distinguish direct and managed Kimi launchers: %q", out.String())
	}
	out.Reset()
	printKimiCLIOnlyStatusHint(&out, []srUsageRow{{
		email: "kimi:key", provider: accounts.ProviderKimi, authMode: accounts.AuthModeAPIKey,
	}})
	if !strings.Contains(out.String(), "routed Subrouter Kimi key pool") || !strings.Contains(out.String(), "sr kimi") || strings.Contains(out.String(), "sr kimi login") {
		t.Fatalf("status did not recognize an API-key-only Kimi pool: %q", out.String())
	}
}

func (f fakeKimiUsageStore) ListAccounts(context.Context) ([]baseaccount.Account, error) {
	return f.accounts, f.err
}

func (f fakeKimiUsageStore) FetchUsage(context.Context, *http.Client, baseaccount.Account) (string, []accounts.UsageWindow, error) {
	if f.fetches != nil {
		*f.fetches++
	}
	return f.plan, f.windows, f.err
}

func TestFetchUsageRowsDoesNotSendKimiAPIKeyToUnconfiguredVendorQuota(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	if _, _, err := store.AddAPIKeyForProvider("gateway", "gateway-only-key", accounts.ProviderKimi); err != nil {
		t.Fatal(err)
	}
	fetches := 0
	runner := srRunner{store: store, kimi: fakeKimiUsageStore{fetches: &fetches}}
	rows, err := runner.fetchUsageRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, row := range rows {
		if row.provider == accounts.ProviderKimi && row.authMode == accounts.AuthModeAPIKey {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Kimi API-key row is missing: %+v", rows)
	}
	if fetches != 0 {
		t.Fatalf("standalone status sent Kimi API key to vendor quota endpoint %d time(s)", fetches)
	}
}

func TestAutoImportIfEmptySkipsProviderOnlyOAuthInstallations(t *testing.T) {
	for _, test := range []struct {
		name string
		kimi srKimiUsageStore
		grok srGrokStore
	}{
		{
			name: "Kimi",
			kimi: fakeKimiUsageStore{accounts: []baseaccount.Account{{
				ID: "kimi-subscription:work", Provider: baseaccount.ProviderKimi, AuthMode: baseaccount.AuthModeOAuth,
			}}},
		},
		{
			name: "Grok",
			grok: &fakeGrokStore{account: baseaccount.Account{
				ID: "grok-subscription", Provider: baseaccount.ProviderGrok, AuthMode: baseaccount.AuthModeOAuth,
			}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", filepath.Join(root, "home"))
			t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
			store := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
			var out bytes.Buffer
			runner := srRunner{store: store, out: &out, kimi: test.kimi, grok: test.grok}
			if err := runner.autoImportIfEmpty(t.Context()); err != nil {
				t.Fatal(err)
			}
			if out.Len() != 0 {
				t.Fatalf("provider-only status printed Codex import output: %q", out.String())
			}
			if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); !os.IsNotExist(err) {
				t.Fatalf("provider-only status published a Codex mutation: %v", err)
			}
		})
	}
}

func TestAutoImportIfEmptyDoesNotPublishMissingActiveCodexAuth(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	store := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
	var out bytes.Buffer
	runner := srRunner{
		store: store, out: &out,
		kimi: fakeKimiUsageStore{},
		grok: &fakeGrokStore{},
	}
	if err := runner.autoImportIfEmpty(t.Context()); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("missing active auth printed false import success: %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); !os.IsNotExist(err) {
		t.Fatalf("missing active auth published a mutation: %v", err)
	}
}

func TestAutoImportIfEmptyDoesNotPublishNonOAuthActiveCodexAuth(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	if err := accounts.WriteActiveCodexAuth(accounts.CodexAuthFile{OpenAIAPIKey: "sk-test"}); err != nil {
		t.Fatal(err)
	}
	store := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
	var out bytes.Buffer
	runner := srRunner{
		store: store, out: &out,
		kimi: fakeKimiUsageStore{},
		grok: &fakeGrokStore{},
	}
	if err := runner.autoImportIfEmpty(t.Context()); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("non-OAuth auth printed false import success: %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); !os.IsNotExist(err) {
		t.Fatalf("non-OAuth auth published a mutation: %v", err)
	}
}

func TestImportActiveRejectsMissingAuthBeforePublishingGeneration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	store := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
	var out bytes.Buffer
	runner := srRunner{store: store, out: &out}
	if err := runner.importActive(t.Context()); err == nil || !strings.Contains(err.Error(), "no active Codex OAuth auth") {
		t.Fatalf("import error = %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("failed import printed success: %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); !os.IsNotExist(err) {
		t.Fatalf("failed import published a mutation: %v", err)
	}
}

func TestFetchUsageRowsIncludesLocalKimiSubscription(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUBROUTER_SESSIONS", session.DefaultStorePath())
	windows := []accounts.UsageWindow{
		{Name: "weekly", UsedPercent: 25, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAfterSeconds: 2 * 24 * 60 * 60},
		{Name: "5h", UsedPercent: 40, LimitWindowSeconds: int64((5 * time.Hour) / time.Second), ResetAfterSeconds: 2 * 60 * 60},
	}
	runner := srRunner{
		store:  accounts.CodexStore{Dir: filepath.Join(home, ".codex", "accounts")},
		client: &http.Client{Timeout: time.Second},
		kimi: fakeKimiUsageStore{
			accounts: []baseaccount.Account{{ID: "kimi-code", Provider: baseaccount.ProviderKimi, AuthMode: baseaccount.AuthModeOAuth}},
			plan:     "subscription", windows: windows,
		},
	}
	rows, err := runner.fetchUsageRows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.provider != accounts.ProviderKimi {
			continue
		}
		if row.email != "kimi-code" || row.active || row.planType != "subscription" {
			t.Fatalf("Kimi row metadata = %+v", row)
		}
		if !row.sessionsKnown {
			t.Fatal("Kimi row did not distinguish ready from an assigned session")
		}
		if !slices.Equal(row.windows, windows) {
			t.Fatalf("Kimi windows = %+v, want %+v", row.windows, windows)
		}
		return
	}
	t.Fatal("local Kimi subscription row is missing")
}

func TestFetchUsageRowsKeepsMultipleKimiAccountsDistinct(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	runner := srRunner{
		store:  accounts.CodexStore{Dir: filepath.Join(home, ".codex", "accounts")},
		client: &http.Client{Timeout: time.Second},
		kimi: fakeKimiUsageStore{
			accounts: []baseaccount.Account{
				{ID: "kimi-code:first", Provider: baseaccount.ProviderKimi, AuthMode: baseaccount.AuthModeOAuth},
				{ID: "kimi-code:second", Provider: baseaccount.ProviderKimi, AuthMode: baseaccount.AuthModeOAuth},
			},
			plan: "subscription",
			windows: []accounts.UsageWindow{{
				Name: "5h", UsedPercent: 20,
				LimitWindowSeconds: int64((5 * time.Hour) / time.Second),
			}},
		},
	}
	rows, err := runner.fetchUsageRows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, row := range rows {
		if row.provider == accounts.ProviderKimi {
			got[row.email] = row.planType == "subscription" && len(row.windows) == 1
		}
	}
	for _, accountID := range []string{"kimi-code:first", "kimi-code:second"} {
		if !got[accountID] {
			t.Fatalf("Kimi account %q was lost or incompletely updated: %v", accountID, got)
		}
	}
}

func TestFetchUsageRowsIncludesAuthOnlyGrokSubscriptionActivity(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("SUBROUTER_SESSIONS", session.DefaultStorePath())
	grokStore := &fakeGrokStore{account: baseaccount.Account{
		ID: "grok-subscription", Provider: baseaccount.ProviderGrok,
		AuthMode: baseaccount.AuthModeOAuth, Token: "access", Email: "person@example.com",
	}}
	sessions, err := session.NewStore(session.DefaultStorePath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Put("grok", "conversation", "grok-subscription", ""); err != nil {
		t.Fatal(err)
	}
	runner := srRunner{
		store: accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")},
		grok:  grokStore,
	}
	rows, err := runner.fetchUsageRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.provider != accounts.ProviderGrok || row.authMode != accounts.AuthModeOAuth {
			continue
		}
		if row.email != "grok-subscription" || row.displayAccount != "person@example.com" || row.planType != "subscription" {
			t.Fatalf("Grok row metadata = %+v", row)
		}
		if row.providerHealth != "stored" || row.err != nil || len(row.windows) != 0 {
			t.Fatalf("Grok row should expose only stored auth without invented validation or quota: %+v", row)
		}
		if !row.sessionsKnown || row.assignedSessions != 1 || !row.active {
			t.Fatalf("Grok activity = %+v", row)
		}
		rankUsageRows(rows)
		if row.gtoRecommended || displayRecommendedForNewSession(row) {
			t.Fatalf("unverified Grok credential was recommended: %+v", row)
		}
		if state := usageGridState(row); state != "active" {
			t.Fatalf("Grok state = %q, want only observed active session", state)
		}
		return
	}
	t.Fatal("local Grok subscription row is missing")
}

func TestUnverifiedGrokStatusIsStoredAndNeverRecommended(t *testing.T) {
	rows := []srUsageRow{{
		email: "grok-subscription", provider: accounts.ProviderGrok,
		authMode: accounts.AuthModeOAuth, providerHealth: "stored",
		score: selectacct.Score{AccountID: "grok-subscription", Headroom: 1, ShortHeadroom: 1},
	}}
	rankUsageRows(rows)
	if rows[0].gtoRecommended || displayRecommendedForNewSession(rows[0]) {
		t.Fatalf("unverified Grok credential was recommended: %+v", rows[0])
	}
	if state := usageGridState(rows[0]); state != "stored" {
		t.Fatalf("Grok state = %q, want stored", state)
	}
	if color := usageGridStateColor(rows[0]); color != ansiDim {
		t.Fatalf("Grok stored state color = %q, want informational dim", color)
	}
}

func TestFetchUsageRowsUsesOnlyConfiguredSessionStore(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	configuredPath := filepath.Join(root, "daemon", "assignments.json")
	t.Setenv("SUBROUTER_SESSIONS", configuredPath)
	configured, err := session.NewStore(configuredPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := configured.Put("kimi", "configured-session", "kimi-code", ""); err != nil {
		t.Fatal(err)
	}
	defaultStore, err := session.NewStore(session.DefaultStorePath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := defaultStore.Put("kimi", "wrong-session", "someone-else", ""); err != nil {
		t.Fatal(err)
	}
	runner := srRunner{
		store: accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")},
		kimi: fakeKimiUsageStore{accounts: []baseaccount.Account{{
			ID: "kimi-code", Provider: baseaccount.ProviderKimi, AuthMode: baseaccount.AuthModeOAuth,
		}}, plan: "subscription"},
	}
	rows, err := runner.fetchUsageRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.provider == accounts.ProviderKimi {
			if !row.sessionsKnown || row.assignedSessions != 1 || !row.active {
				t.Fatalf("Kimi session activity = %+v", row)
			}
			return
		}
	}
	t.Fatal("local Kimi subscription row is missing")
}

func TestFetchUsageRowsLeavesSessionsUnknownWithoutConfiguredStore(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	runner := srRunner{
		store: accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")},
		kimi: fakeKimiUsageStore{accounts: []baseaccount.Account{{
			ID: "kimi-code", Provider: baseaccount.ProviderKimi, AuthMode: baseaccount.AuthModeOAuth,
		}}, plan: "subscription"},
	}
	rows, err := runner.fetchUsageRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.provider == accounts.ProviderKimi {
			if row.sessionsKnown || row.active {
				t.Fatalf("Kimi session activity was inferred without a configured store: %+v", row)
			}
			return
		}
	}
	t.Fatal("local Kimi subscription row is missing")
}

func TestSRListReadsNativeCodexStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "b@example.com",
		AddedAt: "2026-04-28T00:00:00Z",
		Auth: accounts.CodexAuthFile{Tokens: &accounts.CodexTokens{
			AccessToken: "access",
			IDToken:     "id",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "apikey:paid",
		AddedAt: "2026-04-28T00:00:00Z",
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-test",
		},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"list"}); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if !strings.Contains(got, "b@example.com") {
		t.Fatalf("list output missing OAuth account:\n%s", got)
	}
	if !strings.Contains(got, "paid (api key)") {
		t.Fatalf("list output missing API-key account:\n%s", got)
	}
}

func TestSRAddKeyStoresRegistryProviderInLocalStorage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader("work\nsk-or-v1-test\n"),
		out:    &out,
		errOut: &out,
	}

	if err := runner.run(context.Background(), []string{"add-key", "--provider", "open-router"}); err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.FindStored("openrouter:work")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing provider-scoped OpenRouter account")
	}
	if stored.Provider != accounts.ProviderOpenRouter || stored.Auth.AuthMode != "apikey" || stored.Auth.OpenAIAPIKey != "sk-or-v1-test" {
		t.Fatalf("stored account provider=%q auth_mode=%q key_matches=%t, want OpenRouter API-key account", stored.Provider, stored.Auth.AuthMode, stored.Auth.OpenAIAPIKey == "sk-or-v1-test")
	}
	if _, codex, err := store.FindStored("apikey:work"); err != nil {
		t.Fatal(err)
	} else if codex {
		t.Fatal("OpenRouter key was also stored in the Codex API-key pool")
	}
}

func TestSelectedLoopbackServingAPIWinsOverUnattestedLocalDisk(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "cli-state")}
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email: "openrouter:disk-only", Provider: accounts.ProviderOpenRouter, AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "disk-placeholder"},
	}); err != nil {
		t.Fatal(err)
	}
	var imported atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/_subrouter/health":
			authorityID, err := accounts.StoreAuthorityID(store.Dir)
			if err != nil {
				t.Fatal(err)
			}
			proof := ""
			if challenge := request.Header.Get(accounts.StoreAuthorityChallengeHeader); challenge != "" {
				proof, err = accounts.StoreAuthorityProof(store.Dir, challenge)
				if err != nil {
					t.Fatal(err)
				}
			}
			_, _ = fmt.Fprintf(w, `{"ok":true,"account_import":"enabled","account_store_id":%q,"account_store_proof":%q}`, authorityID, proof)
		case "/_subrouter/account-import":
			if request.Header.Get("Authorization") != "Bearer import-token" {
				http.Error(w, "missing import credential", http.StatusUnauthorized)
				return
			}
			if request.Method == http.MethodGet {
				_, _ = io.WriteString(w, `{"ok":true,"providers":["openrouter"]}`)
				return
			}
			var input serverAccountImportRequest
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if input.Provider != accounts.ProviderOpenRouter || input.Codex == nil || input.Codex.Email != "openrouter:network" {
				http.Error(w, "wrong import payload", http.StatusBadRequest)
				return
			}
			imported.Store(true)
			_, _ = io.WriteString(w, `{"ok":true}`)
		case "/_subrouter/usage-status":
			_, _ = io.WriteString(w, `[{
				"id":"openrouter:network","provider":"openrouter","auth_mode":"apikey",
				"plan_type":"credits, per token","provider_health":"auth ok",
				"auth_checked":true,"auth_valid":true,"quota_status":"live","quota_usage_known":true,
				"windows":[{"Name":"monthly","UsedPercent":25,"LimitWindowSeconds":2592000}],
				"credits":{"has_credits":true,"balance":"150"}
			}]`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	attachPrivateLocalTestListener(t, server, store)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL)
	cloudPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	if err := os.WriteFile(cloudPath, []byte(`{"version":1,"baseUrl":"https://cmux.com","credentialSource":"local"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	serverStore := defaultSRServerStore(store)
	if err := serverStore.update(func(file *srServerFile) error {
		file.Default = "local-candidate"
		file.Servers = []srServerConfig{{Name: "local-candidate", URL: server.URL, AccountImportToken: "import-token"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{store: store, useServingAPI: true, in: strings.NewReader("network\napi-key-placeholder\n"), out: &out, errOut: &out, client: server.Client()}
	if err := runner.run(t.Context(), []string{"add-key", "--provider", "openrouter"}); err != nil {
		t.Fatal(err)
	}
	if !imported.Load() {
		t.Fatal("selected serving API did not receive the account import")
	}
	if _, ok, err := store.FindStored("openrouter:network"); err != nil || ok {
		t.Fatalf("CLI disk store received serving account: found=%t err=%v", ok, err)
	}
	out.Reset()
	if err := runner.run(t.Context(), []string{"status"}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"network", "credits", "75% left", "$150"} {
		if !strings.Contains(text, want) {
			t.Fatalf("server-authoritative status omits %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "disk-only") || strings.Contains(text, "Codex isolation:") {
		t.Fatalf("server-authoritative status leaked local disk state:\n%s", text)
	}
	out.Reset()
	if err := runner.run(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if text := out.String(); !strings.Contains(text, "network") || strings.Contains(text, "disk-only") {
		t.Fatalf("bare sr did not use the serving authority:\n%s", text)
	}
}

func TestFreshLocalServingDaemonKeepsOnboardingOnLocalCommandPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{
		"SUBROUTER_ACCOUNT_IMPORT_TOKEN", "SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE",
		"SUBROUTER_ADMIN_TOKEN", "SUBROUTER_ADMIN_TOKEN_FILE",
	} {
		t.Setenv(name, "")
	}
	store := accounts.CodexStore{Dir: filepath.Join(home, ".subrouter", "codex", "accounts")}
	var nonHealthRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/_subrouter/health" {
			authorityID, err := accounts.StoreAuthorityID(store.Dir)
			if err != nil {
				t.Fatal(err)
			}
			proof := ""
			if challenge := request.Header.Get(accounts.StoreAuthorityChallengeHeader); challenge != "" {
				proof, err = accounts.StoreAuthorityProof(store.Dir, challenge)
				if err != nil {
					t.Fatal(err)
				}
			}
			_, _ = fmt.Fprintf(w, `{"ok":true,"account_import":"disabled","account_store_id":%q,"account_store_proof":%q}`, authorityID, proof)
			return
		}
		nonHealthRequests.Add(1)
		http.Error(w, "protected account import credential required", http.StatusUnauthorized)
	}))
	defer server.Close()
	attachPrivateLocalTestListener(t, server, store)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL)
	cloudPath := filepath.Join(home, "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	if err := os.WriteFile(cloudPath, []byte(`{"version":1,"baseUrl":"https://cmux.com","credentialSource":"local"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runner := srRunner{
		store: store, useServingAPI: true,
		in: strings.NewReader("work\nsk-or-v1-test\n"), out: &out, errOut: &out,
		client: server.Client(),
	}
	if err := runner.run(t.Context(), []string{"add-key", "--provider", "openrouter"}); err != nil {
		t.Fatal(err)
	}
	if nonHealthRequests.Load() != 0 {
		t.Fatalf("fresh local onboarding sent %d request(s) to an uncredentialed serving API", nonHealthRequests.Load())
	}
	stored, ok, err := store.FindStored("openrouter:work")
	if err != nil || !ok {
		t.Fatalf("local onboarding account found=%t err=%v", ok, err)
	}
	if stored.Provider != accounts.ProviderOpenRouter || stored.Auth.OpenAIAPIKey != "sk-or-v1-test" {
		t.Fatalf("local onboarding stored provider=%q key_matches=%t", stored.Provider, stored.Auth.OpenAIAPIKey == "sk-or-v1-test")
	}
	out.Reset()
	if err := runner.run(t.Context(), []string{"kimi", "list"}); err != nil {
		t.Fatal(err)
	}
	if nonHealthRequests.Load() != 0 || !strings.Contains(out.String(), "No Kimi subscription accounts configured") {
		t.Fatalf("fresh local Kimi management contacted serving API or missed local state: requests=%d output=%q", nonHealthRequests.Load(), out.String())
	}
}

func TestFreshLocalServingDaemonRejectsUnattestedOnboardingStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{
		"SUBROUTER_ACCOUNT_IMPORT_TOKEN", "SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE",
		"SUBROUTER_ADMIN_TOKEN", "SUBROUTER_ADMIN_TOKEN_FILE",
	} {
		t.Setenv(name, "")
	}
	store := accounts.CodexStore{Dir: filepath.Join(home, ".subrouter", "codex", "accounts")}
	otherStore := accounts.CodexStore{Dir: filepath.Join(home, "daemon-state", "codex", "accounts")}
	otherAuthority, err := accounts.StoreAuthorityID(otherStore.Dir)
	if err != nil {
		t.Fatal(err)
	}
	var nonHealthRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/_subrouter/health" {
			proof := ""
			if challenge := request.Header.Get(accounts.StoreAuthorityChallengeHeader); challenge != "" {
				var proofErr error
				proof, proofErr = accounts.StoreAuthorityProof(otherStore.Dir, challenge)
				if proofErr != nil {
					t.Fatal(proofErr)
				}
			}
			_, _ = fmt.Fprintf(w, `{"ok":true,"account_import":"enabled","account_store_id":%q,"account_store_proof":%q}`, otherAuthority, proof)
			return
		}
		nonHealthRequests.Add(1)
		http.Error(w, "protected account import credential required", http.StatusUnauthorized)
	}))
	defer server.Close()
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL)
	cloudPath := filepath.Join(home, "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	if err := os.WriteFile(cloudPath, []byte(`{"version":1,"baseUrl":"https://cmux.com","credentialSource":"local"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := defaultSRServerStore(store).update(func(file *srServerFile) error {
		file.Servers = []srServerConfig{{Name: "unattested-loopback", URL: server.URL, AccountImportToken: "must-not-be-sent"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store: store, useServingAPI: true,
		in: strings.NewReader("work\nsk-or-v1-test\n"), out: &out, errOut: &out,
		client: server.Client(),
	}
	err = runner.run(t.Context(), []string{"add-key", "--provider", "openrouter"})
	if err == nil || !strings.Contains(err.Error(), "local proxy account store does not match this CLI") {
		t.Fatalf("unattested onboarding error = %v", err)
	}
	if nonHealthRequests.Load() != 0 {
		t.Fatalf("unattested loopback received %d credential-bearing request(s)", nonHealthRequests.Load())
	}
	if _, ok, findErr := store.FindStored("openrouter:work"); findErr != nil || ok {
		t.Fatalf("unattested onboarding mutated CLI store: found=%t err=%v", ok, findErr)
	}
}

func TestLegacyLocalServingDaemonRejectsUnattestedOnboardingBeforeCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.CodexStore{Dir: filepath.Join(home, ".subrouter", "codex", "accounts")}
	otherStore := accounts.CodexStore{Dir: filepath.Join(home, "rogue-state", "codex", "accounts")}
	otherAuthority, err := accounts.StoreAuthorityID(otherStore.Dir)
	if err != nil {
		t.Fatal(err)
	}
	var nonHealthRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/_subrouter/health" {
			nonHealthRequests.Add(1)
			http.Error(w, "credential sink", http.StatusUnauthorized)
			return
		}
		proof := ""
		if challenge := request.Header.Get(accounts.StoreAuthorityChallengeHeader); challenge != "" {
			var proofErr error
			proof, proofErr = accounts.StoreAuthorityProof(otherStore.Dir, challenge)
			if proofErr != nil {
				t.Fatal(proofErr)
			}
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"account_import":"enabled","account_store_id":%q,"account_store_proof":%q}`, otherAuthority, proof)
	}))
	defer server.Close()
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL)
	cloudPath := filepath.Join(home, "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	if err := os.WriteFile(cloudPath, []byte(`{"version":1,"baseUrl":"https://cmux.com","credentialSource":"legacy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := defaultSRServerStore(store).update(func(file *srServerFile) error {
		// No selected remote: this entry exists only to prove a matching legacy
		// credential is not sent before the private local channel is established.
		file.Servers = []srServerConfig{{Name: "unselected-local", URL: server.URL, AdminToken: "must-not-be-sent"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	runner := srRunner{
		store: store, useServingAPI: true,
		in: strings.NewReader("work\nsk-or-v1-must-not-be-sent\n"), out: io.Discard, errOut: io.Discard,
		client: server.Client(),
	}
	err = runner.run(t.Context(), []string{"add-key", "--provider", "openrouter"})
	if err == nil || !strings.Contains(err.Error(), "local proxy account store does not match this CLI") {
		t.Fatalf("unattested legacy-local onboarding error = %v", err)
	}
	if nonHealthRequests.Load() != 0 {
		t.Fatalf("unattested legacy-local listener received %d credential-bearing request(s)", nonHealthRequests.Load())
	}
	if _, ok, findErr := store.FindStored("openrouter:work"); findErr != nil || ok {
		t.Fatalf("unattested legacy-local onboarding mutated CLI store: found=%t err=%v", ok, findErr)
	}
}

func TestLegacyLocalServingDaemonKeepsUnprotectedOnboardingOnLocalStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{
		"SUBROUTER_ACCOUNT_IMPORT_TOKEN", "SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE",
		"SUBROUTER_ADMIN_TOKEN", "SUBROUTER_ADMIN_TOKEN_FILE",
	} {
		t.Setenv(name, "")
	}
	store := accounts.CodexStore{Dir: filepath.Join(home, ".subrouter", "codex", "accounts")}
	var nonHealthRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/_subrouter/health" {
			nonHealthRequests.Add(1)
			http.Error(w, "account import is disabled", http.StatusUnauthorized)
			return
		}
		authorityID, err := accounts.StoreAuthorityID(store.Dir)
		if err != nil {
			t.Fatal(err)
		}
		proof := ""
		if challenge := request.Header.Get(accounts.StoreAuthorityChallengeHeader); challenge != "" {
			proof, err = accounts.StoreAuthorityProof(store.Dir, challenge)
			if err != nil {
				t.Fatal(err)
			}
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"account_import":"disabled","account_store_id":%q,"account_store_proof":%q}`, authorityID, proof)
	}))
	defer server.Close()
	attachPrivateLocalTestListener(t, server, store)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL)
	cloudPath := filepath.Join(home, "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	if err := os.WriteFile(cloudPath, []byte(`{"version":1,"baseUrl":"https://cmux.com","credentialSource":"legacy"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := srRunner{
		store: store, useServingAPI: true,
		in: strings.NewReader("work\nsk-or-v1-local-only\n"), out: io.Discard, errOut: io.Discard,
		client: server.Client(),
	}
	if err := runner.run(t.Context(), []string{"add-key", "--provider", "openrouter"}); err != nil {
		t.Fatal(err)
	}
	if nonHealthRequests.Load() != 0 {
		t.Fatalf("unprotected legacy-local onboarding sent %d HTTP mutation request(s)", nonHealthRequests.Load())
	}
	stored, ok, err := store.FindStored("openrouter:work")
	if err != nil || !ok {
		t.Fatalf("legacy-local onboarding account found=%t err=%v", ok, err)
	}
	if stored.Provider != accounts.ProviderOpenRouter || stored.Auth.OpenAIAPIKey != "sk-or-v1-local-only" {
		t.Fatalf("legacy-local onboarding stored provider=%q key_matches=%t", stored.Provider, stored.Auth.OpenAIAPIKey == "sk-or-v1-local-only")
	}
}

func TestProtectedLocalServingDaemonKeepsOnboardingOnHTTPAuthority(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{
		"SUBROUTER_ACCOUNT_IMPORT_TOKEN", "SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE",
		"SUBROUTER_ADMIN_TOKEN", "SUBROUTER_ADMIN_TOKEN_FILE",
	} {
		t.Setenv(name, "")
	}
	store := accounts.CodexStore{Dir: filepath.Join(home, ".subrouter", "codex", "accounts")}
	authorityID, err := accounts.StoreAuthorityID(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	var importRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/_subrouter/health":
			proof := ""
			if challenge := request.Header.Get(accounts.StoreAuthorityChallengeHeader); challenge != "" {
				var proofErr error
				proof, proofErr = accounts.StoreAuthorityProof(store.Dir, challenge)
				if proofErr != nil {
					t.Fatal(proofErr)
				}
			}
			_, _ = fmt.Fprintf(w, `{"ok":true,"account_import":"enabled","account_store_id":%q,"account_store_proof":%q}`, authorityID, proof)
		case serverAccountImportPath:
			importRequests.Add(1)
			http.Error(w, "protected account import credential required", http.StatusUnauthorized)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	attachPrivateLocalTestListener(t, server, store)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL)
	cloudPath := filepath.Join(home, "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	if err := os.WriteFile(cloudPath, []byte(`{"version":1,"baseUrl":"https://cmux.com","credentialSource":"local"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store: store, useServingAPI: true,
		in: strings.NewReader("work\nsk-or-v1-test\n"), out: &out, errOut: &out,
		client: server.Client(),
	}
	err = runner.run(t.Context(), []string{"add-key", "--provider", "openrouter"})
	if err == nil || !strings.Contains(err.Error(), "no protected HTTP account-import credential") {
		t.Fatalf("protected local onboarding error = %v", err)
	}
	if importRequests.Load() == 0 {
		t.Fatal("protected local onboarding bypassed the daemon HTTP authority")
	}
	if _, ok, findErr := store.FindStored("openrouter:work"); findErr != nil || ok {
		t.Fatalf("protected local onboarding mutated disk directly: found=%t err=%v", ok, findErr)
	}
}

func TestLocalServingAPIIgnoresMalformedOptionalServerRegistry(t *testing.T) {
	var usageRequests atomic.Int32
	var accountRequests atomic.Int32
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "cli-state")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/_subrouter/health":
			authorityID, err := accounts.StoreAuthorityID(store.Dir)
			if err != nil {
				t.Fatal(err)
			}
			proof := ""
			if challenge := request.Header.Get(accounts.StoreAuthorityChallengeHeader); challenge != "" {
				proof, err = accounts.StoreAuthorityProof(store.Dir, challenge)
				if err != nil {
					t.Fatal(err)
				}
			}
			_, _ = fmt.Fprintf(w, `{"ok":true,"account_store_id":%q,"account_store_proof":%q}`, authorityID, proof)
		case "/_subrouter/usage-status":
			usageRequests.Add(1)
			_, _ = io.WriteString(w, `[]`)
		case "/_subrouter/accounts":
			accountRequests.Add(1)
			_, _ = io.WriteString(w, `[]`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	attachPrivateLocalTestListener(t, server, store)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL)
	cloudPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	if err := os.WriteFile(cloudPath, []byte(`{"version":1,"baseUrl":"https://cmux.com","credentialSource":"local"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(store.StoreDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(defaultSRServerStore(store).Path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := srRunner{store: store, useServingAPI: true, out: io.Discard, errOut: io.Discard, client: server.Client()}
	if err := runner.run(t.Context(), []string{"status"}); err != nil {
		t.Fatalf("status with malformed optional server registry: %v", err)
	}
	if err := runner.run(t.Context(), []string{"list"}); err != nil {
		t.Fatalf("list with malformed optional server registry: %v", err)
	}
	if usageRequests.Load() == 0 || accountRequests.Load() == 0 {
		t.Fatalf("local serving API was not used: usage=%d accounts=%d", usageRequests.Load(), accountRequests.Load())
	}
}

func TestReadyLocalServingServerStartsColdDaemon(t *testing.T) {
	var healthy atomic.Bool
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "cli-state")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/_subrouter/health" {
			http.NotFound(w, request)
			return
		}
		if !healthy.Load() {
			http.Error(w, "cold", http.StatusServiceUnavailable)
			return
		}
		authorityID, err := accounts.StoreAuthorityID(store.Dir)
		if err != nil {
			t.Fatal(err)
		}
		proof := ""
		if challenge := request.Header.Get(accounts.StoreAuthorityChallengeHeader); challenge != "" {
			proof, err = accounts.StoreAuthorityProof(store.Dir, challenge)
			if err != nil {
				t.Fatal(err)
			}
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"account_store_id":%q,"account_store_proof":%q}`, authorityID, proof)
	}))
	defer server.Close()
	attachPrivateLocalTestListener(t, server, store)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL)

	var starts atomic.Int32
	runner := srRunner{store: store, errOut: io.Discard, client: server.Client()}
	resolved, err := runner.readyLocalServingServer(t.Context(), func() error {
		starts.Add(1)
		healthy.Store(true)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 || !sameEndpoint(resolved.URL, server.URL) {
		t.Fatalf("ready local server = %+v starts=%d", resolved, starts.Load())
	}
}

func TestLocalServingCommandsRejectUnattestedListenerBeforeAdminCredential(t *testing.T) {
	for _, args := range [][]string{nil, {"list"}, {"status"}, {"reset"}} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("SUBROUTER_ADMIN_TOKEN", "must-not-be-sent")
			t.Setenv("SUBROUTER_ACCOUNT_IMPORT_TOKEN", "must-not-be-sent")
			t.Setenv("SUBROUTER_STATE_DIR", "")
			cloudPath := filepath.Join(home, "cloud.json")
			t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
			if err := os.WriteFile(cloudPath, []byte(`{"version":1,"credentialSource":"local"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			var authenticatedRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Authorization") != "" || request.Header.Get("X-Subrouter-Account-Import-Token") != "" {
					authenticatedRequests.Add(1)
				}
				if request.URL.Path == "/_subrouter/health" {
					_, _ = io.WriteString(w, `{"ok":true,"account_store_id":"spoof","account_store_proof":"spoof"}`)
					return
				}
				http.Error(w, "unexpected authenticated request", http.StatusForbidden)
			}))
			defer server.Close()
			t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL)

			runner := srRunner{
				store:         accounts.CodexStore{Dir: filepath.Join(home, ".subrouter", "codex", "accounts")},
				useServingAPI: true, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard,
				client: server.Client(),
			}
			err := runner.run(t.Context(), args)
			if err == nil || !strings.Contains(err.Error(), "local proxy account store does not match this CLI") {
				t.Fatalf("unattested local command %q error = %v", args, err)
			}
			if authenticatedRequests.Load() != 0 {
				t.Fatalf("unattested local command %q sent %d credentialed request(s)", args, authenticatedRequests.Load())
			}
		})
	}
}

func TestServingAPIRemoteResetKeepsResolvedLoopbackServer(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "cli-state")}
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email: "apikey:disk-only", AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "disk-placeholder"},
	}); err != nil {
		t.Fatal(err)
	}
	var requested atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/_subrouter/health":
			authorityID, err := accounts.StoreAuthorityID(store.Dir)
			if err != nil {
				t.Fatal(err)
			}
			proof := ""
			if challenge := request.Header.Get(accounts.StoreAuthorityChallengeHeader); challenge != "" {
				proof, err = accounts.StoreAuthorityProof(store.Dir, challenge)
				if err != nil {
					t.Fatal(err)
				}
			}
			_, _ = fmt.Fprintf(w, `{"ok":true,"account_store_id":%q,"account_store_proof":%q}`, authorityID, proof)
		case "/_subrouter/reset-credits":
			requested.Store(true)
			_, _ = io.WriteString(w, `{"accounts":[{"email":"server-authority","count":1,"credits":[{"status":"available"}]}]}`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	attachPrivateLocalTestListener(t, server, store)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL)
	cloudPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	if err := os.WriteFile(cloudPath, []byte(`{"version":1,"baseUrl":"https://cmux.com","credentialSource":"local"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{store: store, useServingAPI: true, out: &out, errOut: &out, client: server.Client()}
	if err := runner.run(t.Context(), []string{"reset", "--list"}); err != nil {
		t.Fatal(err)
	}
	if !requested.Load() || !strings.Contains(out.String(), "server-authority") {
		t.Fatalf("reset did not stay on the resolved serving API: requested=%t output=%q", requested.Load(), out.String())
	}
}

func TestServingAPIAccountCommandDoesNotCaptureLocalOnlyCommands(t *testing.T) {
	for _, command := range []string{"add", "add-key", "list", "status", "reset", "qwen", "kimi", "remove"} {
		if !servingAPIAccountCommand(command) {
			t.Fatalf("serving account command %q was not routed", command)
		}
	}
	for _, command := range []string{"switch", "use", "g", "gui", "gui-switch", "gui-use", "pick", "import", "usage", "trace", "breadcrumbs", "why", "add-admin-key", "list-admin-keys", "remove-admin-key", "attach-project"} {
		if servingAPIAccountCommand(command) {
			t.Fatalf("local-only command %q was captured by the serving API", command)
		}
	}
}

func TestLocalOnboardingCommandIncludesFreshDaemonManagement(t *testing.T) {
	for _, args := range [][]string{
		{"add", "codex"},
		{"add-key"},
		{"add-api-key"},
		{"kimi", "login", "work"},
		{"kimi", "list"},
		{"kimi", "remove", "work"},
		{"qwen", "login", "work"},
		{"qwen", "label", "work", "alice@example.com"},
	} {
		if !localOnboardingCommand(args) {
			t.Fatalf("local onboarding command %q was not retained", strings.Join(args, " "))
		}
	}
	for _, args := range [][]string{{"status"}, {"reset"}, {"qwen", "--account", "work"}, {"kimi", "--account", "work"}} {
		if localOnboardingCommand(args) {
			t.Fatalf("serving command %q was captured as local onboarding", strings.Join(args, " "))
		}
	}
}

func TestSRAddKeyForAnotherProviderDoesNotImportActiveCodexAuth(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	store := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
	storedAuth := testCodexAuth("isolated@example.com", "stored")
	storedAuth.LastRefresh = "2026-08-28T00:00:00Z"
	stored := accounts.StoredCodexAccount{
		Email:   "isolated@example.com",
		AddedAt: "2026-08-28T00:00:00Z",
		Auth:    storedAuth,
	}
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(stored.SourcePath(store))
	if err != nil {
		t.Fatal(err)
	}
	activeAuth := testCodexAuth("isolated@example.com", "active")
	activeAuth.LastRefresh = "2026-08-29T00:00:00Z"
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store: store, in: strings.NewReader("work\nsk-or-v1-test\n"),
		out: &out, errOut: &out,
	}
	if err := runner.run(t.Context(), []string{"add-key", "--provider", "openrouter"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(stored.SourcePath(store))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("unrelated provider add rewrote the isolated Codex credential")
	}
	persisted, ok, err := store.FindStored(stored.Email)
	if err != nil || !ok {
		t.Fatalf("stored Codex account found=%t err=%v", ok, err)
	}
	if persisted.Auth.Tokens.RefreshToken != storedAuth.Tokens.RefreshToken {
		t.Fatal("unrelated provider add replaced the isolated Codex token generation")
	}
	if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); err != nil {
		t.Fatalf("provider add did not publish its account generation: %v", err)
	}
}

func TestSRAddKeyRejectedInputDoesNotMutateCodexOrPublishGeneration(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		args  []string
	}{
		{name: "empty label", input: "\n", args: []string{"add-key", "--provider", "openrouter"}},
		{name: "empty key", input: "work\n\n", args: []string{"add-key", "--provider", "openrouter"}},
		{name: "invalid Codex key", input: "work\nnot-an-api-key\n", args: []string{"add-key"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", filepath.Join(root, "home"))
			store := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
			stored := accounts.StoredCodexAccount{
				Email:   "isolated@example.com",
				AddedAt: "2026-08-28T00:00:00Z",
				Auth:    testCodexAuth("isolated@example.com", "stored"),
			}
			if err := store.SaveStored(stored); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(stored.SourcePath(store))
			if err != nil {
				t.Fatal(err)
			}
			activeAuth := testCodexAuth("isolated@example.com", "active")
			activeAuth.LastRefresh = "2026-08-29T00:00:00Z"
			if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			runner := srRunner{store: store, in: strings.NewReader(test.input), out: &out, errOut: &out}
			if err := runner.run(t.Context(), test.args); err == nil {
				t.Fatal("rejected add-key input unexpectedly succeeded")
			}
			after, err := os.ReadFile(stored.SourcePath(store))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Fatal("rejected add-key input rewrote the isolated Codex credential")
			}
			if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); !os.IsNotExist(err) {
				t.Fatalf("rejected add-key input published an account generation: %v", err)
			}
		})
	}
}

func TestSRAddKeyStoresSharedSubscriptionUnderCredentialOwner(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader("work\ntest-provider-key\n"),
		out:    &out,
		errOut: &out,
	}

	if err := runner.run(context.Background(), []string{"add-key", "--provider", "qwen-anthropic"}); err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.FindStored("qwen-token:work")
	if err != nil || !ok {
		t.Fatalf("shared credential owner found=%t err=%v", ok, err)
	}
	if stored.Provider != accounts.ProviderQwenToken {
		t.Fatalf("stored provider = %q, want qwen-token", stored.Provider)
	}
	if _, duplicate, err := store.FindStored("qwen-anthropic:work"); err != nil || duplicate {
		t.Fatalf("protocol-specific duplicate exists=%t err=%v", duplicate, err)
	}
}

func TestSRAddKeyRejectsUnknownLocalProviderBeforePrompting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}

	err := runner.run(context.Background(), []string{"add-key", "--provider", "not/a/provider"})
	if err == nil || !strings.Contains(err.Error(), "unsupported API-key provider") {
		t.Fatalf("error = %v, want unsupported provider", err)
	}
	stored, listErr := store.ListStored()
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(stored) != 0 {
		t.Fatalf("invalid provider stored accounts: %+v", stored)
	}
}

func TestHostedAddKeyRejectsRegistryProviderInsteadOfReclassifyingIt(t *testing.T) {
	var out bytes.Buffer
	runner := srRunner{in: strings.NewReader(""), out: &out, errOut: &out}

	handled, err := runner.runTeamCredentialCommand(context.Background(), []string{"add-key", "--provider", "openrouter"})
	if !handled {
		t.Fatal("hosted add-key command was not handled")
	}
	if err == nil || !strings.Contains(err.Error(), "hosted credential storage does not support openrouter API keys") {
		t.Fatalf("error = %v, want explicit hosted OpenRouter rejection", err)
	}
}

func TestHostedAntigravityManagementFailsClosed(t *testing.T) {
	for _, command := range []string{"agy", "antigravity"} {
		handled, err := (srRunner{}).runTeamCredentialCommand(context.Background(), []string{command, "add", "work"})
		if !handled || err == nil || !strings.Contains(err.Error(), "hosted Antigravity profile management is not available") {
			t.Fatalf("%s handled=%v err=%v", command, handled, err)
		}
	}
}

func TestUsageRowsKeepProviderAPIKeysOutOfCodexSwitching(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	if _, _, err := store.AddProviderAPIKey(accounts.ProviderOpenRouter, "work", "sk-or-v1-test"); err != nil {
		t.Fatal(err)
	}
	runner := srRunner{store: store, client: http.DefaultClient}

	rows, err := runner.fetchUsageRows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].provider != accounts.ProviderOpenRouter {
		t.Fatalf("usage rows = %+v, want one OpenRouter row", rows)
	}
	if err := ensureUsageRowSwitchable(rows[0]); err == nil {
		t.Fatal("OpenRouter API key was switchable as an active Codex credential")
	}
}

func TestSRQwenKeyAccountsCanBeAddedListedAndRemoved(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.DefaultCodexStore()
	for _, input := range []string{
		"large-plan\nsk-sp-large-test\n",
		"small-plan\nsk-sp-small-test\n",
	} {
		var out bytes.Buffer
		runner := srRunner{store: store, in: strings.NewReader(input), out: &out, errOut: &out}
		if err := runner.run(context.Background(), []string{"add-key", "--provider", "qwen-token"}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "(sk-...)") {
			t.Fatalf("Qwen prompt claimed a Codex-specific key format: %s", out.String())
		}
	}
	stored, err := store.ListStored()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("stored Qwen accounts = %+v", stored)
	}
	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"list"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"qwen-token:large-plan", "qwen-token:small-plan"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("list is missing %q:\n%s", want, out.String())
		}
	}
	root := agentqwen.ConsoleRootForStore(store)
	for _, accountID := range []string{"qwen-token:large-plan", "qwen-token:small-plan"} {
		if err := agentqwen.SaveConsoleCredentialIn(root, accountID, agentqwen.ConsoleCredential{AccessToken: "console-secret"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := runner.run(context.Background(), []string{"remove", "qwen-token:small-plan"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); err != nil {
		t.Fatalf("Qwen removal did not publish account generation: %v", err)
	}
	if _, ok, err := store.FindStored("qwen-token:small-plan"); err != nil || ok {
		t.Fatalf("removed Qwen account remains: ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.FindStored("qwen-token:large-plan"); err != nil || !ok {
		t.Fatalf("unrelated Qwen account was removed: ok=%v err=%v", ok, err)
	}
	if _, err := agentqwen.ExportConsoleCredentialIn(root, "qwen-token:small-plan"); err == nil {
		t.Fatal("removed Qwen account retained its console credential")
	}
	if _, err := agentqwen.ExportConsoleCredentialIn(root, "qwen-token:large-plan"); err != nil {
		t.Fatalf("unrelated Qwen console credential was removed: %v", err)
	}
}

func TestSRQwenRemovalKeepsAccountWhenConsoleCredentialCannotBeRemoved(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	account, _, err := store.AddProviderAPIKey(accounts.ProviderQwenToken, "retry-safe", "sk-sp-test")
	if err != nil {
		t.Fatal(err)
	}
	root := agentqwen.ConsoleRootForStore(store)
	consoleDir := agentqwen.ConsoleConfigDirIn(root, account.Email)
	if err := os.MkdirAll(filepath.Dir(consoleDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(consoleDir, []byte("not a safe credential directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := srRunner{store: store, out: io.Discard, errOut: io.Discard}
	err = runner.remove(t.Context(), account.Email)
	if err == nil || !strings.Contains(err.Error(), "account remains and removal can be retried") {
		t.Fatalf("remove error = %v, want retryable console-cleanup failure", err)
	}
	if _, ok, findErr := store.FindStored(account.Email); findErr != nil || !ok {
		t.Fatalf("Qwen account disappeared before console cleanup: ok=%v err=%v", ok, findErr)
	}
	if body, readErr := os.ReadFile(consoleDir); readErr != nil || !strings.Contains(string(body), "credential") {
		t.Fatalf("failed cleanup changed console artifact: body=%q err=%v", body, readErr)
	}

	if err := os.Remove(consoleDir); err != nil {
		t.Fatal(err)
	}
	if err := runner.remove(t.Context(), account.Email); err != nil {
		t.Fatalf("retry removal: %v", err)
	}
	if _, ok, findErr := store.FindStored(account.Email); findErr != nil || ok {
		t.Fatalf("retried Qwen removal left account: ok=%v err=%v", ok, findErr)
	}
}

func TestQwenRemovalRetriesAfterAccountDeleteFailsPostConsoleCleanup(t *testing.T) {
	var calls []string
	removeConsole := func() error {
		calls = append(calls, "console")
		return nil
	}
	removeAttempts := 0
	removeStored := func() (accounts.StoredCodexAccount, bool, error) {
		calls = append(calls, "account")
		removeAttempts++
		if removeAttempts == 1 {
			return accounts.StoredCodexAccount{Email: "qwen-token:post-cleanup"}, false, errors.New("injected account delete failure")
		}
		return accounts.StoredCodexAccount{Email: "qwen-token:post-cleanup"}, true, nil
	}

	_, ok, err := removeQwenStoredAccount(removeConsole, removeStored)
	if err == nil || ok || !strings.Contains(err.Error(), "console credential removed but account remains; retry removal") {
		t.Fatalf("first removal = ok=%v err=%v, want retry guidance", ok, err)
	}
	if got := strings.Join(calls, ","); got != "console,account" {
		t.Fatalf("removal order = %s, want console before account", got)
	}
	removed, ok, err := removeQwenStoredAccount(removeConsole, removeStored)
	if err != nil || !ok || removed.Email != "qwen-token:post-cleanup" {
		t.Fatalf("retried removal = %+v ok=%v err=%v", removed, ok, err)
	}
	if got := strings.Join(calls, ","); got != "console,account,console,account" {
		t.Fatalf("retry order = %s", got)
	}
}

func TestAdditionalKeyedProvidersCanBeAddedListedAndRemoved(t *testing.T) {
	for _, provider := range []accounts.Provider{
		accounts.ProviderDeepSeek,
		accounts.ProviderTogether,
		accounts.ProviderFireworks,
		accounts.ProviderOpenCodeZen,
	} {
		t.Run(string(provider), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			store := accounts.DefaultCodexStore()
			var out bytes.Buffer
			runner := srRunner{
				store: store, in: strings.NewReader("primary\nprovider-test-key\n"),
				out: &out, errOut: &out,
			}
			if err := runner.run(t.Context(), []string{"add-key", "--provider", string(provider)}); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "provider-test-key") {
				t.Fatal("add-key output leaked the API key")
			}
			accountID := string(provider) + ":primary"
			stored, ok, err := store.FindStored(accountID)
			if err != nil || !ok || stored.Provider != provider {
				t.Fatalf("stored account = %+v ok=%v err=%v", stored, ok, err)
			}
			generationPath := filepath.Join(store.StoreDir(), ".account-generation")
			generationAfterAdd, err := os.ReadFile(generationPath)
			if err != nil || len(generationAfterAdd) == 0 {
				t.Fatalf("add did not publish account generation: %q err=%v", generationAfterAdd, err)
			}
			out.Reset()
			runner.in = strings.NewReader("")
			if err := runner.run(t.Context(), []string{"list"}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), accountID) {
				t.Fatalf("list omitted %q: %s", accountID, out.String())
			}
			if strings.Contains(out.String(), "provider-test-key") {
				t.Fatal("list output leaked the API key")
			}
			out.Reset()
			if err := runner.run(t.Context(), []string{"remove", accountID}); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := store.FindStored(accountID); err != nil || ok {
				t.Fatalf("removed account remains: ok=%v err=%v", ok, err)
			}
			generationAfterRemove, err := os.ReadFile(generationPath)
			if err != nil {
				t.Fatalf("read removal generation: %v", err)
			}
			if bytes.Equal(generationAfterAdd, generationAfterRemove) {
				t.Fatal("remove did not publish a new account generation")
			}
			if strings.Contains(out.String(), "provider-test-key") {
				t.Fatal("account lifecycle output leaked the API key")
			}
		})
	}
}

func TestAddKeyHelpListsEveryAdditionalKeyedProvider(t *testing.T) {
	var out bytes.Buffer
	runner := srRunner{store: accounts.DefaultCodexStore(), out: &out, errOut: &out}
	if err := runner.run(t.Context(), []string{"add-key", "--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("add-key --help error = %v, want flag.ErrHelp", err)
	}
	for _, provider := range []string{"deepseek", "together", "fireworks", "opencode-zen"} {
		if !strings.Contains(out.String(), provider) {
			t.Fatalf("add-key --help omits %q:\n%s", provider, out.String())
		}
		if !strings.Contains(srHelp, provider) {
			t.Fatalf("top-level sr help omits %q", provider)
		}
	}
}

func TestSRQwenHelpDocumentsGenericAccountLifecycle(t *testing.T) {
	var out bytes.Buffer
	runner := srRunner{out: &out}
	if err := runner.qwen(t.Context(), []string{"--help"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"sr add-key --provider qwen-token",
		"sr status",
		"sr remove <account>",
		"sr qwen login [--console-account <email-or-label>] <account>",
		"sr qwen label <account> <email-or-label>",
		"sr qwen proxy [qwen args...]",
		"console plan/quota metadata",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("Qwen help missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "sr qwen add") {
		t.Fatalf("Qwen help advertises a redundant add alias:\n%s", out.String())
	}
}

func TestKimiHelpSeparatesLocalLauncherFromRemoteManagement(t *testing.T) {
	var out bytes.Buffer
	runner := srRunner{out: &out}
	if err := runner.kimiCommand(t.Context(), []string{"--help"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"sr kimi [--account", "sr kimi proxy"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("local Kimi help missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := runner.kimiRemote(t.Context(), srServerConfig{Name: "remote"}, []string{"--help"}); err != nil {
		t.Fatal(err)
	}
	for _, localOnly := range []string{"--account", "sr kimi proxy"} {
		if strings.Contains(out.String(), localOnly) {
			t.Fatalf("remote Kimi help advertises local-only %q:\n%s", localOnly, out.String())
		}
	}
	if !strings.Contains(out.String(), "Managed profiles are stored on server remote") {
		t.Fatalf("remote Kimi help omits server-scoped management:\n%s", out.String())
	}
}

func TestHelpDistinguishesNativeProxyLaunchersFromDirectCLIs(t *testing.T) {
	for name, help := range map[string]string{
		"sr":        srHelp,
		"subrouter": usageText("subrouter"),
	} {
		for _, command := range []string{"qwen proxy", "kimi proxy"} {
			if !strings.Contains(help, command) {
				t.Errorf("%s help omits %q", name, command)
			}
		}
		nativeCommand := name + " agy"
		if strings.Contains(help, "agy proxy") || !strings.Contains(help, nativeCommand) || !strings.Contains(strings.ToLower(help), "pooled") {
			t.Errorf("%s help does not describe pooled AGY routing", name)
		}
		if !strings.Contains(help, "Gemini profiles (routing scaffold only)") {
			t.Errorf("%s help does not disclose Gemini's scaffold-only state", name)
		}
	}
}

func TestStatusHelpCoversEveryConfiguredProvider(t *testing.T) {
	for name, help := range map[string]string{
		"sr":        srHelp,
		"subrouter": usageText("subrouter"),
	} {
		if !strings.Contains(help, "status             Show usage across all configured providers") {
			t.Errorf("%s help does not describe provider-wide status", name)
		}
		if strings.Contains(help, "status             Show Codex and Claude usage") {
			t.Errorf("%s help still limits status to Codex and Claude", name)
		}
	}
}

func TestRemoteQwenKeyFingerprintRequiresSelectedAccount(t *testing.T) {
	statuses := []remoteServerUsageStatus{
		{ID: "qwen-token:other", KeyFingerprint: "key:1111111111"},
		{ID: "qwen-token:work", KeyFingerprint: "key:2222222222"},
	}
	got, err := remoteQwenKeyFingerprint(statuses, "qwen-token:work")
	if err != nil || got != "key:2222222222" {
		t.Fatalf("fingerprint = %q err=%v", got, err)
	}
	if _, err := remoteQwenKeyFingerprint(statuses, "qwen-token:missing"); err == nil {
		t.Fatal("missing selected remote Qwen account was accepted")
	}
	if _, err := remoteQwenKeyFingerprint([]remoteServerUsageStatus{{ID: "qwen-token:work"}}, "qwen-token:work"); err == nil {
		t.Fatal("empty remote Qwen fingerprint was accepted")
	}
}

func TestRemoteQwenConsoleRootIsTenantScopedWithoutExposingTenantKey(t *testing.T) {
	serverA := srServerConfig{Name: "shared", URL: "https://router.example", TenantKey: "srt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	serverB := srServerConfig{Name: "shared", URL: "https://router.example", TenantKey: "srt_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	rootA := qwenRemoteConsoleRoot(serverA)
	rootB := qwenRemoteConsoleRoot(serverB)
	if rootA == rootB {
		t.Fatal("different tenants reused one remote Qwen console credential root")
	}
	if strings.Contains(rootA, serverA.TenantKey) || strings.Contains(rootB, serverB.TenantKey) {
		t.Fatal("tenant key was exposed in a remote Qwen console credential path")
	}
}

func TestSRQwenRemovalRetainsAccountWhenCredentialCleanupFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.DefaultCodexStore()
	accountID := "qwen-token:work"
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:    accountID,
		Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "model-secret",
		},
	}); err != nil {
		t.Fatal(err)
	}
	root := agentqwen.ConsoleRootForStore(store)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), agentqwen.ConsoleConfigDirIn(root, accountID)); err != nil {
		t.Fatal(err)
	}
	runner := srRunner{store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	if err := runner.run(context.Background(), []string{"remove", accountID}); err == nil {
		t.Fatal("removal succeeded despite unsafe console credential directory")
	}
	if _, ok, err := store.FindStored(accountID); err != nil || !ok {
		t.Fatalf("Qwen account should remain retryable: ok=%v err=%v", ok, err)
	}
	if generation, err := os.ReadFile(filepath.Join(store.StoreDir(), ".account-generation")); err != nil || len(generation) == 0 {
		t.Fatalf("failed Qwen removal did not publish rollback invalidation: %q err=%v", generation, err)
	}
}

func TestSRQwenLoginPreparesBrowserAuthAndStoresIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:    "qwen-token:work",
		Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "model-secret",
		},
	}); err != nil {
		t.Fatal(err)
	}
	command := &qwenLoginCommandRunner{}
	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: command}
	if err := runner.run(context.Background(), []string{"qwen", "login", "--console-account", "person@example.com", "qwen-token:work"}); err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"auth", "login", "--console", "--console-site", "international"}
	if command.name != "bl" || !slices.Equal(command.args, wantArgs) {
		t.Fatalf("Bailian command = %q %v", command.name, command.args)
	}
	if got := agentqwen.ConsoleAccount("qwen-token:work"); got != "person@example.com" {
		t.Fatalf("console account = %q", got)
	}
	body, err := os.ReadFile(agentqwen.ConsoleConfigPath("qwen-token:work"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "model-secret") || !strings.Contains(string(body), "console-secret") {
		t.Fatalf("console config did not strip the temporary model key: %s", body)
	}
}

func TestSRQwenReauthPreservesSavedConsoleIdentity(t *testing.T) {
	root := t.TempDir()
	accountID := "qwen-token:work"
	if err := agentqwen.SaveConsoleCredentialIn(root, accountID, agentqwen.ConsoleCredential{
		AccessToken: "expired-console-token",
		Account:     "person@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	command := &qwenLoginCommandRunner{}
	runner := srRunner{in: strings.NewReader(""), out: io.Discard, errOut: io.Discard, cmd: command}
	stored := accounts.StoredCodexAccount{
		Email: accountID, Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "model-secret"},
	}
	// This is the exact no-flag command suggested by an expired-login status.
	if err := runner.qwenLoginStored(t.Context(), root, stored, "", nil); err != nil {
		t.Fatal(err)
	}
	credential, err := agentqwen.ExportConsoleCredentialIn(root, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccessToken != "console-secret" || credential.Account != "person@example.com" {
		t.Fatalf("reauthorized credential = %+v", credential)
	}
}

func TestSRQwenLoginPreservesCompletedAuthorizationWhenDurableSaveFails(t *testing.T) {
	root := filepath.Join(t.TempDir(), "blocked-root")
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := &qwenLoginCommandRunner{}
	runner := srRunner{in: strings.NewReader(""), out: io.Discard, errOut: io.Discard, cmd: command}
	stored := accounts.StoredCodexAccount{
		Email: "qwen-token:work", Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "model-secret"},
	}
	err := runner.qwenLoginStored(t.Context(), root, stored, "person@example.com", nil)
	if err == nil || !strings.Contains(err.Error(), "authorization preserved at") {
		t.Fatalf("login error = %v, want preserved-authorization location", err)
	}
	stageRoot := filepath.Dir(command.configDir)
	t.Cleanup(func() { _ = os.RemoveAll(stageRoot) })
	credential, exportErr := agentqwen.ExportConsoleCredentialIn(stageRoot, stored.Email)
	if exportErr != nil {
		t.Fatalf("completed staged authorization was removed: %v", exportErr)
	}
	if credential.AccessToken != "console-secret" || credential.Account != "person@example.com" {
		t.Fatalf("preserved credential = %+v", credential)
	}
}

func TestQwenConsoleCredentialSyncsToExplicitRemote(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	if err := agentqwen.SaveConsoleCredential("qwen-token:work", agentqwen.ConsoleCredential{
		AccessToken:   "console-secret",
		ConsoleRegion: "ap-southeast-1",
		ConsoleSite:   "international",
		Account:       "person@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	received := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/_subrouter/qwen-console" || req.Header.Get("Authorization") != "Bearer admin-secret" {
			t.Error("Qwen sync used an unexpected path or credential")
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		var payload struct {
			AccountID  string                      `json:"account_id"`
			Credential agentqwen.ConsoleCredential `json:"credential"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Error("Qwen sync payload could not be decoded")
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		if payload.AccountID != "qwen-token:work" || payload.Credential.AccessToken != "console-secret" {
			t.Error("Qwen sync payload omitted expected account data")
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		received = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	if err := defaultSRServerStore(store).save(srServerFile{Default: "test", Servers: []srServerConfig{{Name: "test", URL: server.URL, AdminToken: "admin-secret"}}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_SERVER", "test")
	var out bytes.Buffer
	runner := srRunner{store: store, out: &out, errOut: &out, client: server.Client()}
	if err := runner.syncQwenConsoleToSelectedRemote(context.Background(), "qwen-token:work"); err != nil {
		t.Fatal(err)
	}
	if !received || !strings.Contains(out.String(), "Synced Qwen quota authorization to server: test") {
		t.Fatalf("sync output = %q received=%v", out.String(), received)
	}
}

func TestQwenConsoleCredentialSyncNeverFollowsRedirects(t *testing.T) {
	root := t.TempDir()
	if err := agentqwen.SaveConsoleCredentialIn(root, "qwen-token:work", agentqwen.ConsoleCredential{
		AccessToken: "console-secret", ConsoleRegion: "ap-southeast-1", ConsoleSite: "international",
	}); err != nil {
		t.Fatal(err)
	}
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	runner := srRunner{client: source.Client(), out: io.Discard}
	err := runner.syncQwenConsoleToServer(t.Context(), root, srServerConfig{
		Name: "team", URL: source.URL, AdminToken: "admin-secret",
	}, "qwen-token:work")
	if err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("Qwen redirect error = %v", err)
	}
	if redirected.Load() != 0 {
		t.Fatalf("Qwen credential request followed redirect %d time(s)", redirected.Load())
	}
}

func TestQwenConsoleCredentialSyncRedactsTenantKeyFromTransportError(t *testing.T) {
	root := t.TempDir()
	if err := agentqwen.SaveConsoleCredentialIn(root, "qwen-token:work", agentqwen.ConsoleCredential{
		AccessToken: "console-secret", ConsoleRegion: "ap-southeast-1", ConsoleSite: "international",
	}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	client := server.Client()
	server.Close()
	const tenantKey = "srt_tenant_secret"
	runner := srRunner{client: client, out: io.Discard}
	err := runner.syncQwenConsoleToServer(t.Context(), root, srServerConfig{
		Name: "tenant", URL: server.URL, TenantKey: tenantKey,
	}, "qwen-token:work")
	if err == nil {
		t.Fatal("sync unexpectedly succeeded against a closed server")
	}
	if strings.Contains(err.Error(), tenantKey) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("sync transport error exposed tenant credential: %v", err)
	}
}

func TestSRQwenLoginTargetsSelectedRemoteAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	accountID := "qwen-token:work"
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:    accountID,
		Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "model-secret",
		},
	}); err != nil {
		t.Fatal(err)
	}
	received := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer admin-secret" {
			t.Fatalf("authorization = %q", req.Header.Get("Authorization"))
		}
		switch req.URL.Path {
		case "/_subrouter/accounts":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id": accountID, "provider": "qwen-token", "auth_mode": "apikey",
			}})
		case "/_subrouter/usage-status":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id": accountID, "provider": "qwen-token", "auth_mode": "apikey",
				"key_fingerprint": accounts.APIKeyFingerprint("sk-sp-remote-test"),
			}})
		case "/_subrouter/qwen-console":
			received = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, req)
		}
	}))
	defer server.Close()
	if err := defaultSRServerStore(store).save(srServerFile{Default: "test", Servers: []srServerConfig{{Name: "test", URL: server.URL, AdminToken: "admin-secret"}}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_CODEX_SERVER", "test")
	command := &qwenLoginCommandRunner{}
	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader("sk-sp-remote-test\n"), out: &out, errOut: &out, client: server.Client(), cmd: command}
	if err := runner.run(context.Background(), []string{"qwen", "login", "--console-account", "person@example.com", accountID}); err != nil {
		t.Fatal(err)
	}
	if !received || !strings.Contains(out.String(), "Synced Qwen quota authorization to server: test") {
		t.Fatalf("sync output = %q received=%v", out.String(), received)
	}
}

type qwenLoginCommandRunner struct {
	name      string
	args      []string
	configDir string
}

func (r *qwenLoginCommandRunner) Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return r.RunWithEnv(ctx, name, args, nil, stdin, stdout, stderr)
}

func (r *qwenLoginCommandRunner) RunWithEnv(_ context.Context, name string, args, env []string, _ io.Reader, _, _ io.Writer) error {
	r.name = name
	r.args = append([]string(nil), args...)
	var configDir string
	for _, value := range env {
		if strings.HasPrefix(value, "BAILIAN_CONFIG_DIR=") {
			configDir = strings.TrimPrefix(value, "BAILIAN_CONFIG_DIR=")
		}
	}
	r.configDir = configDir
	body, err := os.ReadFile(filepath.Join(configDir, "config.json"))
	if err != nil {
		return err
	}
	var config map[string]any
	if err := json.Unmarshal(body, &config); err != nil {
		return err
	}
	config["access_token"] = "console-secret"
	body, err = json.Marshal(config)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(configDir, "config.json"), body, 0o600)
}

func (r *qwenLoginCommandRunner) Output(context.Context, string, []string) ([]byte, error) {
	return nil, nil
}

func TestSRTraceShowsOAuthBreadcrumbs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "a@example.com",
		AddedAt: "2026-04-28T00:00:00Z",
		Auth: accounts.CodexAuthFile{AuthMode: "chatgpt", Tokens: &accounts.CodexTokens{
			AccessToken:  "access-token",
			RefreshToken: "raw-refresh-token",
			IDToken:      "id-token",
			AccountID:    "acct_123",
		}},
		Breadcrumbs: []accounts.CodexAuthBreadcrumb{{
			At:              "2026-05-19T08:00:00Z",
			Event:           "refresh_terminal_failure",
			Source:          "oauth_refresh",
			Reason:          "proxy.score-accounts",
			Host:            "subrouter-team",
			PID:             123,
			PPID:            1,
			Executable:      "/usr/local/bin/subrouter",
			WorkingDir:      "/var/lib/subrouter",
			SourcePath:      "/var/lib/subrouter/codex/accounts/a@example.com.json",
			RefreshFP:       "refreshfp",
			OldRefreshFP:    "oldrefreshfp",
			StatusCode:      http.StatusUnauthorized,
			ProviderType:    "invalid_request_error",
			ProviderCode:    "refresh_token_reused",
			ProviderMessage: "Your refresh token has already been used to generate a new access token. Please try signing in again.",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"trace", "a@example.com"}); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	for _, want := range []string{
		"OAuth breadcrumbs for a@example.com",
		"refresh_terminal_failure",
		"reason=\"proxy.score-accounts\"",
		"host=\"subrouter-team\"",
		"pid=\"123\"",
		"refresh=\"refreshfp\"",
		"old_refresh=\"oldrefreshfp\"",
		"provider_code=\"refresh_token_reused\"",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("trace output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "raw-refresh-token") {
		t.Fatalf("trace output leaked raw refresh token:\n%s", got)
	}
}

func TestSRSwitchAPIKeyWritesCodexAuthJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "apikey:paid",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-test",
			Tokens:       &accounts.CodexTokens{IDToken: ""},
		},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"switch", "paid"}); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var active map[string]any
	if err := json.Unmarshal(body, &active); err != nil {
		t.Fatal(err)
	}
	if active["auth_mode"] != "apikey" {
		t.Fatalf("auth_mode = %v, want apikey", active["auth_mode"])
	}
	if active["OPENAI_API_KEY"] != "sk-test" {
		t.Fatalf("OPENAI_API_KEY = %v, want sk-test", active["OPENAI_API_KEY"])
	}
	if _, ok := active["tokens"]; ok {
		t.Fatal("tokens should be stripped for API-key auth")
	}
	var openCodeAuth map[string]map[string]any
	readJSONFile(t, filepath.Join(home, ".local", "share", "opencode", "auth.json"), &openCodeAuth)
	if got := openCodeAuth["openai"]["type"]; got != "api" {
		t.Fatalf("opencode API-key type = %v, want api", got)
	}
	if got := openCodeAuth["openai"]["key"]; got != "sk-test" {
		t.Fatalf("opencode API key was not synced")
	}
	var piAuth map[string]map[string]any
	readJSONFile(t, filepath.Join(home, ".pi", "agent", "auth.json"), &piAuth)
	if got := piAuth["openai"]["type"]; got != "api_key" {
		t.Fatalf("pi API-key type = %v, want api_key", got)
	}
	if got := piAuth["openai"]["key"]; got != "sk-test" {
		t.Fatalf("pi API key was not synced")
	}
	if !strings.Contains(out.String(), "Switched to apikey:paid") {
		t.Fatalf("missing switch confirmation:\n%s", out.String())
	}
}

func TestSRSwitchPublishesOAuthIsolationDowngradeToRunningServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	auth := testCodexAuth("isolated@example.test", "acct_isolated")
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:                 "isolated@example.test",
		AddedAt:               time.Now().UTC().Format(time.RFC3339),
		OAuthCredentialOrigin: accounts.CodexOAuthOriginIsolatedServerLogin,
		Auth:                  auth,
	}); err != nil {
		t.Fatal(err)
	}

	servingStore := store
	servingStore.DisableActiveAuthSync = true
	servingStore.RequireIsolatedOAuth = true
	ref, err := proxy.OpenAccountRef(servingStore, agentclaude.Store{Dir: filepath.Join(home, "claude-store")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	beforeGeneration := ref.Generation()
	handler := (proxy.Server{AccountRef: ref}).Handler()

	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.switchAccount(context.Background(), "isolated@example.test", srSwitchOptions{}); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "http://subrouter.local/_subrouter/accounts", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("account reload status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if got := ref.Generation(); got <= beforeGeneration {
		t.Fatalf("running server generation = %d, want > %d after switch", got, beforeGeneration)
	}

	stored, ok, err := store.FindStored("isolated@example.test")
	if err != nil || !ok {
		t.Fatalf("stored account found = %v, err = %v", ok, err)
	}
	if stored.OAuthCredentialOrigin != accounts.CodexOAuthOriginInteractiveImport {
		t.Fatalf("stored OAuth origin = %q, want interactive import", stored.OAuthCredentialOrigin)
	}
	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil || !ok {
		t.Fatalf("active auth found = %v, err = %v", ok, err)
	}
	if active.Tokens == nil || active.Tokens.RefreshToken != auth.Tokens.RefreshToken {
		t.Fatal("active auth does not contain switched credential")
	}

	loaded := ref.All()
	if len(loaded) != 1 {
		t.Fatalf("running server account count = %d, want one", len(loaded))
	}
	_, refreshErr := ref.Refresh(context.Background(), loaded[0])
	var unisolated *accounts.CodexUnisolatedCredentialError
	if !errors.As(refreshErr, &unisolated) {
		t.Fatalf("running server refresh error = %v, want isolation rejection", refreshErr)
	}
}

func TestSRSwitchDoesNotWriteActiveAuthOrDowngradeWhenPublicationFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	account := accounts.StoredCodexAccount{
		Email:                 "isolated@example.test",
		AddedAt:               time.Now().UTC().Format(time.RFC3339),
		OAuthCredentialOrigin: accounts.CodexOAuthOriginIsolatedServerLogin,
		Auth:                  testCodexAuth("isolated@example.test", "acct_isolated"),
	}
	if err := store.SaveStored(account); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(store.StoreDir(), ".account-generation"), 0o700); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.switchAccount(context.Background(), account.Email, srSwitchOptions{}); err == nil {
		t.Fatal("switch unexpectedly succeeded when generation publication failed")
	}
	stored, ok, err := store.FindStored(account.Email)
	if err != nil || !ok {
		t.Fatalf("stored account found = %v, err = %v", ok, err)
	}
	if stored.OAuthCredentialOrigin != accounts.CodexOAuthOriginIsolatedServerLogin {
		t.Fatalf("stored OAuth origin = %q, want isolated server login", stored.OAuthCredentialOrigin)
	}
	if _, err := os.Stat(accounts.DefaultCodexAuthPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active auth was written despite publication failure: %v", err)
	}
}

func TestSRSwitchDoesNotSyncActiveAuthWhenPublicationFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	account := accounts.StoredCodexAccount{
		Email:                 "isolated@example.test",
		AddedAt:               time.Now().UTC().Format(time.RFC3339),
		OAuthCredentialOrigin: accounts.CodexOAuthOriginIsolatedServerLogin,
		Auth:                  testCodexAuth("isolated@example.test", "acct_isolated"),
	}
	if err := store.SaveStored(account); err != nil {
		t.Fatal(err)
	}
	interactive := testCodexAuth("isolated@example.test", "acct_interactive")
	if err := accounts.WriteActiveCodexAuth(interactive); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(store.StoreDir(), ".account-generation"), 0o700); err != nil {
		t.Fatal(err)
	}

	runner := srRunner{store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	if err := runner.switchAccount(context.Background(), account.Email, srSwitchOptions{}); err == nil {
		t.Fatal("switch unexpectedly succeeded when active-sync publication failed")
	}
	stored, ok, err := store.FindStored(account.Email)
	if err != nil || !ok {
		t.Fatalf("stored account found = %v, err = %v", ok, err)
	}
	if stored.OAuthCredentialOrigin != accounts.CodexOAuthOriginIsolatedServerLogin {
		t.Fatalf("stored OAuth origin = %q, want isolated server login", stored.OAuthCredentialOrigin)
	}
	if stored.Auth.Tokens == nil || stored.Auth.Tokens.RefreshToken != account.Auth.Tokens.RefreshToken {
		t.Fatal("active auth was imported despite publication failure")
	}
	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil || !ok || active.Tokens == nil || active.Tokens.RefreshToken != interactive.Tokens.RefreshToken {
		t.Fatal("publication failure changed the pre-existing active auth")
	}
}

func TestSRSwitchDoesNotRefreshStoredAuthWhenPublicationFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	auth := testCodexAuth("expired@example.test", "acct_expired")
	auth.Tokens.AccessToken = testJWT(map[string]any{
		"exp": time.Now().Add(-time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_expired",
		},
	})
	account := accounts.StoredCodexAccount{
		Email:   "expired@example.test",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    auth,
	}
	if err := store.SaveStored(account); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(store.StoreDir(), ".account-generation"), 0o700); err != nil {
		t.Fatal(err)
	}
	refreshRequests := 0
	client := &http.Client{Transport: srRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "auth.openai.com" && request.URL.Path == "/oauth/token" {
			refreshRequests++
		}
		return nil, errors.New("refresh endpoint should not be called")
	})}

	runner := srRunner{store: store, client: client, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	if err := runner.switchAccount(context.Background(), account.Email, srSwitchOptions{}); err == nil {
		t.Fatal("switch unexpectedly succeeded when refresh publication failed")
	}
	if refreshRequests != 0 {
		t.Fatalf("refresh endpoint requests = %d, want zero", refreshRequests)
	}
	stored, ok, err := store.FindStored(account.Email)
	if err != nil || !ok {
		t.Fatalf("stored account found = %v, err = %v", ok, err)
	}
	if stored.Auth.Tokens == nil || stored.Auth.Tokens.RefreshToken != account.Auth.Tokens.RefreshToken {
		t.Fatal("stored credential changed despite publication failure")
	}
}

func TestSRSwitchDoesNotActivateStaleAuthAfterCommittedRefreshTeardownFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()

	activeAccount := accounts.StoredCodexAccount{
		Email:   "active@example.test",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    testCodexAuth("active@example.test", "acct_active"),
	}
	if err := store.SaveStored(activeAccount); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WriteActiveCodexAuth(activeAccount.Auth); err != nil {
		t.Fatal(err)
	}

	staleAuth := testCodexAuth("target@example.test", "acct_target")
	staleAuth.Tokens.AccessToken = testJWT(map[string]any{
		"exp": time.Now().Add(-time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_target",
		},
	})
	target := accounts.StoredCodexAccount{
		Email:   "target@example.test",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    staleAuth,
	}
	if err := store.SaveStored(target); err != nil {
		t.Fatal(err)
	}

	freshAccess := testJWT(map[string]any{
		"exp": time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_target",
		},
	})
	freshID := testJWT(map[string]any{
		"exp":   time.Now().Add(time.Hour).Unix(),
		"email": target.Email,
	})
	client := &http.Client{Transport: srRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "auth.openai.com" && request.URL.Path == "/oauth/token" {
			return srJSONResponse(request, http.StatusOK, fmt.Sprintf(
				`{"access_token":%q,"refresh_token":%q,"id_token":%q}`,
				freshAccess, "fresh-refresh", freshID,
			)), nil
		}
		return nil, errors.New("usage unavailable")
	})}

	teardownErr := errors.New("close account publication transaction")
	var out bytes.Buffer
	runner := srRunner{
		store: store, client: client, in: strings.NewReader(""), out: &out, errOut: &out,
		withCodexRefreshPublication: func(
			ctx context.Context,
			storeDir string,
			mutate func(func() error) error,
		) error {
			if err := proxy.WithAccountDiskMutationPublication(ctx, storeDir, mutate); err != nil {
				return err
			}
			return teardownErr
		},
	}

	err := runner.switchAccount(context.Background(), target.Email, srSwitchOptions{})
	if !errors.Is(err, teardownErr) {
		t.Fatalf("switch error = %v, want committed-refresh teardown error", err)
	}
	stored, ok, err := store.FindStored(target.Email)
	if err != nil || !ok {
		t.Fatalf("stored target found = %v, err = %v", ok, err)
	}
	if stored.Auth.Tokens == nil || stored.Auth.Tokens.RefreshToken != "fresh-refresh" {
		t.Fatalf("stored refresh token = %v, want committed fresh credential", stored.Auth.Tokens)
	}
	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil || !ok {
		t.Fatalf("active auth found = %v, err = %v", ok, err)
	}
	if active.Tokens == nil || active.Tokens.RefreshToken != activeAccount.Auth.Tokens.RefreshToken {
		t.Fatal("failed switch replaced active auth after refresh transaction teardown")
	}
	if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); err != nil {
		t.Fatalf("committed refresh generation was not published: %v", err)
	}
	if strings.Contains(out.String(), "Switched to") {
		t.Fatalf("failed switch reported success:\n%s", out.String())
	}
}
func TestSRSwitchSyncsOpenCodeAndPiAuth(t *testing.T) {
	home := t.TempDir()
	xdgData := t.TempDir()
	piDir := filepath.Join(home, "pi-agent")
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", xdgData)
	t.Setenv("PI_CODING_AGENT_DIR", piDir)

	access := testJWT(map[string]any{
		"exp": time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_test",
		},
	})
	id := testJWT(map[string]any{
		"exp":   time.Now().Add(time.Hour).Unix(),
		"email": "sync@example.com",
	})
	store := accounts.DefaultCodexStore()
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "sync@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth: accounts.CodexAuthFile{Tokens: &accounts.CodexTokens{
			AccessToken:  access,
			RefreshToken: "refresh-test",
			IDToken:      id,
		}},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"switch", "sync@example.com"}); err != nil {
		t.Fatal(err)
	}

	var openCodeAuth map[string]map[string]any
	readJSONFile(t, filepath.Join(xdgData, "opencode", "auth.json"), &openCodeAuth)
	if got := openCodeAuth["openai"]["type"]; got != "oauth" {
		t.Fatalf("opencode openai type = %v, want oauth", got)
	}
	if got := openCodeAuth["openai"]["access"]; got != access {
		t.Fatalf("opencode access was not synced")
	}
	if got := openCodeAuth["openai"]["accountId"]; got != "acct_test" {
		t.Fatalf("opencode accountId = %v, want acct_test", got)
	}

	var piAuth map[string]map[string]any
	readJSONFile(t, filepath.Join(piDir, "auth.json"), &piAuth)
	if got := piAuth["openai-codex"]["type"]; got != "oauth" {
		t.Fatalf("pi openai-codex type = %v, want oauth", got)
	}
	if got := piAuth["openai-codex"]["access"]; got != access {
		t.Fatalf("pi access was not synced")
	}
	if got := piAuth["openai-codex"]["accountId"]; got != "acct_test" {
		t.Fatalf("pi accountId = %v, want acct_test", got)
	}
}

func TestSRAddUsesIsolatedLoginAndLeavesActiveAccountUnchanged(t *testing.T) {
	home := t.TempDir()
	xdgData := t.TempDir()
	piDir := filepath.Join(home, "pi-agent")
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", xdgData)
	t.Setenv("PI_CODING_AGENT_DIR", piDir)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("alice@example.com", "acct_active")
	addedAuth := testCodexAuth("founders@example.com", "acct_added")
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "alice@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    activeAuth,
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}
	installFakeCodexLogin(t, home, addedAuth)

	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	// "add" now asks which provider; this test exercises the Codex path, so it
	// names it rather than relying on a default that no longer exists.
	if err := runner.run(context.Background(), []string{"add", "codex"}); err != nil {
		t.Fatal(err)
	}

	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing active auth")
	}
	email, err := accounts.ExtractEmailFromJWT(active.Tokens.IDToken)
	if err != nil {
		t.Fatal(err)
	}
	if email != "alice@example.com" {
		t.Fatalf("active email = %q, want alice@example.com", email)
	}
	added, found, err := store.FindStored("founders@example.com")
	if err != nil {
		t.Fatal(err)
	} else if !found {
		t.Fatal("newly logged-in account was not imported")
	}
	if added.OAuthCredentialOrigin != accounts.CodexOAuthOriginIsolatedServerLogin {
		t.Fatalf("OAuth origin = %q, want isolated server login", added.OAuthCredentialOrigin)
	}
	if !strings.Contains(out.String(), "Local Codex auth was left unchanged") {
		t.Fatalf("missing isolation message:\n%s", out.String())
	}
}

func TestSRAutoSwitchesWhenActiveAccountIsExhausted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("alice@example.com", "acct_active")
	bestAuth := testCodexAuth("bob@example.com", "acct_best")
	for email, auth := range map[string]accounts.CodexAuthFile{
		"alice@example.com": activeAuth,
		"bob@example.com":   bestAuth,
	} {
		if err := store.SaveStored(accounts.StoredCodexAccount{
			Email:   email,
			AddedAt: time.Now().UTC().Format(time.RFC3339),
			Auth:    auth,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	usageByToken := map[string]float64{
		activeAuth.Tokens.AccessToken: 100,
		bestAuth.Tokens.AccessToken:   1,
	}
	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			used := usageByToken[strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")]
			return usageResponse(used), nil
		})},
	}

	if err := runner.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing active auth")
	}
	email, err := accounts.ExtractEmailFromJWT(active.Tokens.IDToken)
	if err != nil {
		t.Fatal(err)
	}
	if email != "bob@example.com" {
		t.Fatalf("active email = %q, want bob@example.com", email)
	}
	if !strings.Contains(out.String(), "Auto-switched to bob@example.com") {
		t.Fatalf("missing auto-switch confirmation:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Switch to (#):") {
		t.Fatalf("prompt should be skipped after auto-switch:\n%s", out.String())
	}
}

func TestSRPickSwitchesToRecommendedAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("alice@example.com", "acct_active")
	bestAuth := testCodexAuth("bob@example.com", "acct_best")
	for email, auth := range map[string]accounts.CodexAuthFile{
		"alice@example.com": activeAuth,
		"bob@example.com":   bestAuth,
	} {
		if err := store.SaveStored(accounts.StoredCodexAccount{
			Email:   email,
			AddedAt: time.Now().UTC().Format(time.RFC3339),
			Auth:    auth,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	usageByToken := map[string]float64{
		activeAuth.Tokens.AccessToken: 80,
		bestAuth.Tokens.AccessToken:   1,
	}
	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			used := usageByToken[strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")]
			return usageResponse(used), nil
		})},
	}

	if err := runner.run(context.Background(), []string{"pick"}); err != nil {
		t.Fatal(err)
	}

	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing active auth")
	}
	email, err := accounts.ExtractEmailFromJWT(active.Tokens.IDToken)
	if err != nil {
		t.Fatal(err)
	}
	if email != "bob@example.com" {
		t.Fatalf("active email = %q, want bob@example.com", email)
	}
	if !strings.Contains(out.String(), "Picked recommended account: bob@example.com") {
		t.Fatalf("missing pick confirmation:\n%s", out.String())
	}
	if strings.Contains(out.String(), "alice@example.com") {
		t.Fatalf("successful pick should only display the picked row:\n%s", out.String())
	}
}

func TestSRPickSucceedsWhenRecommendedActiveHasQuota(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("alice@example.com", "acct_active")
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "alice@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    activeAuth,
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return usageResponseWindows(20, 20), nil
		})},
	}

	if err := runner.run(context.Background(), []string{"pick"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Already using recommended account: alice@example.com") {
		t.Fatalf("missing already using confirmation:\n%s", out.String())
	}
}

func TestSRPickFailsWhenNoRecommendedAccountHasQuota(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("alice@example.com", "acct_active")
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "alice@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    activeAuth,
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return usageResponseWindows(100, 100), nil
		})},
	}

	err := runner.run(context.Background(), []string{"pick"})
	if err == nil || !strings.Contains(err.Error(), "no recommended account has quota") {
		t.Fatalf("err = %v, want no quota failure", err)
	}
	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing active auth")
	}
	email, err := accounts.ExtractEmailFromJWT(active.Tokens.IDToken)
	if err != nil {
		t.Fatal(err)
	}
	if email != "alice@example.com" {
		t.Fatalf("active email = %q, want alice@example.com", email)
	}
}

func TestSRDefaultShowsCookedAccountAndDoesNotPromptWhenAllCooked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("alice@example.com", "acct_active")
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "alice@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    activeAuth,
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return usageResponseWindows(0, 100), nil
		})},
	}

	if err := runner.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if !strings.Contains(got, "All of your OAuth accounts are cooked") {
		t.Fatalf("missing all-cooked warning:\n%s", got)
	}
	if !strings.Contains(got, "active, cooked") || !strings.Contains(got, "cooked, canno") {
		t.Fatalf("missing cooked row state:\n%s", got)
	}
	if strings.Contains(got, "Switch to (#):") {
		t.Fatalf("should not prompt when all OAuth accounts are cooked:\n%s", got)
	}
}

func TestSRDefaultShowsTemporarilyCookedAccountWhenShortWindowConsumed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("alice@example.com", "acct_active")
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "alice@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    activeAuth,
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader("\n"),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return usageResponseWindows(100, 55), nil
		})},
	}

	if err := runner.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if !strings.Contains(got, "active, temp") {
		t.Fatalf("missing temporarily cooked marker:\n%s", got)
	}
	if !strings.Contains(got, "temp cooked") {
		t.Fatalf("missing temporarily cooked reason:\n%s", got)
	}
	if strings.Contains(got, "active, cooked") || strings.Contains(got, "All of your OAuth accounts are cooked") {
		t.Fatalf("short-window saturation should not be treated as permanently cooked:\n%s", got)
	}
	if strings.Contains(got, "recommended") {
		t.Fatalf("temporarily cooked account should not be recommended:\n%s", got)
	}
	if strings.Contains(got, "Switch to (#):") {
		t.Fatalf("should not prompt when every OAuth account is unavailable for new sessions:\n%s", got)
	}
}

func TestModelScopedShortWindowDoesNotTemporarilyCookWholeAccount(t *testing.T) {
	windows := []accounts.UsageWindow{
		{Name: "primary", UsedPercent: 28, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
		{
			Name:               "GPT-5.3-Codex-Spark/primary",
			Feature:            "GPT-5.3-Codex-Spark",
			UsedPercent:        100,
			LimitWindowSeconds: int64((5 * time.Hour) / time.Second),
			ResetAfterSeconds:  int64((4 * time.Hour) / time.Second),
		},
		{
			Name:               "GPT-5.3-Codex-Spark/secondary",
			Feature:            "GPT-5.3-Codex-Spark",
			UsedPercent:        48,
			LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second),
		},
	}

	tempCooked, reason := tempCookedFromWindows(windows)
	if tempCooked {
		t.Fatalf("account should remain switchable when only a model-scoped short window is exhausted (reason=%q)", reason)
	}
}

func TestSRInteractiveRefusesTemporarilyCookedSelection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("active@example.com", "acct_active")
	tempCookedAuth := testCodexAuth("temp@example.com", "acct_temp")
	for email, auth := range map[string]accounts.CodexAuthFile{
		"active@example.com": activeAuth,
		"temp@example.com":   tempCookedAuth,
	} {
		if err := store.SaveStored(accounts.StoredCodexAccount{
			Email:   email,
			AddedAt: time.Now().UTC().Format(time.RFC3339),
			Auth:    auth,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader("2\n"),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
			if token == tempCookedAuth.Tokens.AccessToken {
				return usageResponseWindows(100, 20), nil
			}
			return usageResponseWindows(0, 20), nil
		})},
	}

	err := runner.run(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "account is temporarily cooked") {
		t.Fatalf("err = %v, want temporarily cooked refusal", err)
	}

	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing active auth")
	}
	email, err := accounts.ExtractEmailFromJWT(active.Tokens.IDToken)
	if err != nil {
		t.Fatal(err)
	}
	if email != "active@example.com" {
		t.Fatalf("active email = %q, want active@example.com", email)
	}
}

func TestSRInteractiveRefusesCookedSelection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("active@example.com", "acct_active")
	cookedAuth := testCodexAuth("cooked@example.com", "acct_cooked")
	for email, auth := range map[string]accounts.CodexAuthFile{
		"active@example.com": activeAuth,
		"cooked@example.com": cookedAuth,
	} {
		if err := store.SaveStored(accounts.StoredCodexAccount{
			Email:   email,
			AddedAt: time.Now().UTC().Format(time.RFC3339),
			Auth:    auth,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader("2\n"),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
			if token == cookedAuth.Tokens.AccessToken {
				return usageResponseWindows(0, 100), nil
			}
			return usageResponseWindows(0, 0), nil
		})},
	}

	err := runner.run(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "account is cooked") {
		t.Fatalf("err = %v, want cooked refusal", err)
	}

	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing active auth")
	}
	email, err := accounts.ExtractEmailFromJWT(active.Tokens.IDToken)
	if err != nil {
		t.Fatal(err)
	}
	if email != "active@example.com" {
		t.Fatalf("active email = %q, want active@example.com", email)
	}
}

func TestSRSwitchRefusesCookedAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("active@example.com", "acct_active")
	cookedAuth := testCodexAuth("cooked@example.com", "acct_cooked")
	for email, auth := range map[string]accounts.CodexAuthFile{
		"active@example.com": activeAuth,
		"cooked@example.com": cookedAuth,
	} {
		if err := store.SaveStored(accounts.StoredCodexAccount{
			Email:   email,
			AddedAt: time.Now().UTC().Format(time.RFC3339),
			Auth:    auth,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return usageResponseWindows(0, 100), nil
		})},
	}

	err := runner.run(context.Background(), []string{"switch", "cooked@example.com"})
	if err == nil || !strings.Contains(err.Error(), "account is cooked") {
		t.Fatalf("err = %v, want cooked refusal", err)
	}

	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing active auth")
	}
	email, err := accounts.ExtractEmailFromJWT(active.Tokens.IDToken)
	if err != nil {
		t.Fatal(err)
	}
	if email != "active@example.com" {
		t.Fatalf("active email = %q, want active@example.com", email)
	}
}

func TestSRRankRowsKeepsAPIKeyAsFallback(t *testing.T) {
	rows := []srUsageRow{
		{
			email:    "apikey:paid",
			authMode: accounts.AuthModeAPIKey,
			score:    selectacct.Score{AccountID: "apikey:paid", Headroom: 1, ShortHeadroom: 1},
		},
		{
			email:    "later@example.com",
			authMode: accounts.AuthModeOAuth,
			score: selectacct.Score{
				AccountID:              "later@example.com",
				Headroom:               1,
				ShortHeadroom:          1,
				ShortResetAfterSeconds: int64((5 * time.Hour) / time.Second),
				ExpiryPressure:         1 / float64((5*time.Hour)/time.Second),
			},
		},
		{
			email:    "soon@example.com",
			authMode: accounts.AuthModeOAuth,
			score: selectacct.Score{
				AccountID:              "soon@example.com",
				Headroom:               0.90,
				ShortHeadroom:          0.90,
				ShortResetAfterSeconds: int64((2 * time.Hour) / time.Second),
				ExpiryPressure:         0.90 / float64((2*time.Hour)/time.Second),
			},
		},
	}

	rankUsageRows(rows)

	if rows[0].email != "soon@example.com" {
		t.Fatalf("first = %s, want soon@example.com", rows[0].email)
	}
	if rows[len(rows)-1].email != "apikey:paid" {
		t.Fatalf("last = %s, want apikey:paid", rows[len(rows)-1].email)
	}
	if !rows[0].gtoRecommended {
		t.Fatal("best row should be marked recommended")
	}
}

func TestSRRankRowsFallsBackToAPIKeyBeforeExhaustedOAuth(t *testing.T) {
	rows := []srUsageRow{
		{
			email:    "empty@example.com",
			authMode: accounts.AuthModeOAuth,
			score:    selectacct.Score{AccountID: "empty@example.com", Headroom: 0, ShortHeadroom: 0},
		},
		{
			email:    "apikey:paid",
			authMode: accounts.AuthModeAPIKey,
			score:    selectacct.Score{AccountID: "apikey:paid", Headroom: 0.01, ShortHeadroom: 0.01},
		},
	}

	rankUsageRows(rows)

	if rows[0].email != "apikey:paid" {
		t.Fatalf("first = %s, want apikey:paid", rows[0].email)
	}
	if !rows[0].gtoRecommended {
		t.Fatal("API-key fallback should be recommended when OAuth is exhausted")
	}
}

func TestSRRankRowsKeepsTemporarilyCookedAboveCooked(t *testing.T) {
	rows := []srUsageRow{
		{
			email:        "cooked@example.com",
			authMode:     accounts.AuthModeOAuth,
			score:        selectacct.Score{AccountID: "cooked@example.com", Headroom: 0, ShortHeadroom: 1, ShortResetAfterSeconds: int64((5 * time.Hour) / time.Second)},
			cooked:       true,
			cookedReason: "7d limit fully consumed",
		},
		{
			email:            "temp@example.com",
			authMode:         accounts.AuthModeOAuth,
			score:            selectacct.Score{AccountID: "temp@example.com", Headroom: 0, ShortHeadroom: 0, ShortResetAfterSeconds: int64((3 * time.Hour) / time.Second)},
			tempCooked:       true,
			tempCookedReason: "5h limit fully consumed",
		},
	}

	rankUsageRows(rows)

	if rows[0].email != "temp@example.com" {
		t.Fatalf("first = %s, want temp@example.com", rows[0].email)
	}
	if rows[0].gtoRecommended || rows[1].gtoRecommended {
		t.Fatalf("unavailable rows should not be recommended: %#v", rows)
	}
}

func TestSRRankRowsGroupsProvidersSeparately(t *testing.T) {
	rows := []srUsageRow{
		{
			email:    "claude-inactive@example.com",
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderClaude,
			score:    selectacct.Score{AccountID: "claude-inactive@example.com", Headroom: 1, ShortHeadroom: 1},
		},
		{
			email:    "codex-low@example.com",
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderCodex,
			score:    selectacct.Score{AccountID: "codex-low@example.com", Headroom: 0.3, ShortHeadroom: 0.3},
		},
		{
			email:    "claude-active@example.com",
			active:   true,
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderClaude,
			score:    selectacct.Score{AccountID: "claude-active@example.com", Headroom: 0.5, ShortHeadroom: 0.5},
		},
		{
			email:    "codex-high@example.com",
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderCodex,
			score:    selectacct.Score{AccountID: "codex-high@example.com", Headroom: 0.9, ShortHeadroom: 0.9},
		},
	}

	rankUsageRows(rows)

	got := []string{rows[0].email, rows[1].email, rows[2].email, rows[3].email}
	want := []string{"codex-high@example.com", "codex-low@example.com", "claude-active@example.com", "claude-inactive@example.com"}
	if !slices.Equal(got, want) {
		t.Fatalf("ranked rows = %#v, want %#v", got, want)
	}
	if !rows[0].gtoRecommended {
		t.Fatal("best Codex row should still be recommended")
	}
	if rows[2].gtoRecommended || rows[3].gtoRecommended {
		t.Fatalf("Claude rows should not be marked as Codex recommendations: %#v", rows)
	}
}

func TestScoreFromWindowsUsesAllDisplayedRateLimits(t *testing.T) {
	score := scoreFromWindows("a@example.com", []accounts.UsageWindow{
		{Name: "primary", UsedPercent: 10, LimitWindowSeconds: int64((5 * time.Hour) / time.Second), ResetAfterSeconds: int64((3 * time.Hour) / time.Second)},
		{Name: "secondary", UsedPercent: 10, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAfterSeconds: int64((6 * 24 * time.Hour) / time.Second)},
		{Name: "GPT-5.3-Codex-Spark (weekly)", UsedPercent: 100, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAfterSeconds: int64((2 * 24 * time.Hour) / time.Second)},
	})

	if score.Headroom != 0 {
		t.Fatalf("headroom = %.2f, want 0", score.Headroom)
	}
}

func TestRenderBarSupportsANSIColors(t *testing.T) {
	plain := renderBar(25, false)
	if !strings.HasPrefix(plain, "[") || !strings.HasSuffix(plain, "]") {
		t.Fatalf("plain bar = %q", plain)
	}

	colored := renderBar(25, true)
	if !strings.Contains(colored, ansiBGGreen) || !strings.Contains(colored, ansiBGGray) || !strings.HasSuffix(colored, ansiReset) {
		t.Fatalf("colored bar missing ANSI segments: %q", colored)
	}
}

func TestDisplayUsageRowsGridWhenForced(t *testing.T) {
	t.Setenv("COLUMNS", "200")
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{
		{
			email:          "lawrence@cmux.com",
			planType:       "pro",
			active:         true,
			gtoRecommended: true,
			gtoReason:      "45% bottleneck left, 5h resets in 1h",
			authMode:       accounts.AuthModeOAuth,
			windows: []accounts.UsageWindow{
				{Name: "primary", UsedPercent: 55, LimitWindowSeconds: int64((5 * time.Hour) / time.Second), ResetAfterSeconds: int64(time.Hour / time.Second)},
				{Name: "secondary", UsedPercent: 20, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAfterSeconds: int64((4 * 24 * time.Hour) / time.Second)},
				{Name: "GPT-5.3-Codex-Spark/primary", UsedPercent: 8, LimitWindowSeconds: int64((5 * time.Hour) / time.Second), ResetAfterSeconds: int64((30 * time.Minute) / time.Second)},
				{Name: "GPT-5.3-Codex-Spark/secondary", UsedPercent: 2, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAfterSeconds: int64((6 * 24 * time.Hour) / time.Second)},
			},
			credits:            &accounts.CreditsInfo{Balance: "0"},
			complimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Consumed: true},
		},
	}, true)

	got := out.String()
	if !strings.Contains(got, "Account") || !strings.Contains(got, "Spark wk") || !strings.Contains(got, "1x reset") {
		t.Fatalf("grid header missing:\n%s", got)
	}
	if !strings.Contains(got, "lawrence@cmux.com") || !strings.Contains(got, "active rec") || !strings.Contains(got, "used") {
		t.Fatalf("grid row missing state:\n%s", got)
	}
	if strings.Contains(got, "pick") {
		t.Fatalf("forced grid should not render detailed pick label:\n%s", got)
	}
	lines := strings.Split(got, "\n")
	separator := ""
	for _, line := range lines {
		if strings.Contains(line, "───") {
			separator = line
			break
		}
	}
	if separator == "" || strings.Contains(separator, "===") || strings.Contains(separator, "---") {
		t.Fatalf("grid separator should be a thin Unicode rule:\n%s", got)
	}
}

func TestUsageGridResetCellStates(t *testing.T) {
	ineligible := false
	cases := []struct {
		name string
		row  srUsageRow
		want string
	}{
		{
			name: "unknown",
			row:  srUsageRow{authMode: accounts.AuthModeOAuth, provider: accounts.ProviderCodex},
			want: "unknown",
		},
		{
			name: "available",
			row: srUsageRow{
				authMode:           accounts.AuthModeOAuth,
				provider:           accounts.ProviderCodex,
				complimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Available: true},
			},
			want: "avail",
		},
		{
			name: "consumed",
			row: srUsageRow{
				authMode:           accounts.AuthModeOAuth,
				provider:           accounts.ProviderCodex,
				complimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Consumed: true},
			},
			want: "used",
		},
		{
			name: "ineligible",
			row: srUsageRow{
				authMode:           accounts.AuthModeOAuth,
				provider:           accounts.ProviderCodex,
				complimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Eligible: &ineligible},
			},
			want: "not elig",
		},
		{
			name: "api key blank",
			row: srUsageRow{
				authMode:           accounts.AuthModeAPIKey,
				provider:           accounts.ProviderCodex,
				complimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Available: true},
			},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := usageGridResetCell(tc.row).Text; got != tc.want {
				t.Fatalf("reset cell = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDisplayUsageRowsGridGroupsProviders(t *testing.T) {
	t.Setenv("COLUMNS", "200")
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{
		{
			email:    "codex@example.com",
			planType: "pro",
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderCodex,
			score:    selectacct.Score{AccountID: "codex@example.com", Headroom: 0.9, ShortHeadroom: 0.9},
		},
		{
			email:    "claude@example.com",
			planType: "max",
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderClaude,
			score:    selectacct.Score{AccountID: "claude@example.com", Headroom: 0.8, ShortHeadroom: 0.8},
		},
	}, true)

	got := out.String()
	codexGroup := strings.Index(got, "Codex accounts")
	codexRow := strings.Index(got, "codex@example.com")
	claudeGroup := strings.Index(got, "Claude profiles")
	claudeRow := strings.Index(got, "claude@example.com")
	if codexGroup < 0 || codexRow < 0 || claudeGroup < 0 || claudeRow < 0 {
		t.Fatalf("grouped rows missing labels or accounts:\n%s", got)
	}
	if !(codexGroup < codexRow && codexRow < claudeGroup && claudeGroup < claudeRow) {
		t.Fatalf("provider grouping order is unclear:\n%s", got)
	}
}

func TestDisplayUsageRowsGridUsesClaudeLimitLabels(t *testing.T) {
	t.Setenv("COLUMNS", "160")
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{
		{
			email:    "primary@example.test",
			planType: "claude",
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderClaude,
			score:    selectacct.Score{AccountID: "lawrence@manaflow.ai", Headroom: 0.46, ShortHeadroom: 0.89, ShortResetAfterSeconds: int64((3 * time.Hour) / time.Second)},
			windows: []accounts.UsageWindow{
				{Name: "5h", UsedPercent: 11, ResetAfterSeconds: int64((3 * time.Hour) / time.Second)},
				{Name: "7d", UsedPercent: 54, ResetAfterSeconds: int64((5 * 24 * time.Hour) / time.Second)},
				{Name: "oauth-apps-weekly", UsedPercent: 100, ResetAfterSeconds: int64((5 * 24 * time.Hour) / time.Second)},
				{Name: "opus-weekly", UsedPercent: 100, ResetAfterSeconds: int64((5 * 24 * time.Hour) / time.Second)},
				{Name: "sonnet-weekly", UsedPercent: 0, ResetAfterSeconds: int64((5 * 24 * time.Hour) / time.Second)},
			},
		},
	}, false)

	got := out.String()
	if !strings.Contains(got, "Session") || !strings.Contains(got, "Weekly") || !strings.Contains(got, "Fable wk") || !strings.Contains(got, "Opus wk") || !strings.Contains(got, "Sonnet wk") {
		t.Fatalf("Claude grid missing Claude-specific labels:\n%s", got)
	}
	if strings.Contains(got, "  5h  ") || strings.Contains(got, "  7d  ") || strings.Contains(got, "Spark") {
		t.Fatalf("Claude grid should not use Codex labels:\n%s", got)
	}
	if !strings.Contains(got, "session reset") {
		t.Fatalf("Claude pick reason should use session terminology:\n%s", got)
	}
}

func TestClaudeUsageGridPrioritizesPopulatedColumnsWithoutTruncatingCore(t *testing.T) {
	t.Setenv("COLUMNS", "137")
	rows := make([]srUsageRow, 3)
	profiles := []struct {
		email        string
		plan         string
		headroom     float64
		sessionUsed  float64
		sessionReset time.Duration
		weeklyUsed   float64
		weeklyReset  time.Duration
		fableUsed    float64
		fableReset   time.Duration
		wantUse      string
		wantSession  string
		wantWeekly   string
		wantFable    string
	}{
		{
			email: "primary.account@example.test", plan: "max", headroom: .74,
			sessionReset: 4*time.Hour + 34*time.Minute, weeklyReset: 2*24*time.Hour + 6*time.Hour, fableReset: 24*time.Hour + 8*time.Hour,
			wantUse: "74% left, session reset 4h34m", wantSession: "100%/4h34m", wantWeekly: "100%/2d6h", wantFable: "100%/1d8h",
		},
		{
			email: "secondary.account@example.test", plan: "pro", headroom: .83,
			sessionUsed: 11, sessionReset: 3*time.Hour + 21*time.Minute, weeklyUsed: 12, weeklyReset: 24*time.Hour + 5*time.Hour, fableUsed: 13, fableReset: 17 * time.Hour,
			wantUse: "83% left, session reset 3h21m", wantSession: "89%/3h21m", wantWeekly: "88%/1d5h", wantFable: "87%/17h",
		},
		{
			email: "lab-profile", plan: "free", headroom: .92,
			sessionUsed: 22, sessionReset: 2*time.Hour + 8*time.Minute, weeklyUsed: 23, weeklyReset: 12 * time.Hour, fableUsed: 24, fableReset: 9 * time.Hour,
			wantUse: "92% left, session reset 2h8m", wantSession: "78%/2h8m", wantWeekly: "77%/12h", wantFable: "76%/9h",
		},
	}
	for i, profile := range profiles {
		sessionReset := int64(profile.sessionReset / time.Second)
		rows[i] = srUsageRow{
			email: profile.email, planType: profile.plan, authMode: accounts.AuthModeOAuth, provider: accounts.ProviderClaude,
			score: selectacct.Score{AccountID: profile.email, Headroom: profile.headroom, ShortHeadroom: profile.headroom, ShortResetAfterSeconds: sessionReset},
			windows: []accounts.UsageWindow{
				{Name: "5h", UsedPercent: profile.sessionUsed, ResetAfterSeconds: sessionReset},
				{Name: "7d", UsedPercent: profile.weeklyUsed, ResetAfterSeconds: int64(profile.weeklyReset / time.Second)},
				{Name: "oauth-apps-weekly", UsedPercent: profile.fableUsed, ResetAfterSeconds: int64(profile.fableReset / time.Second)},
			},
		}
	}
	var out bytes.Buffer
	displayUsageRows(&out, rows, false)
	got := out.String()
	columns := usageGridColumnsForRows(&out, false, rows)
	for _, profile := range profiles {
		var line string
		for _, candidate := range strings.Split(got, "\n") {
			if strings.Contains(candidate, profile.email) {
				line = candidate
				break
			}
		}
		wantCells := map[string]string{
			"Account":  profile.email,
			"Plan":     profile.plan,
			"Pick":     profile.wantUse,
			"Session":  profile.wantSession,
			"Weekly":   profile.wantWeekly,
			"Fable wk": profile.wantFable,
		}
		for key, want := range wantCells {
			if cell := renderedUsageGridCell(line, columns, key); cell != want {
				t.Fatalf("Claude grid row for %q rendered %s cell %q, want %q:\n%s", profile.email, key, cell, want, got)
			}
		}
	}
	for _, want := range []string{"Plan", "Session", "Weekly", "Fable wk"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Claude grid missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"Opus wk", "Sonnet wk", "Extra", "..."} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("Claude grid unexpectedly contains %q:\n%s", unwanted, got)
		}
	}
	assertUsageGridLineWidths(t, got, 137)
}

func TestClaudeUsageGridNarrowSchemaIsDeterministicAndOmitsEmptyColumns(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	rows := []srUsageRow{{
		email: "long-claude-account@example.com", planType: "max", authMode: accounts.AuthModeOAuth, provider: accounts.ProviderClaude,
		score:   selectacct.Score{AccountID: "long-claude-account@example.com", Headroom: .74, ShortHeadroom: .74, ShortResetAfterSeconds: 3600},
		windows: []accounts.UsageWindow{{Name: "5h", UsedPercent: 10, ResetAfterSeconds: 3600}, {Name: "7d", UsedPercent: 20, ResetAfterSeconds: 86400}},
	}}
	var first, second bytes.Buffer
	displayUsageRows(&first, rows, false)
	displayUsageRows(&second, rows, false)
	if first.String() != second.String() {
		t.Fatalf("narrow schema is not deterministic:\nfirst:\n%s\nsecond:\n%s", first.String(), second.String())
	}
	for _, unwanted := range []string{"Fable wk", "Opus wk", "Sonnet wk", "Extra"} {
		if strings.Contains(first.String(), unwanted) {
			t.Fatalf("narrow grid unexpectedly contains empty %q column:\n%s", unwanted, first.String())
		}
	}
	assertUsageGridLineWidths(t, first.String(), 80)

	shortRows := []srUsageRow{{
		email: "work", planType: "max", provider: accounts.ProviderClaude,
		score:   selectacct.Score{AccountID: "work", Headroom: .74, ShortHeadroom: .74, ShortResetAfterSeconds: 3600},
		windows: []accounts.UsageWindow{{Name: "5h", UsedPercent: 0, ResetAfterSeconds: 3600}, {Name: "7d", UsedPercent: 0, ResetAfterSeconds: 86400}},
	}}
	var shortOut bytes.Buffer
	displayUsageRows(&shortOut, shortRows, false)
	for _, want := range []string{"74% left, session reset 1h", "100%/1h", "100%/1d"} {
		if !strings.Contains(shortOut.String(), want) {
			t.Fatalf("narrow short-account grid truncated %q:\n%s", want, shortOut.String())
		}
	}
	assertUsageGridLineWidths(t, shortOut.String(), 80)

	var worstOut bytes.Buffer
	displayUsageRows(&worstOut, []srUsageRow{{
		email: strings.Repeat("a", 36), planType: "enterprise", provider: accounts.ProviderClaude,
		active: true, err: errors.New("usage unavailable"),
	}}, false)
	if !strings.Contains(worstOut.String(), "enterprise") || !strings.Contains(worstOut.String(), "usage unavailable") {
		t.Fatalf("narrow worst-case grid lost higher-priority values:\n%s", worstOut.String())
	}
	assertUsageGridLineWidths(t, worstOut.String(), 80)
}

func TestClaudeUsageGridDropsLowerPriorityPopulatedWindowsBeforeTruncatingCore(t *testing.T) {
	t.Setenv("COLUMNS", "137")
	reset := int64((4*time.Hour + 34*time.Minute) / time.Second)
	row := srUsageRow{
		email: "claude@example.com", planType: "max", provider: accounts.ProviderClaude,
		score: selectacct.Score{AccountID: "claude@example.com", Headroom: .74, ShortHeadroom: .74, ShortResetAfterSeconds: reset},
		windows: []accounts.UsageWindow{
			{Name: "5h", UsedPercent: 0, ResetAfterSeconds: reset},
			{Name: "7d", UsedPercent: 0, ResetAfterSeconds: 86400},
			{Name: "oauth-apps-weekly", UsedPercent: 0, ResetAfterSeconds: 86400},
			{Name: "opus-weekly", UsedPercent: 0, ResetAfterSeconds: 86400},
			{Name: "sonnet-weekly", UsedPercent: 0, ResetAfterSeconds: 86400},
			{Name: "extra", UsedPercent: 0, ResetAfterSeconds: 86400},
		},
	}
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{row}, false)
	got := out.String()
	for _, want := range []string{"74% left, session reset 4h34m", "100%/4h34m", "100%/1d", "Session", "Weekly", "Fable wk"} {
		if !strings.Contains(got, want) {
			t.Fatalf("all-window Claude grid truncated priority value %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "...") {
		t.Fatalf("all-window Claude grid truncated a retained value:\n%s", got)
	}
	assertUsageGridLineWidths(t, got, 137)
}

func TestClaudeUsageGridUsesOneSchemaForMixedRows(t *testing.T) {
	t.Setenv("COLUMNS", "137")
	rows := []srUsageRow{
		{email: "session@example.com", planType: "pro", provider: accounts.ProviderClaude, windows: []accounts.UsageWindow{{Name: "5h", UsedPercent: 10, ResetAfterSeconds: 3600}}},
		{email: "fable@example.com", planType: "free", provider: accounts.ProviderClaude, windows: []accounts.UsageWindow{{Name: "oauth-apps-weekly", UsedPercent: 20, ResetAfterSeconds: 86400}}},
	}
	var out bytes.Buffer
	displayUsageRows(&out, rows, false)
	got := out.String()
	if strings.Count(got, "Session") != 1 || strings.Count(got, "Fable wk") != 1 {
		t.Fatalf("mixed Claude rows did not share one populated schema:\n%s", got)
	}
	assertUsageGridLineWidths(t, got, 137)
}

func TestClaudeUsageGridFitsUnicodeAccountsByDisplayWidth(t *testing.T) {
	row := srUsageRow{
		email: "工程师-👩🏽‍💻-équipe@example.com", planType: "max", provider: accounts.ProviderClaude,
		score: selectacct.Score{AccountID: "unicode", Headroom: .74, ShortHeadroom: .74, ShortResetAfterSeconds: 3600},
		windows: []accounts.UsageWindow{
			{Name: "5h", UsedPercent: 0, ResetAfterSeconds: 3600},
			{Name: "7d", UsedPercent: 0, ResetAfterSeconds: 86400},
			{Name: "oauth-apps-weekly", UsedPercent: 0, ResetAfterSeconds: 86400},
		},
	}
	for _, width := range []string{"137", "80"} {
		t.Run(width, func(t *testing.T) {
			t.Setenv("COLUMNS", width)
			var out bytes.Buffer
			displayUsageRows(&out, []srUsageRow{row}, false)
			got := out.String()
			if !utf8.ValidString(got) {
				t.Fatalf("grid split a UTF-8 sequence: %q", got)
			}
			assertUsageGridLineWidths(t, got, mustAtoi(t, width))
		})
	}
}

func TestFitCellPreservesUnicodeGraphemesAndExactDisplayWidth(t *testing.T) {
	for _, tc := range []struct {
		value string
		width int
		want  string
	}{
		{value: "👩🏽‍💻abcdef", width: 5, want: "👩🏽‍💻..."},
		{value: "界x", width: 1, want: " "},
		{value: "界界界", width: 5, want: "界..."},
	} {
		got := fitCell(tc.value, tc.width)
		if got != tc.want {
			t.Fatalf("fitCell(%q, %d) = %q, want %q", tc.value, tc.width, got, tc.want)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("fitCell(%q, %d) returned invalid UTF-8: %q", tc.value, tc.width, got)
		}
		if gotWidth := runewidth.StringWidth(got); gotWidth != tc.width {
			t.Fatalf("fitCell(%q, %d) display width = %d, want %d", tc.value, tc.width, gotWidth, tc.width)
		}
	}
}

func mustAtoi(t *testing.T, value string) int {
	t.Helper()
	parsed, err := strconv.Atoi(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func assertUsageGridLineWidths(t *testing.T, output string, width int) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		if lineWidth := runewidth.StringWidth(line); lineWidth > width {
			t.Fatalf("line width = %d, want <= %d:\n%s", lineWidth, width, line)
		}
	}
}

func renderedUsageGridCell(line string, columns []usageGridColumn, key string) string {
	offset := 0
	for _, column := range columns {
		if column.Key == key {
			prefix := runewidth.Truncate(line, offset, "")
			return strings.TrimSpace(runewidth.Truncate(line[len(prefix):], column.Width, ""))
		}
		offset += column.Width + 2
	}
	return ""
}

func TestClaudeUsageWindowsIncludeOAuthAppsWeekly(t *testing.T) {
	reset := time.Now().Add(4 * 24 * time.Hour).Format(time.RFC3339)
	windows := claudeUsageWindows(&agentclaude.UsageResponse{
		FiveHour:          &agentclaude.RateLimit{Utilization: srFloatPtr(0), ResetsAt: time.Now().Add(time.Hour).Format(time.RFC3339)},
		SevenDay:          &agentclaude.RateLimit{Utilization: srFloatPtr(60), ResetsAt: reset},
		SevenDayOAuthApps: &agentclaude.RateLimit{Utilization: srFloatPtr(100), ResetsAt: reset},
	})

	var oauthApps *accounts.UsageWindow
	for i := range windows {
		if windows[i].Name == "oauth-apps-weekly" {
			oauthApps = &windows[i]
			break
		}
	}
	if oauthApps == nil {
		t.Fatalf("missing oauth-apps-weekly window: %+v", windows)
	}
	if oauthApps.LimitWindowSeconds != int64((7*24*time.Hour)/time.Second) {
		t.Fatalf("oauth apps LimitWindowSeconds = %d, want 7d", oauthApps.LimitWindowSeconds)
	}
	if oauthApps.Feature != agentclaude.FableFeature {
		t.Fatalf("oauth apps Feature = %q, want %q", oauthApps.Feature, agentclaude.FableFeature)
	}
	score := scoreFromWindows("claude@example.com", windows)
	if score.Headroom == 0 {
		t.Fatalf("base headroom = %.2f, hidden Fable bucket should not exhaust non-Fable Claude models", score.Headroom)
	}
	fableScore, ok := score.ModelScores[selectacct.ModelKey(agentclaude.FableFeature)]
	if !ok {
		t.Fatalf("missing Fable model score: %+v", score.ModelScores)
	}
	if fableScore.Headroom != 0 {
		t.Fatalf("Fable headroom = %.2f, want 0 from saturated oauth app weekly bucket", fableScore.Headroom)
	}
	// A saturated Fable (per-model) pool must NOT cook the account: Opus/Sonnet
	// and the base weekly quota remain usable. The Use column instead notes that
	// Fable specifically is out.
	cooked, reason := cookedFromWindows(windows)
	if cooked {
		t.Fatalf("account should not be cooked when only the Fable pool is exhausted (reason=%q)", reason)
	}
	if suffix := exhaustedModelSuffix(windows); !strings.Contains(suffix, "Fable") {
		t.Fatalf("Use suffix = %q, want it to note Fable is out", suffix)
	}
}

func srFloatPtr(v float64) *float64 { return &v }

func TestDisplayUsageRowsGridCompactsForNarrowTerminals(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{
		{
			email:          "test@example.invalid",
			planType:       "pro",
			gtoRecommended: true,
			authMode:       accounts.AuthModeOAuth,
			provider:       accounts.ProviderCodex,
			score:          selectacct.Score{AccountID: "test@example.invalid", Headroom: 0.67, ShortHeadroom: 0.96, ShortResetAfterSeconds: int64(time.Minute / time.Second)},
			windows: []accounts.UsageWindow{
				{Name: "primary", UsedPercent: 4, LimitWindowSeconds: int64((5 * time.Hour) / time.Second), ResetAfterSeconds: int64(time.Minute / time.Second)},
				{Name: "secondary", UsedPercent: 33, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAfterSeconds: int64((5 * 24 * time.Hour) / time.Second)},
				{Name: "GPT-5.3-Codex-Spark/primary", UsedPercent: 0, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)},
				{Name: "GPT-5.3-Codex-Spark/secondary", UsedPercent: 0, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
			},
			credits: &accounts.CreditsInfo{Balance: "0"},
		},
		{
			email:    "austin@manaflow.ai",
			planType: "claude",
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderClaude,
			score:    selectacct.Score{AccountID: "austin@manaflow.ai", Headroom: 0.95, ShortHeadroom: 0.95},
			windows: []accounts.UsageWindow{
				{Name: "5h", UsedPercent: 5, ResetAfterSeconds: int64((4 * time.Hour) / time.Second)},
				{Name: "7d", UsedPercent: 1, ResetAfterSeconds: int64((2 * 24 * time.Hour) / time.Second)},
			},
		},
	}, false)

	got := out.String()
	if strings.Contains(got, "Spark") || strings.Contains(got, "Spark wk") {
		t.Fatalf("narrow grid should omit Spark columns:\n%s", got)
	}
	if strings.HasPrefix(strings.TrimLeft(got, "\n"), "#") {
		t.Fatalf("non-numbered grid should not render blank # column:\n%s", got)
	}
	if !strings.Contains(got, "Use") || !strings.Contains(got, "Claude profiles") {
		t.Fatalf("narrow grid missing core columns or groups:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if line == "" {
			continue
		}
		if width := runewidth.StringWidth(line); width > 80 {
			t.Fatalf("line width = %d, want <= 80:\n%s\nfull output:\n%s", width, line, got)
		}
	}
}

func TestDisplayUsageRowsGridColorsWhenForced(t *testing.T) {
	t.Setenv("SR_USAGE_GRID", "1")
	t.Setenv("FORCE_COLOR", "1")
	t.Setenv("COLUMNS", "200")
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{
		{
			email:          "ok@example.com",
			gtoRecommended: true,
			gtoReason:      "90% bottleneck left",
			authMode:       accounts.AuthModeOAuth,
			score:          selectacct.Score{AccountID: "ok@example.com", Headroom: 0.9, ShortHeadroom: 0.9},
			windows:        []accounts.UsageWindow{{Name: "primary", UsedPercent: 10, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)}},
		},
		{
			email:            "temp@example.com",
			gtoReason:        "temporarily cooked, cannot start new session",
			authMode:         accounts.AuthModeOAuth,
			tempCooked:       true,
			tempCookedReason: "5h limit fully consumed",
			windows:          []accounts.UsageWindow{{Name: "primary", UsedPercent: 100, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)}},
		},
		{
			email:        "cooked@example.com",
			gtoReason:    "cooked, cannot switch",
			authMode:     accounts.AuthModeOAuth,
			cooked:       true,
			windows:      []accounts.UsageWindow{{Name: "secondary", UsedPercent: 100, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)}},
			planType:     "pro",
			cookedReason: "7d limit fully consumed",
		},
	}, true)

	got := out.String()
	for _, code := range []string{ansiGreen, ansiYellow, ansiRed, ansiBold + ansiWhite, ansiDim, ansiBGRowAlt} {
		if !strings.Contains(got, code) {
			t.Fatalf("grid output missing color %q:\n%q", code, got)
		}
	}
	if strings.Contains(got, "\x1b[32mok@example.com") {
		t.Fatalf("account column should keep account styling separate from state colors:\n%q", got)
	}
}

func TestUsageGridSuppressesShortWindowsByQuotaFamily(t *testing.T) {
	windows := []accounts.UsageWindow{
		{Name: "primary", UsedPercent: 10, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)},
		{Name: "secondary", UsedPercent: 100, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
		{Name: "GPT-5.3-Codex-Spark/primary", UsedPercent: 1, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)},
		{Name: "GPT-5.3-Codex-Spark/secondary", UsedPercent: 2, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
	}
	cooked := srUsageRow{cooked: true, windows: windows}
	if cell := usageGridShortWindowCell(cooked); cell.Text != "" {
		t.Fatalf("short window cell = %q, want blank when general weekly quota is cooked", cell.Text)
	}
	if cell := usageGridShortNamedWindowCell(cooked); cell.Text == "" {
		t.Fatal("Spark short window should remain visible when only general weekly quota is cooked")
	}

	tempCooked := srUsageRow{tempCooked: true, windows: []accounts.UsageWindow{
		{Name: "primary", UsedPercent: 100, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)},
		{Name: "secondary", UsedPercent: 2, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
		{Name: "GPT-5.3-Codex-Spark/primary", UsedPercent: 100, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)},
		{Name: "GPT-5.3-Codex-Spark/secondary", UsedPercent: 2, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
	}}
	if cell := usageGridShortWindowCell(tempCooked); cell.Text == "" {
		t.Fatal("temporarily cooked row should still show the short window")
	}
	if cell := usageGridShortNamedWindowCell(tempCooked); cell.Text == "" {
		t.Fatal("temporarily cooked row should still show the short named window")
	}

	sparkWeeklyCooked := srUsageRow{cooked: true, windows: []accounts.UsageWindow{
		{Name: "primary", UsedPercent: 10, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)},
		{Name: "secondary", UsedPercent: 2, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
		{Name: "GPT-5.3-Codex-Spark/primary", UsedPercent: 1, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)},
		{Name: "GPT-5.3-Codex-Spark/secondary", UsedPercent: 100, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
	}}
	if cell := usageGridShortWindowCell(sparkWeeklyCooked); cell.Text == "" {
		t.Fatal("general short window should remain visible when only Spark weekly quota is cooked")
	}
	if cell := usageGridShortNamedWindowCell(sparkWeeklyCooked); cell.Text != "" {
		t.Fatalf("Spark short window cell = %q, want blank when Spark weekly quota is cooked", cell.Text)
	}
}

func TestDisplayUsageRowsIgnoresDetailedModeEnv(t *testing.T) {
	t.Setenv("SR_USAGE_GRID", "0")
	t.Setenv("CX_USAGE_GRID", "0")
	t.Setenv("COLUMNS", "200")
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{
		{
			email:     "a@example.com",
			gtoReason: "80% bottleneck left",
			authMode:  accounts.AuthModeOAuth,
		},
	}, true)

	got := out.String()
	if !strings.Contains(got, "Account") || !strings.Contains(got, "a@example.com") {
		t.Fatalf("grid output missing expected row:\n%s", got)
	}
	if strings.Contains(got, "1) a@example.com") || strings.Contains(got, "pick:") {
		t.Fatalf("detailed output should never be rendered:\n%s", got)
	}
}

func TestParseSRSwitchArgs(t *testing.T) {
	selector, opts, err := parseSRSwitchArgs([]string{"a@example.com", "--restart-gui"}, srSwitchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if selector != "a@example.com" {
		t.Fatalf("selector = %q", selector)
	}
	if !opts.restartCodexGUI {
		t.Fatal("restartCodexGUI should be true")
	}
}

func readJSONFile(t *testing.T, path string, out any) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatal(err)
	}
}

func testJWT(claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func testCodexAuth(email, accountID string) accounts.CodexAuthFile {
	access := testJWT(map[string]any{
		"exp": time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
		},
		"https://api.openai.com/profile": map[string]any{
			"email": email,
		},
	})
	id := testJWT(map[string]any{
		"exp":   time.Now().Add(time.Hour).Unix(),
		"email": email,
	})
	return accounts.CodexAuthFile{AuthMode: "chatgpt", Tokens: &accounts.CodexTokens{
		AccessToken:  access,
		RefreshToken: "refresh-" + accountID,
		IDToken:      id,
	}}
}

func installFakeCodexLogin(t *testing.T, home string, auth accounts.CodexAuthFile) {
	t.Helper()
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"test \"$1\" = login || exit 2\n" +
		"codex_home=${CODEX_HOME:-$HOME/.codex}\n" +
		"mkdir -p \"$codex_home\"\n" +
		"cat > \"$codex_home/auth.json\" <<'JSON'\n" +
		string(body) + "\nJSON\n"
	path := filepath.Join(binDir, "codex")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

type srRoundTripFunc func(*http.Request) (*http.Response, error)

func (f srRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func usageResponse(usedPercent float64) *http.Response {
	return usageResponseWindows(usedPercent, 0)
}

func usageResponseWindows(primaryUsedPercent, secondaryUsedPercent float64) *http.Response {
	body, _ := json.Marshal(map[string]any{
		"plan_type": "pro",
		"rate_limit": map[string]any{
			"primary_window": map[string]any{
				"used_percent":         primaryUsedPercent,
				"limit_window_seconds": int64((5 * time.Hour) / time.Second),
				"reset_after_seconds":  int64((3 * time.Hour) / time.Second),
			},
			"secondary_window": map[string]any{
				"used_percent":         secondaryUsedPercent,
				"limit_window_seconds": int64((7 * 24 * time.Hour) / time.Second),
				"reset_after_seconds":  int64((6 * 24 * time.Hour) / time.Second),
			},
		},
	})
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

func TestUsageErrorFootnoteShowsProviderAndReaddCommand(t *testing.T) {
	t.Setenv("COLUMNS", "200")
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{
		{
			email:    "codex-broken@example.com",
			authMode: accounts.AuthModeOAuth,
			err:      errors.New("usage fetch failed: 401 Unauthorized"),
		},
		{
			email:    "claude-broken@example.com",
			provider: accounts.ProviderClaude,
			err:      errors.New("Claude OAuth refresh failed: 400 Bad Request: invalid_grant"),
		},
		{
			email:    "codex-flaky@example.com",
			authMode: accounts.AuthModeOAuth,
			err:      errors.New("usage fetch failed: connection refused"),
		},
	}, false)
	text := out.String()
	if !strings.Contains(text, "codex-broken@example.com [codex]: usage fetch failed: 401 Unauthorized (re-add with: sr add)") {
		t.Fatalf("codex 401 footnote missing provider/re-add hint:\n%s", text)
	}
	if !strings.Contains(text, "claude-broken@example.com [claude]: Claude OAuth refresh failed: 400 Bad Request: invalid_grant (re-add with: sr claude add)") {
		t.Fatalf("claude invalid_grant footnote missing provider/re-add hint:\n%s", text)
	}
	if strings.Contains(text, "codex-flaky@example.com [codex]: usage fetch failed: connection refused (re-add") {
		t.Fatalf("transient error should not suggest re-add:\n%s", text)
	}
	if !strings.Contains(text, "codex-flaky@example.com [codex]: usage fetch failed: connection refused") {
		t.Fatalf("transient error footnote should still show provider:\n%s", text)
	}
}

// A usage row must be filed under the provider its key actually reaches.
// Hardcoding Codex listed every Qwen, Grok, and OpenRouter key under
// "Codex accounts", which misreports what the account is for.
func TestUsageRowsGroupByTheirOwnProvider(t *testing.T) {
	cases := []struct {
		provider  accounts.Provider
		wantLabel string
		wantPlan  string
	}{
		{provider: accounts.ProviderCodex, wantLabel: "Codex accounts", wantPlan: "api key"},
		{provider: "", wantLabel: "Codex accounts", wantPlan: "api key"},
		{provider: accounts.ProviderClaude, wantLabel: "Claude profiles", wantPlan: "claude key"},
		{provider: accounts.ProviderQwenToken, wantLabel: "Qwen accounts", wantPlan: "qwen-token key"},
		{provider: accounts.ProviderQwenAnthropic, wantLabel: "Qwen-anthropic accounts", wantPlan: "qwen-anthropic key"},
		{provider: accounts.ProviderGrok, wantLabel: "Grok accounts", wantPlan: "grok key"},
	}
	for _, tc := range cases {
		t.Run(string(tc.provider), func(t *testing.T) {
			row := srUsageRow{email: "acct", provider: tc.provider}
			if got := usageProviderLabel(row); got != tc.wantLabel {
				t.Fatalf("usageProviderLabel = %q, want %q", got, tc.wantLabel)
			}
			if got := apiKeyPlanLabel(tc.provider); got != tc.wantPlan {
				t.Fatalf("apiKeyPlanLabel = %q, want %q", got, tc.wantPlan)
			}
		})
	}

	// Codex and Claude keep their positions; everything else sorts after them
	// rather than interleaving.
	codex := usageProviderOrder(srUsageRow{provider: accounts.ProviderCodex})
	claude := usageProviderOrder(srUsageRow{provider: accounts.ProviderClaude})
	other := usageProviderOrder(srUsageRow{provider: accounts.ProviderQwenToken})
	if !(codex < claude && claude < other) {
		t.Fatalf("ordering = codex %d, claude %d, other %d; want codex first", codex, claude, other)
	}
}

func TestRankUsageRowsKeepsProviderHeadingsContiguous(t *testing.T) {
	rows := []srUsageRow{
		{email: "qwen-token:z", provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey},
		{email: "kimi:z-api", provider: accounts.ProviderKimi, authMode: accounts.AuthModeAPIKey},
		{email: "qwen-token:a", provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey},
		{email: "kimi:a-oauth", provider: accounts.ProviderKimi, authMode: accounts.AuthModeOAuth},
	}
	rankUsageRows(rows)
	got := make([]string, 0, len(rows))
	for _, row := range rows {
		got = append(got, usageProviderLabel(row))
	}
	want := []string{
		"Kimi API-key accounts",
		"Kimi subscription accounts",
		"Qwen accounts",
		"Qwen accounts",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("provider groups = %v, want %v", got, want)
	}
}

// The Codex windows and the scheduler's "Use" advice mean nothing for a vendor
// that publishes no quota, so a keyed-provider section gets its own columns.
func TestKeyedProviderSectionUsesItsOwnColumns(t *testing.T) {
	var out bytes.Buffer
	keyed := usageGridColumns(&out, false, srUsageRow{provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey, showShortWindow: true, showLongWindow: true})
	keys := map[string]int{}
	for _, column := range keyed {
		keys[column.Key] = column.Width
	}
	for _, want := range []string{"Account", "Plan", "State", "Pick", "5h", "7d"} {
		if _, ok := keys[want]; !ok {
			t.Fatalf("keyed section is missing the %q column: %v", want, keys)
		}
	}
	for _, unwanted := range []string{"Key ID", "Sessions", "Login", "Models", "Endpoints", "Reset", "Credits"} {
		if _, ok := keys[unwanted]; ok {
			t.Fatalf("keyed section should not carry the %q column", unwanted)
		}
	}
	// Codex keeps its own columns untouched.
	codex := usageGridColumns(&out, false, srUsageRow{provider: accounts.ProviderCodex})
	codexKeys := map[string]bool{}
	for _, column := range codex {
		codexKeys[column.Key] = true
	}
	for _, want := range []string{"Pick", "5h", "7d"} {
		if !codexKeys[want] {
			t.Fatalf("codex section lost its %q column", want)
		}
	}
	if codexKeys["Endpoints"] || codexKeys["Quota"] {
		t.Fatal("codex section must not gain the keyed-provider columns")
	}

	// A standalone sr process does not have serve's declared registry, but the
	// stored provider still identifies this as an API-key section. It must not
	// fall back to Codex quota and switch columns merely because metering is
	// unknown locally.
	custom := usageGridColumns(&out, false, srUsageRow{
		provider: accounts.Provider("acme-relay"), authMode: accounts.AuthModeAPIKey,
	})
	customKeys := map[string]bool{}
	for _, column := range custom {
		customKeys[column.Key] = true
	}
	if customKeys["Endpoints"] || customKeys["Quota"] || customKeys["5h"] || customKeys["7d"] {
		t.Fatalf("declared-provider columns = %v, want compact provider layout", customKeys)
	}
}

func TestKimiSubscriptionSectionShowsIndependentQuotaWindows(t *testing.T) {
	t.Setenv("COLUMNS", "120")
	var out bytes.Buffer
	columns := usageGridColumns(&out, false, srUsageRow{provider: accounts.ProviderKimi, authMode: accounts.AuthModeOAuth})
	keys := map[string]bool{}
	for _, column := range columns {
		keys[column.Key] = true
		if column.Key == "5h" && column.Width < len("100%/12h34m") {
			t.Fatalf("Kimi 5h width = %d, want an untruncated reset value", column.Width)
		}
	}
	for _, want := range []string{"Account", "State", "5h", "Weekly"} {
		if !keys[want] {
			t.Fatalf("Kimi section is missing %q: %+v", want, columns)
		}
	}
	for _, unwanted := range []string{"Plan", "Pick", "Models", "Endpoints", "Quota", "7d"} {
		if keys[unwanted] {
			t.Fatalf("Kimi subscription section should not carry %q", unwanted)
		}
	}

	displayUsageRows(&out, []srUsageRow{{
		email:    "kimi-code",
		provider: accounts.ProviderKimi,
		authMode: accounts.AuthModeOAuth,
		active:   true,
		planType: "subscription",
		windows: []accounts.UsageWindow{
			{Name: "5h", UsedPercent: 25, LimitWindowSeconds: int64((5 * time.Hour) / time.Second), ResetAfterSeconds: 3600},
			{Name: "weekly", UsedPercent: 40, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAfterSeconds: 2 * 86400},
		},
	}}, false)
	text := out.String()
	for _, want := range []string{"Kimi subscription accounts", "kimi-code", "75%/1h", "60%/2d"} {
		if !strings.Contains(text, want) {
			t.Fatalf("Kimi status is missing %q:\n%s", want, text)
		}
	}
}

func TestKimiSubscriptionStateDistinguishesReadyRecommendedAndActive(t *testing.T) {
	ready := srUsageRow{
		provider: accounts.ProviderKimi, authMode: accounts.AuthModeOAuth,
		sessionsKnown: true, score: selectacct.Score{Headroom: 1, ShortHeadroom: 1},
	}
	if got := usageGridState(ready); got != "ready" {
		t.Fatalf("ready Kimi state = %q", got)
	}
	ready.gtoRecommended = true
	if got := usageGridState(ready); got != "rec" {
		t.Fatalf("recommended Kimi state = %q", got)
	}
	ready.active = true
	ready.assignedSessions = 1
	if got := usageGridState(ready); got != "active, rec" {
		t.Fatalf("active recommended Kimi state = %q", got)
	}
}

func TestKimiAPIKeyShowsQuotaWindowsOnlyWhenAvailable(t *testing.T) {
	withQuota := srUsageRow{
		provider: accounts.ProviderKimi, authMode: accounts.AuthModeAPIKey,
		quotaUsageKnown: true,
	}
	keys := map[string]bool{}
	for _, column := range usageGridColumns(&bytes.Buffer{}, false, withQuota) {
		keys[column.Key] = true
	}
	if !keys["5h"] || !keys["Weekly"] {
		t.Fatalf("Kimi subscription key columns = %v", keys)
	}
	withoutQuota := withQuota
	withoutQuota.quotaUsageKnown = false
	keys = map[string]bool{}
	for _, column := range usageGridColumns(&bytes.Buffer{}, false, withoutQuota) {
		keys[column.Key] = true
	}
	if keys["5h"] || keys["Weekly"] {
		t.Fatalf("Kimi key without quota exposed empty windows: %v", keys)
	}
}

func TestQwenTokenPlanNamesAndQuotaWindowsDoNotTruncate(t *testing.T) {
	t.Setenv("COLUMNS", "120")
	row := srUsageRow{
		provider:        accounts.ProviderQwenToken,
		authMode:        accounts.AuthModeAPIKey,
		planType:        "Standard",
		showShortWindow: true,
		showLongWindow:  true,
	}
	if got := usageGridPlan(row); got != "Token Plan Standard" {
		t.Fatalf("usageGridPlan = %q, want Token Plan Standard", got)
	}
	for _, column := range usageGridColumns(&bytes.Buffer{}, false, row) {
		switch column.Key {
		case "Plan":
			if column.Width < len("Token Plan Standard") {
				t.Fatalf("Qwen plan width = %d, want untruncated Token Plan Standard", column.Width)
			}
		case "5h", "7d":
			if column.Width < len("100%/12h34m") {
				t.Fatalf("Qwen %s width = %d, want an untruncated reset value", column.Key, column.Width)
			}
		}
	}
}

func TestAntigravityStatusUsesAuthAndSessionTruthWithoutFakeQuota(t *testing.T) {
	t.Setenv("COLUMNS", "120")
	statuses := []remoteServerUsageStatus{
		{
			ID: "antigravity", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth,
			AccountIdentity: "router agy login", PlanType: "subscription", AuthChecked: true, AuthValid: true,
			SessionsKnown: true,
		},
		{
			ID: "antigravity-active", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth,
			AccountIdentity: "second agy login", PlanType: "subscription", AuthChecked: true, AuthValid: true,
			AssignedSessions: 1, SessionsKnown: true,
		},
	}
	rows := usageRowsFromServerUsageStatuses(statuses)
	if len(rows) != 2 || usageGridState(rows[0]) != "ready" || usageGridState(rows[1]) != "active" {
		t.Fatalf("Antigravity states = %+v", rows)
	}
	var out bytes.Buffer
	displayUsageRows(&out, rows, false)
	text := out.String()
	for _, want := range []string{"router agy login", "second agy login", "subscription", "ready", "active", "quota not exposed"} {
		if !strings.Contains(text, want) {
			t.Fatalf("Antigravity status should show %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"subsc...", "100%", "5h", "7d"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("Antigravity status should not show %q:\n%s", unwanted, text)
		}
	}
}

func TestAntigravityStatusPreservesIndependentFamilyQuotaAtRealisticWidth(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	statuses := []remoteServerUsageStatus{{
		ID: "antigravity-subscription:work", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth,
		AccountIdentity: "verified@example.com", PlanType: "Google AI Pro", AuthChecked: true, AuthValid: true,
		QuotaUsageKnown: true, Windows: []accounts.UsageWindow{
			{Name: "gemini 5h", Feature: "gemini", UsedPercent: 25, LimitWindowSeconds: 18000, ResetAfterSeconds: 3600},
			{Name: "gemini weekly", Feature: "gemini", UsedPercent: 60, LimitWindowSeconds: 604800, ResetAfterSeconds: 172800},
			{Name: "claude-gpt 5h", Feature: "claude-gpt", UsedPercent: 100, LimitWindowSeconds: 18000, ResetAfterSeconds: 1800},
			{Name: "claude-gpt weekly", Feature: "claude-gpt", UsedPercent: 10, LimitWindowSeconds: 604800, ResetAfterSeconds: 432000},
		},
	}}
	rows := usageRowsFromServerUsageStatuses(statuses)
	columns := usageGridColumnsForRows(&bytes.Buffer{}, false, rows)
	keys := map[string]bool{}
	for _, column := range columns {
		keys[column.Key] = true
	}
	for _, key := range []string{"AG Gemini 5h", "AG Gemini wk", "AG 3P 5h", "AG 3P wk"} {
		if !keys[key] {
			t.Fatalf("columns = %+v, missing %s", columns, key)
		}
	}
	if keys["Pick"] {
		t.Fatalf("columns = %+v, constrained four-lane layout should drop Use", columns)
	}
	var out bytes.Buffer
	displayUsageRows(&out, rows, false)
	got := out.String()
	for _, want := range []string{
		"verified@example.com", "Google AI Pro", "G 5h", "G wk", "C/G 5h", "C/G wk",
		"75%/1h", "40%/2d", "0%/30m", "90%/5d",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Antigravity status missing %q:\n%s", want, got)
		}
	}
	assertUsageGridLineWidths(t, got, 100)
}

func TestAntigravityStatusHidesUnavailableFamilyColumns(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	row := srUsageRow{
		provider: accounts.ProviderAntigravity, authMode: accounts.AuthModeOAuth,
		accountIdentity: "verified@example.com", planType: "Paid", authChecked: true, authValid: true,
		quotaUsageKnown: true,
		windows:         []accounts.UsageWindow{{Name: "gemini 5h", Feature: "gemini", UsedPercent: 20, LimitWindowSeconds: 18000}},
	}
	columns := usageGridColumns(&bytes.Buffer{}, false, row)
	keys := map[string]bool{}
	for _, column := range columns {
		keys[column.Key] = true
	}
	if !keys["AG Gemini 5h"] {
		t.Fatalf("columns = %+v, missing available Gemini 5h", columns)
	}
	for _, unavailable := range []string{"AG Gemini wk", "AG 3P 5h", "AG 3P wk"} {
		if keys[unavailable] {
			t.Fatalf("columns = %+v, rendered unavailable %s", columns, unavailable)
		}
	}
}

func TestAntigravityLegacyModelQuotaUseDoesNotClaimBaseHundredPercent(t *testing.T) {
	row := srUsageRow{
		provider: accounts.ProviderAntigravity, authMode: accounts.AuthModeOAuth,
		quotaUsageKnown: true,
		windows: []accounts.UsageWindow{
			{Name: "claude-sonnet-4.5", Feature: "claude-sonnet-4.5", UsedPercent: 70, ResetAfterSeconds: 3600},
			{Name: "claude-opus-4.1", Feature: "claude-opus-4.1", UsedPercent: 20},
		},
	}
	if got := compactPickReason(row); got != "30% left Sonnet/1h" {
		t.Fatalf("legacy compact Use = %q", got)
	}
}

func TestAntigravityUnknownModelQuotaDoesNotPrintPlaceholderLabel(t *testing.T) {
	row := srUsageRow{
		provider: accounts.ProviderAntigravity, authMode: accounts.AuthModeOAuth,
		quotaUsageKnown: true,
		windows:         []accounts.UsageWindow{{Name: "model", UsedPercent: 0, ResetAfterSeconds: 604800}},
	}
	if got := compactPickReason(row); got != "100% left/7d" {
		t.Fatalf("generic compact Use = %q", got)
	}
}

func TestAntigravityFamilyColumnsOmitRedundantUseAtWideWidth(t *testing.T) {
	t.Setenv("COLUMNS", "180")
	row := srUsageRow{
		provider: accounts.ProviderAntigravity, authMode: accounts.AuthModeOAuth,
		quotaUsageKnown: true,
		windows: []accounts.UsageWindow{
			{Name: "gemini-3.1-pro", Feature: "gemini-3.1-pro", UsedPercent: 1, ResetAfterSeconds: 604800},
			{Name: "claude-opus-4.1", Feature: "claude-opus-4.1", UsedPercent: 3, ResetAfterSeconds: 604800},
		},
	}
	for _, column := range usageGridColumns(&bytes.Buffer{}, false, row) {
		if column.Key == "Pick" {
			t.Fatal("wide Antigravity family table retained redundant Use column")
		}
	}
}

func TestAntigravityLegacyModelQuotaProducesCadenceNeutralFamilyColumns(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	row := srUsageRow{
		provider: accounts.ProviderAntigravity, authMode: accounts.AuthModeOAuth,
		accountIdentity: "verified@example.com", planType: "Starter", authChecked: true, authValid: true,
		quotaUsageKnown: true,
		windows: []accounts.UsageWindow{
			{Name: "gemini-3.1-pro-high", Feature: "gemini-3.1-pro-high", UsedPercent: 0, ResetAfterSeconds: 604800},
			{Name: "gemini-3.1-pro-low", Feature: "gemini-3.1-pro-low", UsedPercent: 25, ResetAfterSeconds: 432000},
			{Name: "openai-o3", Feature: "openai-o3", UsedPercent: 20, ResetAfterSeconds: 432000},
		},
	}
	columns := usageGridColumns(&bytes.Buffer{}, false, row)
	keys := map[string]bool{}
	for _, column := range columns {
		keys[column.Key] = true
	}
	if !keys["AG Gemini model"] || !keys["AG 3P model"] || keys["AG Gemini wk"] || keys["AG 3P wk"] || keys["AG Gemini 5h"] || keys["AG 3P 5h"] {
		t.Fatalf("legacy Antigravity columns = %+v", columns)
	}
	if keys["Pick"] {
		t.Fatalf("legacy Antigravity columns = %+v, redundant Use should be omitted", columns)
	}
	if got := usageGridValues(row, "")["AG Gemini model"].Text; got != "75%/5d" {
		t.Fatalf("Gemini model family = %q, want most constrained quota", got)
	}
}

func TestAntigravityStatusSurfacesFailedAuth(t *testing.T) {
	rows := usageRowsFromServerUsageStatuses([]remoteServerUsageStatus{{
		ID: "antigravity", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth,
		AuthChecked: true, AuthValid: false, Error: "refresh rejected",
	}})
	if len(rows) != 1 || !rows[0].authChecked || rows[0].authValid || usageGridState(rows[0]) != "error" {
		t.Fatalf("failed Antigravity row = %+v", rows)
	}
}

func TestKimiAPIKeySectionDoesNotClaimSubscriptionQuota(t *testing.T) {
	columns := usageGridColumns(&bytes.Buffer{}, false, srUsageRow{provider: accounts.ProviderKimi, authMode: accounts.AuthModeAPIKey})
	keys := map[string]bool{}
	for _, column := range columns {
		keys[column.Key] = true
	}
	if keys["5h"] || keys["Weekly"] || keys["Quota"] || !keys["Pick"] {
		t.Fatalf("Kimi API-key columns = %+v", columns)
	}
	if got := usageGridProviderQuota(srUsageRow{provider: accounts.ProviderKimi, authMode: accounts.AuthModeAPIKey}); got != "OAuth only" {
		t.Fatalf("Kimi API-key quota = %q", got)
	}
	if got := usageGridProviderQuota(srUsageRow{provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey}); got != "console only" {
		t.Fatalf("Qwen Token Plan quota = %q", got)
	}
}

func TestAPIKeyProviderStatusUsesCompactRoutingColumns(t *testing.T) {
	t.Setenv("COLUMNS", "120")
	var out bytes.Buffer
	rows := []srUsageRow{
		{email: "deepseek:primary", provider: accounts.ProviderDeepSeek, authMode: accounts.AuthModeAPIKey, providerHealth: "auth ok", planType: "credits, per token", assignedSessions: 2, sessionsKnown: true},
		{email: "deepseek:reserve", provider: accounts.ProviderDeepSeek, authMode: accounts.AuthModeAPIKey, providerHealth: "auth ok", planType: "credits, per token", sessionsKnown: true},
	}
	rankUsageRows(rows)
	displayUsageRows(&out, rows, false)
	text := out.String()
	for _, want := range []string{"Deepseek accounts", "Account", "Plan", "State", "Use", "credits", "active", "rec", "ready", "quota not exposed"} {
		if !strings.Contains(text, want) {
			t.Fatalf("compact API-key provider status should show %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"Key ID", "Sessions", "Models", "Endpoints", "Quota", "5h", "7d"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("compact API-key provider status should not show %q:\n%s", unwanted, text)
		}
	}
}

func TestQwenStatusKeepsMultipleKeysAsSeparateAccounts(t *testing.T) {
	var out bytes.Buffer
	rows := []srUsageRow{
		{email: "qwen-token:team-a", provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey, planType: "Lite", providerHealth: "auth ok", quotaStatus: "live", accountIdentity: "first@example.com", keyFingerprint: "key:1111111111", assignedSessions: 2, sessionsKnown: true, windows: []accounts.UsageWindow{{Name: "7d", UsedPercent: 25, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAfterSeconds: 2 * 86400}}},
		{email: "qwen-token:team-b", provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey, planType: "Pro", providerHealth: "auth ok", quotaStatus: "live", accountIdentity: "second@example.com", keyFingerprint: "key:2222222222", assignedSessions: 0, sessionsKnown: true, windows: []accounts.UsageWindow{{Name: "5h", UsedPercent: 10, LimitWindowSeconds: int64((5 * time.Hour) / time.Second), ResetAfterSeconds: 3600}, {Name: "7d", UsedPercent: 40, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAfterSeconds: 3 * 86400}}},
	}
	for i := range rows {
		rows[i].quotaUsageKnown = true
		rows[i].score = scoreFromWindows(rows[i].email, rows[i].windows)
	}
	rankUsageRows(rows)
	displayUsageRows(&out, rows, false)
	text := out.String()
	for _, want := range []string{"Lite", "Pro", "active", "rec", "75% left", "60% left", "75%/2d", "90%/1h", "60%/3d", "first@example.com", "second@example.com"} {
		if !strings.Contains(text, want) {
			t.Fatalf("Qwen status should show %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"qwen-token:team-", "key:1111111111", "key:2222222222", "Sessions", "Login", "Key ID", "unknown"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("Qwen status should not show %q:\n%s", unwanted, text)
		}
	}
}

func TestQwenStatusOmitsUnreportedQuotaWindow(t *testing.T) {
	var out bytes.Buffer
	rows := []srUsageRow{{
		email: "qwen-token:weekly-only", provider: accounts.ProviderQwenToken,
		authMode: accounts.AuthModeAPIKey, providerHealth: "auth ok", quotaStatus: "live",
		quotaUsageKnown: true, windows: []accounts.UsageWindow{{Name: "7d", UsedPercent: 5}},
	}}
	rows[0].score = scoreFromWindows(rows[0].email, rows[0].windows)
	rankUsageRows(rows)
	displayUsageRows(&out, rows, false)
	text := out.String()
	if strings.Contains(text, "5h") || strings.Contains(text, "unknown") || !strings.Contains(text, "7d") {
		t.Fatalf("Qwen weekly-only status should omit the unreported 5h window:\n%s", text)
	}
}

func TestQwenStatusDisambiguatesSharedLoginWithSavedLabel(t *testing.T) {
	t.Setenv("COLUMNS", "160")
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{
		{email: "qwen-token:large", provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey, accountIdentity: "same@example.com"},
		{email: "qwen-token:small", provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey, accountIdentity: "same@example.com"},
	}, false)
	for _, want := range []string{"same@example.com (large)", "same@example.com (small)"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("duplicate Qwen login should retain saved-label disambiguation %q:\n%s", want, out.String())
		}
	}
}

func TestQwenRemoteLiveQuotaDoesNotImplyModelKeyHealth(t *testing.T) {
	row := srUsageRow{provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey, quotaStatus: "live"}
	if got := usageGridState(row); got != "quota live" {
		t.Fatalf("state = %q", got)
	}
	if got := usageGridStateColor(row); got == ansiGreen {
		t.Fatal("an unprobed remote model key was rendered healthy")
	}
}

func TestQwenRemoteStatusStillShowsQuotaStateWithoutLocalHealthProbe(t *testing.T) {
	row := srUsageRow{provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey, quotaStatus: "login needed"}
	if got := usageGridState(row); got != "login needed" {
		t.Fatalf("Qwen remote state = %q", got)
	}
}

func TestQwenValidatedKeyStaysReadyWhenConsoleLoginExpires(t *testing.T) {
	row := srUsageRow{
		provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey,
		providerHealth: "auth ok", quotaStatus: "login needed",
		err: errors.New("Qwen console login needed"),
	}
	if !displayRecommendedForNewSession(row) {
		t.Fatal("valid Qwen routing key became ineligible when optional telemetry expired")
	}
	if got := usageGridState(row); got != "ready" {
		t.Fatalf("state = %q, want ready", got)
	}
	if got := compactPickReason(row); got != "quota login needed" {
		t.Fatalf("Use = %q, want quota login needed", got)
	}
	if got := usageGridStateColor(row); got == ansiRed {
		t.Fatal("telemetry-only failure rendered valid routing key red")
	}

	row.active = true
	row.gtoRecommended = true
	if got := usageGridState(row); got != "active, rec" {
		t.Fatalf("active state = %q, want active, rec", got)
	}
}

func TestQwenValidatedKeyWithoutTelemetryIsReadyButKnownExhaustionBlocks(t *testing.T) {
	row := srUsageRow{
		provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey,
		providerHealth: "auth ok",
	}
	if usageGridState(row) != "ready" || compactPickReason(row) != "quota not exposed" ||
		!displayRecommendedForNewSession(row) {
		t.Fatalf("missing optional telemetry contaminated routing status: %+v", row)
	}

	row.quotaUsageKnown = true
	row.quotaStatus = "live"
	row.windows = []accounts.UsageWindow{{
		Name: "7d", UsedPercent: 100,
		LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second),
	}}
	row.score = scoreFromWindows("qwen-token:work", row.windows)
	if displayRecommendedForNewSession(row) {
		t.Fatal("known exhausted Qwen quota remained eligible")
	}
}

func TestRemoteQwenValidatedKeyKeepsTelemetryFailureSeparate(t *testing.T) {
	rows := usageRowsFromServerUsageStatuses([]remoteServerUsageStatus{{
		ID: "qwen-token:work", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey,
		AuthChecked: true, AuthValid: true, QuotaStatus: "login needed",
		Error: "Qwen console login needed",
	}})
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	row := rows[0]
	if row.providerHealth != "auth ok" || usageGridState(row) != "rec" ||
		!displayRecommendedForNewSession(row) || compactPickReason(row) != "quota login needed" {
		t.Fatalf("remote Qwen telemetry failure contaminated routing status: %+v", row)
	}
}

func TestLocalQwenStatusExplainsExpiredConsoleLoginOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SUBROUTER_STATE_DIR", t.TempDir())
	store := accounts.DefaultCodexStore()
	stored := accounts.StoredCodexAccount{
		Email: "qwen-token:work", Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "model-secret"},
	}
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	if err := agentqwen.SaveConsoleCredential(stored.Email, agentqwen.ConsoleCredential{
		AccessToken: "expired-console-token", Account: "saved-account@example.test",
	}); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"code":"BailianGateway.Login.NotLogined","message":"login expired"}`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	rows, err := (srRunner{store: store, client: client}).fetchUsageRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.email != stored.Email {
			continue
		}
		if row.quotaStatus != "login needed" || row.accountIdentity != "saved-account@example.test" || row.err == nil ||
			!strings.Contains(row.err.Error(), "sr qwen login 'qwen-token:work'") || strings.Count(row.err.Error(), "login needed") != 1 {
			t.Fatalf("expired local Qwen row = %+v", row)
		}
		return
	}
	t.Fatal("Qwen status row was missing")
}

func TestUsageRowsFromServerPreserveKeyIdentityAndSessions(t *testing.T) {
	rows := usageRowsFromServerUsageStatuses([]remoteServerUsageStatus{{
		ID:               "qwen-token:large-plan",
		Provider:         accounts.ProviderQwenToken,
		AuthMode:         accounts.AuthModeAPIKey,
		KeyFingerprint:   "key:1234567890",
		AssignedSessions: 3,
		SessionsKnown:    true,
	}})
	if len(rows) != 1 || rows[0].keyFingerprint != "key:1234567890" || rows[0].assignedSessions != 3 || !rows[0].sessionsKnown {
		t.Fatalf("remote Qwen identity/session fields lost: %+v", rows)
	}
}

func TestUsageRowsFromServerApplyKimiAPIKeyQuota(t *testing.T) {
	rows := usageRowsFromServerUsageStatuses([]remoteServerUsageStatus{{
		ID: "kimi:work-key", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeAPIKey,
		AuthChecked: true, AuthValid: true, QuotaUsageKnown: true,
		Windows: []accounts.UsageWindow{
			{Name: "5h", UsedPercent: 100, LimitWindowSeconds: int64((5 * time.Hour) / time.Second), ResetAfterSeconds: 900},
			{Name: "weekly", UsedPercent: 20, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
		},
	}})
	if len(rows) != 1 || rows[0].providerHealth != "auth ok" || !rows[0].tempCooked || rows[0].score.ShortHeadroom != 0 {
		t.Fatalf("remote Kimi quota was not applied: %+v", rows)
	}
}

func TestUsageRowsFromServerRecommendAValidatedKimiAPIKey(t *testing.T) {
	rows := usageRowsFromServerUsageStatuses([]remoteServerUsageStatus{{
		ID: "kimi:healthy", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeAPIKey,
		AuthChecked: true, AuthValid: true, QuotaUsageKnown: true,
		Windows: []accounts.UsageWindow{
			{Name: "5h", UsedPercent: 10, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)},
			{Name: "weekly", UsedPercent: 20, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
		},
	}})
	if len(rows) != 1 || rows[0].providerHealth != "auth ok" || !displayRecommendedForNewSession(rows[0]) {
		t.Fatalf("validated remote Kimi key was not recommendable: %+v", rows)
	}
}

func TestKeyedProviderSectionFitsNarrowTerminal(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	columns := usageGridColumns(&bytes.Buffer{}, false, srUsageRow{provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey})
	if width := usageGridWidth(columns); width > 80 {
		t.Fatalf("keyed provider grid width = %d, want <= 80", width)
	}
}

// One subscription reachable over two protocols must read as one account
// listing both endpoints, not as two accounts.
func TestProviderEndpointsCollapseASharedSubscription(t *testing.T) {
	for _, provider := range []accounts.Provider{accounts.ProviderQwenToken, accounts.ProviderQwenAnthropic} {
		got := proxy.ProviderEndpoints(provider)
		want := []string{"/qwen-anthropic", "/qwen-token"}
		if !slices.Equal(got, want) {
			t.Fatalf("provider %q lists %v, want both protocol endpoints", provider, got)
		}
	}
	if got := proxy.ProviderEndpoints(accounts.ProviderGrok); len(got) != 1 || got[0] != "/grok" {
		t.Fatalf("a provider that owns its credential lists %v, want just its own", got)
	}
}

// The health probe must classify what the vendor actually returns, since these
// providers offer no quota API and key validity is the only live signal.
func TestProbeProviderKeyClassifiesTheResponse(t *testing.T) {
	if state, models := probeProviderKey(context.Background(), nil, accounts.ProviderQwenAnthropic, "", "k"); state != "" || models != -1 {
		t.Fatalf("a provider with no health endpoint should report nothing, got %q %d", state, models)
	}
	cases := []struct {
		status int
		body   string
		state  string
		models int
	}{
		{http.StatusUnauthorized, `{}`, "bad key", -1},
		{http.StatusForbidden, `{}`, "denied", -1},
		{http.StatusBadGateway, `{}`, "http 502", -1},
		{http.StatusOK, `{"data":[{},{}]}`, "auth ok", 2},
		{http.StatusOK, `{`, "auth ok", -1},
	}
	for _, tc := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}))
		state, models := probeProviderKey(context.Background(), nil, accounts.ProviderQwenToken, server.URL, "k")
		server.Close()
		if state != tc.state || models != tc.models {
			t.Fatalf("status %d body %q => (%q, %d), want (%q, %d)", tc.status, tc.body, state, models, tc.state, tc.models)
		}
	}
	if state := usageGridState(srUsageRow{providerHealth: "bad key"}); state != "bad key" {
		t.Fatalf("a keyed provider's state should come from its probe, got %q", state)
	}
	if state := usageGridState(srUsageRow{providerHealth: "not checked"}); state != "not checked" {
		t.Fatalf("an unprobed provider should be explicit, got %q", state)
	}
	if color := usageGridStateColor(srUsageRow{providerHealth: "not checked"}); color != "" {
		t.Fatalf("an unprobed key is not known-bad and should not be red, got %q", color)
	}
	// A row with no probe result falls back to the scheduler's state.
	if state := usageGridState(srUsageRow{active: true}); state != "active" {
		t.Fatalf("a non-keyed row should keep the scheduler state, got %q", state)
	}
	if got := usageGridModels(srUsageRow{providerModels: -1}); got != "?" {
		t.Fatalf("an unknown model count should render %q, got %q", "?", got)
	}
	if got := usageGridModels(srUsageRow{providerModels: 11}); got != "11" {
		t.Fatalf("model count = %q, want 11", got)
	}
}

// The flag defaults and the health probe must read the same base URL, or a
// probe silently checks an address the proxy does not use.
func TestProviderDefaultUpstreamsAreDeclared(t *testing.T) {
	for _, provider := range []accounts.Provider{
		accounts.ProviderKimi, accounts.ProviderZAI, accounts.ProviderOpenRouter,
		accounts.ProviderDeepSeek, accounts.ProviderTogether, accounts.ProviderFireworks,
		accounts.ProviderOpenCodeZen,
		accounts.ProviderGrok, accounts.ProviderQwen, accounts.ProviderQwenToken,
		accounts.ProviderQwenAnthropic,
	} {
		if proxy.ProviderDefaultUpstream(provider) == "" {
			t.Fatalf("provider %q declares no default upstream", provider)
		}
		if proxy.ProviderMetering(provider) == "" {
			t.Fatalf("provider %q declares no metering description", provider)
		}
	}
}
