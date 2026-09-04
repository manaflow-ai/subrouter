package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentantigravity "github.com/manaflow-ai/subrouter/internal/agents/antigravity"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	agentgrok "github.com/manaflow-ai/subrouter/internal/agents/grok"
	agentkimi "github.com/manaflow-ai/subrouter/internal/agents/kimi"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestAntigravityAccountImportPublishesMultipleManagedProfiles(t *testing.T) {
	root := t.TempDir()
	codexStore := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
	agyStore := (&agentantigravity.Store{ManagedDir: filepath.Join(root, "antigravity")}).ForServing()
	ref := NewAccountRef(codexStore, nil, nil)
	ref.oauthSources = []OAuthAccountSource{agyStore}
	server := Server{
		AccountRef: ref, SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		antigravityImportAttestForTest: func(_ context.Context, _ *http.Client, credential agentantigravity.CredentialInfo, _ time.Time) (agentantigravity.CredentialInfo, error) {
			credential.AccessToken = "attested-" + credential.AccessToken
			return credential, nil
		},
	}
	for _, label := range []string{"work", "personal"} {
		credential := agentantigravity.CredentialInfo{
			AccessToken: "access-" + label, RefreshToken: "refresh-" + label,
			ExpiresAt: time.Now().Add(time.Hour), OAuthClientID: "client-id", OAuthClientSecret: "client-secret",
		}
		id, err := server.installImportedAccount(t.Context(), accountImportRequest{
			Provider:    accounts.ProviderAntigravity,
			Antigravity: &antigravityAccountImport{Label: label, Credential: credential},
		})
		if err != nil {
			t.Fatal(err)
		}
		if id != "antigravity-subscription:"+label {
			t.Fatalf("import id = %q", id)
		}
	}
	listed, err := agyStore.ListAccounts(t.Context())
	if err != nil || len(listed) != 2 {
		t.Fatalf("managed Antigravity pool = %+v err=%v", listed, err)
	}
}

func TestOpenAccountRefConfiguresAntigravityRefreshTransaction(t *testing.T) {
	root := t.TempDir()
	store := &agentantigravity.Store{ManagedDir: filepath.Join(root, "antigravity")}
	ref, err := OpenAccountRefWithSources(t.Context(), accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}, agentclaude.Store{Dir: filepath.Join(root, "claude")}, nil, []OAuthAccountSource{store})
	if err != nil {
		t.Fatal(err)
	}
	configured, ok := ref.antigravityStore()
	if !ok || configured.RefreshTransaction == nil {
		t.Fatal("Antigravity managed refresh is not serialized with account mutations")
	}
}

func TestAntigravityImportWithoutExplicitStoreFailsClosed(t *testing.T) {
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, nil)
	server := Server{AccountRef: ref}
	_, err := server.installImportedAccount(t.Context(), accountImportRequest{
		Provider: accounts.ProviderAntigravity,
		Antigravity: &antigravityAccountImport{Label: "work", Credential: agentantigravity.CredentialInfo{
			AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour), OAuthClientID: "client", OAuthClientSecret: "secret",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "not configured for this pool") {
		t.Fatalf("unconfigured Antigravity import error = %v", err)
	}
}

func TestAccountImportPreflightAdvertisesOnlyConfiguredAntigravityStore(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured bool
	}{
		{name: "disabled"},
		{name: "enabled", configured: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, nil)
			if tc.configured {
				ref.oauthSources = []OAuthAccountSource{(&agentantigravity.Store{ManagedDir: t.TempDir()}).ForServing()}
			}
			handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
			req := httptest.NewRequest(http.MethodGet, "/_subrouter/account-import", nil)
			req.Header.Set("Authorization", "Bearer secret")
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)
			if resp.Code != http.StatusOK {
				t.Fatalf("preflight status = %d body=%s", resp.Code, resp.Body.String())
			}
			var payload struct {
				Providers []string `json:"providers"`
			}
			if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			advertised := false
			for _, provider := range payload.Providers {
				advertised = advertised || provider == string(accounts.ProviderAntigravity)
			}
			if advertised != tc.configured {
				t.Fatalf("providers = %v, configured=%v", payload.Providers, tc.configured)
			}
		})
	}
}

// stubOAuthSource records refresh calls and returns a fixed account.
type stubOAuthSource struct {
	provider     accounts.Provider
	refreshCalls int
	refreshed    accounts.Account
	err          error
	listed       []accounts.Account
	listErr      error
}

type stubOAuthUsageSource struct {
	stubOAuthSource
	plan     string
	windows  []accounts.UsageWindow
	usageErr error
}

type stubIdentityOAuthUsageSource struct {
	stubOAuthUsageSource
	email string
}

type concurrentOAuthUsageSource struct {
	accounts []accounts.Account
	entered  chan string
	release  chan struct{}

	mu                 sync.Mutex
	sawListDeadline    bool
	sawRefreshDeadline int
	sawUsageDeadline   int
}

type sharedStatusProbeGate struct {
	mu      sync.Mutex
	current int
	maximum int
	entered chan struct{}
	release chan struct{}
}

func (g *sharedStatusProbeGate) run(ctx context.Context) error {
	g.mu.Lock()
	g.current++
	if g.current > g.maximum {
		g.maximum = g.current
	}
	g.mu.Unlock()
	g.entered <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
		g.mu.Lock()
		g.current--
		g.mu.Unlock()
		return ctx.Err()
	}
	g.mu.Lock()
	g.current--
	g.mu.Unlock()
	return nil
}

func (g *sharedStatusProbeGate) max() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.maximum
}

type mixedOAuthStatusSource struct {
	gate     *sharedStatusProbeGate
	accounts []accounts.Account
}

type deadlineOAuthStatusSource struct {
	listDeadline    time.Time
	refreshDeadline time.Time
	usageDeadline   time.Time
}

func (s *deadlineOAuthStatusSource) Provider() accounts.Provider { return accounts.ProviderKimi }

func (s *deadlineOAuthStatusSource) ListAccounts(ctx context.Context) ([]accounts.Account, error) {
	s.listDeadline, _ = ctx.Deadline()
	return []accounts.Account{{
		ID:       "kimi-subscription:deadline",
		Provider: accounts.ProviderKimi,
		AuthMode: accounts.AuthModeOAuth,
	}}, nil
}

func (s *deadlineOAuthStatusSource) RefreshAccount(ctx context.Context, _ *http.Client, account accounts.Account) (accounts.Account, error) {
	s.refreshDeadline, _ = ctx.Deadline()
	return account, nil
}

func (s *deadlineOAuthStatusSource) FetchUsage(ctx context.Context, _ *http.Client, _ accounts.Account) (string, []accounts.UsageWindow, error) {
	s.usageDeadline, _ = ctx.Deadline()
	return "subscription", nil, nil
}

func (s *mixedOAuthStatusSource) Provider() accounts.Provider { return accounts.ProviderKimi }

func (s *mixedOAuthStatusSource) ListAccounts(context.Context) ([]accounts.Account, error) {
	return s.accounts, nil
}

func (s *mixedOAuthStatusSource) RefreshAccount(ctx context.Context, _ *http.Client, account accounts.Account) (accounts.Account, error) {
	return account, s.gate.run(ctx)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func (s *stubOAuthUsageSource) FetchUsage(_ context.Context, _ *http.Client, _ accounts.Account) (string, []accounts.UsageWindow, error) {
	return s.plan, s.windows, s.usageErr
}

func (s *stubIdentityOAuthUsageSource) FetchUsageIdentity(_ context.Context, _ *http.Client, _ accounts.Account) (string, string, []accounts.UsageWindow, error) {
	return s.email, s.plan, s.windows, s.usageErr
}

func (s *stubOAuthSource) Provider() accounts.Provider { return s.provider }

func (s *stubOAuthSource) ListAccounts(_ context.Context) ([]accounts.Account, error) {
	return s.listed, s.listErr
}

func (s *stubOAuthSource) RefreshAccount(_ context.Context, _ *http.Client, account accounts.Account) (accounts.Account, error) {
	s.refreshCalls++
	if s.err != nil {
		return account, s.err
	}
	return s.refreshed, nil
}

func (s *concurrentOAuthUsageSource) Provider() accounts.Provider { return accounts.ProviderKimi }

func (s *concurrentOAuthUsageSource) ListAccounts(ctx context.Context) ([]accounts.Account, error) {
	_, hasDeadline := ctx.Deadline()
	s.mu.Lock()
	s.sawListDeadline = hasDeadline
	s.mu.Unlock()
	return s.accounts, nil
}

func (s *concurrentOAuthUsageSource) RefreshAccount(ctx context.Context, _ *http.Client, account accounts.Account) (accounts.Account, error) {
	_, hasDeadline := ctx.Deadline()
	s.mu.Lock()
	if hasDeadline {
		s.sawRefreshDeadline++
	}
	s.mu.Unlock()
	s.entered <- account.ID
	select {
	case <-s.release:
		return account, nil
	case <-ctx.Done():
		return account, ctx.Err()
	}
}

func (s *concurrentOAuthUsageSource) FetchUsage(ctx context.Context, _ *http.Client, _ accounts.Account) (string, []accounts.UsageWindow, error) {
	_, hasDeadline := ctx.Deadline()
	s.mu.Lock()
	if hasDeadline {
		s.sawUsageDeadline++
	}
	s.mu.Unlock()
	return "subscription", nil, nil
}

// An OAuth account of a registered provider must refresh through its source
// and the refreshed account must replace the snapshot entry.
func TestAccountRefRefreshDispatchesToTheOAuthSource(t *testing.T) {
	kimi := accounts.Account{ID: "kimi-code", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "stale"}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{kimi}, nil)
	source := &stubOAuthSource{
		provider:  accounts.ProviderKimi,
		refreshed: accounts.Account{ID: "kimi-code", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "fresh"},
	}
	ref.oauthSources = []OAuthAccountSource{source}

	refreshed, err := ref.Refresh(context.Background(), kimi)
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if source.refreshCalls != 1 {
		t.Fatalf("source saw %d refreshes, want 1", source.refreshCalls)
	}
	if refreshed.Token != "fresh" {
		t.Fatal("refresh did not return the expected token")
	}
	stored, _ := ref.Snapshot()
	if len(stored) != 1 || stored[0].Token != "fresh" {
		t.Fatal("snapshot does not contain the refreshed account")
	}
}

// A refresh failure must leave the snapshot untouched so the account is not
// swapped for a zero value mid-flight.
func TestAccountRefRefreshKeepsTheAccountWhenTheSourceFails(t *testing.T) {
	kimi := accounts.Account{ID: "kimi-code", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "stale"}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{kimi}, nil)
	source := &stubOAuthSource{provider: accounts.ProviderKimi, err: errors.New("401 Unauthorized")}
	ref.oauthSources = []OAuthAccountSource{source}

	refreshed, err := ref.Refresh(context.Background(), kimi)
	if err == nil {
		t.Fatal("a failing source must surface its error")
	}
	if refreshed.Token != "stale" {
		t.Fatal("failed refresh changed the returned account token")
	}
	stored, _ := ref.Snapshot()
	if len(stored) != 1 || stored[0].Token != "stale" {
		t.Fatal("failed refresh changed the stored account")
	}
}

// Providers without a registered source pass through unchanged, and API-key
// accounts are never refreshed.
func TestAccountRefRefreshLeavesUnregisteredProvidersAlone(t *testing.T) {
	apiKey := accounts.Account{ID: "apikey:kimi", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeAPIKey, Token: "key"}
	oauthOther := accounts.Account{ID: "other", Provider: accounts.ProviderZAI, AuthMode: accounts.AuthModeOAuth, Token: "tok"}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{apiKey, oauthOther}, nil)
	source := &stubOAuthSource{provider: accounts.ProviderKimi}
	ref.oauthSources = []OAuthAccountSource{source}

	for _, acct := range []accounts.Account{apiKey, oauthOther} {
		refreshed, err := ref.Refresh(context.Background(), acct)
		if err != nil {
			t.Fatalf("refresh of %s failed: %v", acct.ID, err)
		}
		if refreshed != acct {
			t.Fatalf("account %s changed", acct.ID)
		}
	}
	if source.refreshCalls != 0 {
		t.Fatalf("source saw %d refreshes, want none", source.refreshCalls)
	}
}

// Account listing aggregates the registered sources alongside the Codex and
// Claude stores.
func TestLoadAccountRefAccountsAggregatesOAuthSources(t *testing.T) {
	source := &stubOAuthSource{
		provider: accounts.ProviderKimi,
		listed:   []accounts.Account{{ID: "kimi-code", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "tok"}},
	}
	loaded, err := loadAccountRefAccounts(accounts.CodexStore{Dir: t.TempDir()}, agentclaude.Store{Dir: t.TempDir()}, []OAuthAccountSource{source})
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	var found bool
	for _, acct := range loaded {
		if acct.Provider == accounts.ProviderKimi && acct.AuthMode == accounts.AuthModeOAuth {
			found = true
		}
	}
	if !found {
		t.Fatalf("%d loaded accounts missing the source account", len(loaded))
	}
}

func TestUsageStatusesIncludesAuthOnlyOAuthSource(t *testing.T) {
	account := accounts.Account{
		ID: "grok-subscription", Provider: accounts.ProviderGrok,
		AuthMode: accounts.AuthModeOAuth, Token: "access", Label: "Grok subscription",
	}
	source := &stubOAuthSource{
		provider: accounts.ProviderGrok,
		listed:   []accounts.Account{account},
		refreshed: accounts.Account{
			ID: "grok-subscription", Provider: accounts.ProviderGrok,
			AuthMode: accounts.AuthModeOAuth, Token: "fresh", Label: "Grok subscription",
		},
	}
	ref := &AccountRef{
		accounts:     []accounts.Account{account},
		store:        accounts.CodexStore{Dir: t.TempDir()},
		claudeStore:  agentclaude.Store{Dir: t.TempDir()},
		oauthSources: []OAuthAccountSource{source},
		client:       http.DefaultClient,
	}
	statuses := ref.UsageStatuses(t.Context())
	if len(statuses) != 1 {
		t.Fatalf("got %d statuses, want one Grok auth-only status: %+v", len(statuses), statuses)
	}
	got := statuses[0]
	if got.Provider != accounts.ProviderGrok || got.AuthMode != accounts.AuthModeOAuth || !got.AuthChecked || !got.AuthValid {
		t.Fatalf("Grok auth status = %+v", got)
	}
	if got.PlanType != "subscription" || len(got.Windows) != 0 || got.UsageFresh {
		t.Fatalf("Grok quota fields should be honest auth-only data: %+v", got)
	}
	if source.refreshCalls != 1 {
		t.Fatalf("source saw %d refreshes, want one auth check", source.refreshCalls)
	}
}

func TestOpenAccountRefConfiguresKimiRefreshTransaction(t *testing.T) {
	store := accounts.CodexStore{Dir: t.TempDir()}
	kimiStore := agentkimi.Store{Path: t.TempDir() + "/kimi.json", ManagedDir: t.TempDir()}
	ref, err := OpenAccountRefWithSources(t.Context(), store, agentclaude.Store{Dir: t.TempDir()}, http.DefaultClient, []OAuthAccountSource{kimiStore})
	if err != nil {
		t.Fatal(err)
	}
	if ref.kimiStore().RefreshTransaction == nil {
		t.Fatal("Kimi source refresh is not coordinated with account add/remove transactions")
	}
}

func TestOpenAccountRefForcesSuppliedKimiStoresToManagedOnly(t *testing.T) {
	for _, pointer := range []bool{false, true} {
		name := "value"
		if pointer {
			name = "pointer"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			cliPath := filepath.Join(root, "credentials", "kimi-code.json")
			if err := os.MkdirAll(filepath.Dir(cliPath), 0o700); err != nil {
				t.Fatal(err)
			}
			before := []byte(`{"access_token":"interactive","refresh_token":"interactive-refresh","expires_at":1}`)
			if err := os.WriteFile(cliPath, before, 0o600); err != nil {
				t.Fatal(err)
			}
			kimiStore := agentkimi.Store{Path: cliPath, KimiHome: root, ManagedDir: filepath.Join(root, "managed")}
			managed, err := kimiStore.SaveManagedCredential("work", agentkimi.CredentialInfo{
				AccessToken: "managed", RefreshToken: "managed-refresh", ExpiresAt: time.Now().Add(time.Hour),
				OAuthDeviceID: "managed-device",
			})
			if err != nil {
				t.Fatal(err)
			}
			var source OAuthAccountSource = kimiStore
			if pointer {
				source = &kimiStore
			}
			codexStore := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
			ref, err := OpenAccountRefWithSources(t.Context(), codexStore, agentclaude.Store{Dir: filepath.Join(root, "claude")}, http.DefaultClient, []OAuthAccountSource{source})
			if err != nil {
				t.Fatal(err)
			}
			all := ref.All()
			if len(all) != 1 || all[0].ID != managed.ID {
				t.Fatalf("serving snapshot = %+v, want only managed Kimi account %q", all, managed.ID)
			}
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
			defer server.Close()
			configured := ref.kimiStore()
			_, err = configured.RefreshAccount(t.Context(), server.Client(), accounts.Account{
				ID: "kimi-code", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth,
			})
			if err == nil || !strings.Contains(err.Error(), "not routable") {
				t.Fatalf("interactive refresh error = %v", err)
			}
			if requests != 0 {
				t.Fatalf("interactive refresh made %d HTTP request(s)", requests)
			}
			after, err := os.ReadFile(cliPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Fatal("serving AccountRef changed interactive Kimi credential bytes")
			}
		})
	}
}

func TestOpenAccountRefConfiguresGrokRefreshTransaction(t *testing.T) {
	store := accounts.CodexStore{Dir: t.TempDir()}
	grokStore := agentgrok.Store{Path: filepath.Join(t.TempDir(), "grok.json")}
	ref, err := OpenAccountRefWithSources(t.Context(), store, agentclaude.Store{Dir: t.TempDir()}, http.DefaultClient, []OAuthAccountSource{grokStore})
	if err != nil {
		t.Fatal(err)
	}
	configured, ok := ref.oauthSources[0].(agentgrok.Store)
	if !ok || configured.RefreshTransaction == nil {
		t.Fatal("Grok source refresh is not coordinated with account add/remove transactions")
	}
}

func TestLoadAccountRefAccountsKeepsHealthySourcesWhenOneFails(t *testing.T) {
	broken := &stubOAuthSource{provider: accounts.ProviderKimi, listErr: errors.New("unreadable credential")}
	healthy := &stubOAuthSource{provider: accounts.ProviderZAI, listed: []accounts.Account{{ID: "healthy", Provider: accounts.ProviderZAI}}}
	loaded, err := loadAccountRefAccounts(accounts.CodexStore{Dir: t.TempDir()}, agentclaude.Store{Dir: t.TempDir()}, []OAuthAccountSource{broken, healthy})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].ID != "healthy" {
		t.Fatalf("loaded %d accounts, want the healthy source retained", len(loaded))
	}
}

func TestLoadAccountRefAccountsKeepsPartialResultsFromOneSource(t *testing.T) {
	partial := &stubOAuthSource{
		provider: accounts.ProviderKimi,
		listed:   []accounts.Account{{ID: "kimi-subscription:healthy", Provider: accounts.ProviderKimi}},
		listErr:  errors.New("one managed profile is unreadable"),
	}
	loaded, err := loadAccountRefAccounts(accounts.CodexStore{Dir: t.TempDir()}, agentclaude.Store{Dir: t.TempDir()}, []OAuthAccountSource{partial})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].ID != "kimi-subscription:healthy" {
		t.Fatalf("partial OAuth source retained %d accounts, want healthy account", len(loaded))
	}
}

func TestKimiManagedAndAPIKeyAccountsUseDisjointIDs(t *testing.T) {
	codexStore := accounts.CodexStore{Dir: t.TempDir()}
	apiKey, _, err := codexStore.AddAPIKeyForProvider("work", "test-kimi-key", accounts.ProviderKimi)
	if err != nil {
		t.Fatal(err)
	}
	kimiStore := agentkimi.Store{Path: filepath.Join(t.TempDir(), "unused-cli.json"), ManagedDir: t.TempDir()}
	managed, err := kimiStore.SaveManagedCredential("work", agentkimi.CredentialInfo{
		AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour), OAuthDeviceID: "authorized-device",
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := loadAccountRefAccounts(codexStore, agentclaude.Store{Dir: t.TempDir()}, []OAuthAccountSource{kimiStore})
	if err != nil {
		t.Fatal(err)
	}
	if apiKey.Email == managed.ID {
		t.Fatalf("Kimi API key and managed OAuth account collided at %q", managed.ID)
	}
	want := map[string]bool{apiKey.Email: false, managed.ID: false}
	for _, acct := range loaded {
		if _, ok := want[acct.ID]; ok {
			want[acct.ID] = true
		}
	}
	for id, found := range want {
		if !found {
			t.Fatalf("loaded accounts missing %q: %+v", id, loaded)
		}
	}
}

func TestRefreshSelectedKimiAccountFailsOverOnTerminalCredentialError(t *testing.T) {
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	dead := accounts.Account{ID: "kimi-subscription:dead", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "stale"}
	healthy := accounts.Account{ID: "kimi-subscription:healthy", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "fresh"}
	server := Server{
		Accounts:     []accounts.Account{dead, healthy},
		Sessions:     sessions,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
	}
	var refreshed []string
	server.RefreshAccountFn = func(_ context.Context, acct accounts.Account) (accounts.Account, error) {
		refreshed = append(refreshed, acct.ID)
		if acct.ID == dead.ID {
			return acct, errors.New("Kimi OAuth refresh failed: invalid_grant")
		}
		return acct, nil
	}
	request, err := http.NewRequest(http.MethodPost, "https://subrouter.test/v1/chat/completions", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	got, pending, err := server.refreshSelectedAccount(context.Background(), accounts.ProviderKimi, "kimi", "session-1", "", request, dead)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != healthy.ID || len(refreshed) != 2 || refreshed[0] != dead.ID || refreshed[1] != healthy.ID {
		t.Fatalf("Kimi refresh failover got=%q refreshed=%v", got.ID, refreshed)
	}
	if !pending {
		t.Fatal("the refreshed Kimi alternate should remain provisional until upstream success")
	}
	if !server.SchedulerRef.Get().Exhausted(accounts.ProviderKimi, dead.ID) {
		t.Fatal("terminal Kimi refresh failure was not marked exhausted")
	}
	if assignment, ok := sessions.Get("kimi", "session-1"); ok {
		t.Fatalf("pre-request refresh committed the provisional Kimi alternate: %+v", assignment)
	}
}

func TestRefreshSelectedAntigravityAccountFailsOverOnTerminalCredentialError(t *testing.T) {
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	dead := accounts.Account{ID: "antigravity-subscription:dead", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth, Token: "stale"}
	healthy := accounts.Account{ID: "antigravity-subscription:healthy", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth, Token: "fresh"}
	server := Server{Accounts: []accounts.Account{dead, healthy}, Sessions: sessions, SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))}
	var refreshed []string
	server.RefreshAccountFn = func(_ context.Context, acct accounts.Account) (accounts.Account, error) {
		refreshed = append(refreshed, acct.ID)
		if acct.ID == dead.ID {
			return acct, errors.New("Antigravity OAuth refresh failed: invalid_grant")
		}
		return acct, nil
	}
	request, err := http.NewRequest(http.MethodPost, "https://subrouter.test/antigravity/v1internal:test", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	got, pending, err := server.refreshSelectedAccount(context.Background(), accounts.ProviderAntigravity, "antigravity", "session-1", "", request, dead)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != healthy.ID || !pending || len(refreshed) != 2 {
		t.Fatalf("Antigravity refresh failover got=%q pending=%v refreshed=%v", got.ID, pending, refreshed)
	}
	if !server.SchedulerRef.Get().Exhausted(accounts.ProviderAntigravity, dead.ID) {
		t.Fatal("terminal Antigravity refresh failure was not marked exhausted")
	}
}

func TestRefreshSelectedKimiAccountDoesNotFailOverOnTransientError(t *testing.T) {
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	selected := accounts.Account{ID: "kimi-subscription:selected", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "stale"}
	server := Server{
		Accounts: []accounts.Account{
			selected,
			{ID: "kimi-subscription:other", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "fresh"},
		},
		Sessions:     sessions,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
	}
	refreshes := 0
	server.RefreshAccountFn = func(_ context.Context, acct accounts.Account) (accounts.Account, error) {
		refreshes++
		return acct, context.DeadlineExceeded
	}
	request, err := http.NewRequest(http.MethodPost, "https://subrouter.test/v1/chat/completions", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	got, pending, err := server.refreshSelectedAccount(context.Background(), accounts.ProviderKimi, "kimi", "session-1", "", request, selected)
	if !errors.Is(err, context.DeadlineExceeded) || got.ID != selected.ID || pending || refreshes != 1 {
		t.Fatalf("transient Kimi refresh got=%q refreshes=%d err=%v", got.ID, refreshes, err)
	}
	if server.SchedulerRef.Get().Exhausted(accounts.ProviderKimi, selected.ID) {
		t.Fatal("transient Kimi refresh failure marked the account exhausted")
	}
}

func TestRefreshSelectedKimiAccountKeepsStickyAssignmentWhenAlternateRefreshFails(t *testing.T) {
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	dead := accounts.Account{ID: "kimi-subscription:dead", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "stale"}
	alternate := accounts.Account{ID: "kimi-subscription:alternate", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "also-stale"}
	if _, err := sessions.Put("kimi", "session-1", dead.ID, ""); err != nil {
		t.Fatal(err)
	}
	server := Server{
		Accounts:     []accounts.Account{dead, alternate},
		Sessions:     sessions,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		RefreshAccountFn: func(_ context.Context, acct accounts.Account) (accounts.Account, error) {
			if acct.ID == dead.ID {
				return acct, errors.New("Kimi OAuth refresh failed: invalid_grant")
			}
			return acct, context.DeadlineExceeded
		},
	}
	request, err := http.NewRequest(http.MethodPost, "https://subrouter.test/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	got, pending, err := server.refreshSelectedAccount(context.Background(), accounts.ProviderKimi, "kimi", "session-1", "", request, dead)
	if !errors.Is(err, context.DeadlineExceeded) || got.ID != alternate.ID || pending {
		t.Fatalf("failed alternate refresh got=%q pending=%v err=%v", got.ID, pending, err)
	}
	assignment, ok := sessions.Get("kimi", "session-1")
	if !ok || assignment.AccountID != dead.ID {
		t.Fatalf("failed alternate refresh changed sticky assignment: %+v", assignment)
	}
}

func TestHandlerCommitsRefreshFailoverOnlyAfterUpstreamSuccess(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		status     int
		wantCommit bool
	}{
		{name: "non replayable success", method: http.MethodGet, path: "/kimi/models", status: http.StatusOK, wantCommit: true},
		{name: "non replayable failure", method: http.MethodGet, path: "/kimi/models", status: http.StatusServiceUnavailable},
		{name: "replayable success", method: http.MethodPost, path: "/kimi/v1/messages", status: http.StatusOK, wantCommit: true},
		{name: "replayable failure", method: http.MethodPost, path: "/kimi/v1/messages", status: http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dead := accounts.Account{ID: "kimi-subscription:dead", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "stale"}
			healthy := accounts.Account{ID: "kimi-subscription:healthy", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "fresh"}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Authorization") != "Bearer fresh" {
					t.Errorf("upstream Authorization = %q, want refreshed alternate", request.Header.Get("Authorization"))
				}
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, `{}`)
			}))
			defer upstream.Close()

			sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
			if err != nil {
				t.Fatal(err)
			}
			const sessionID = "refresh-success-boundary"
			if _, err := sessions.Put("kimi", sessionID, dead.ID, ""); err != nil {
				t.Fatal(err)
			}
			server := Server{
				Accounts:     []accounts.Account{dead, healthy},
				Sessions:     sessions,
				SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
				KimiUpstream: mustParseURL(t, upstream.URL+"/coding/v1"),
				MaxBodyBytes: 1024,
				RefreshAccountFn: func(_ context.Context, acct accounts.Account) (accounts.Account, error) {
					if acct.ID == dead.ID {
						return acct, errors.New("Kimi OAuth refresh failed: invalid_grant")
					}
					return acct, nil
				},
			}
			var body io.Reader
			if test.method == http.MethodPost {
				body = strings.NewReader(`{"model":"kimi-for-coding","messages":[]}`)
			}
			request := httptest.NewRequest(test.method, test.path, body)
			request.Header.Set("X-Subrouter-Agent", "kimi")
			request.Header.Set("X-Subrouter-Session", sessionID)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}
			assignment, ok := sessions.Get("kimi", sessionID)
			if !ok {
				t.Fatal("sticky assignment disappeared")
			}
			want := dead.ID
			if test.wantCommit {
				want = healthy.ID
			}
			if assignment.AccountID != want {
				t.Fatalf("sticky assignment = %q, want %q", assignment.AccountID, want)
			}
		})
	}
}

func TestNonReplayableStreamingRefreshFailoverWaitsForTerminalSuccess(t *testing.T) {
	for _, test := range []struct {
		name       string
		stream     string
		wantCommit bool
	}{
		{name: "completed", stream: "data: {\"type\":\"response.completed\"}\n\n", wantCommit: true},
		{name: "failed", stream: "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_is_overloaded\"}}}\n\n"},
		{name: "clean eof without terminal marker", stream: "data: {\"type\":\"response.in_progress\"}\n\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dead := accounts.Account{ID: "kimi-subscription:dead", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "stale"}
			healthy := accounts.Account{ID: "kimi-subscription:healthy", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "fresh"}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Authorization") != "Bearer fresh" {
					t.Errorf("Authorization = %q, want refreshed alternate", request.Header.Get("Authorization"))
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.stream)
			}))
			defer upstream.Close()
			store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
			if err != nil {
				t.Fatal(err)
			}
			const sessionID = "non-replayable-stream"
			if _, err := store.Put("kimi", sessionID, dead.ID, ""); err != nil {
				t.Fatal(err)
			}
			server := Server{
				Accounts: []accounts.Account{dead, healthy}, Sessions: store,
				SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
				KimiUpstream: mustParseURL(t, upstream.URL+"/coding/v1"), MaxBodyBytes: 1024,
				RefreshAccountFn: func(_ context.Context, acct accounts.Account) (accounts.Account, error) {
					if acct.ID == dead.ID {
						return acct, errors.New("Kimi OAuth refresh failed: invalid_grant")
					}
					return acct, nil
				},
			}
			request := httptest.NewRequest(http.MethodGet, "/kimi/models", nil)
			request.Header.Set("X-Subrouter-Agent", "kimi")
			request.Header.Set("X-Subrouter-Session", sessionID)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.Code)
			}
			assignment, ok := store.Get("kimi", sessionID)
			if !ok {
				t.Fatal("sticky assignment disappeared")
			}
			want := dead.ID
			if test.wantCommit {
				want = healthy.ID
			}
			if assignment.AccountID != want {
				t.Fatalf("sticky assignment = %q, want %q", assignment.AccountID, want)
			}
		})
	}
}

func TestReplayableRefreshFailoverRejectsSuccessWhenStickyAssignmentCannotPersist(t *testing.T) {
	dead := accounts.Account{ID: "kimi-subscription:dead", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "stale"}
	healthy := accounts.Account{ID: "kimi-subscription:healthy", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "fresh"}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer fresh" {
			t.Errorf("Authorization = %q, want refreshed alternate", request.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "sessions.json")
	sessions, err := session.NewStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "refresh-persistence-failure"
	if _, err := sessions.Put("kimi", sessionID, dead.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(storePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(storePath, 0o700); err != nil {
		t.Fatal(err)
	}
	server := Server{
		Accounts:     []accounts.Account{dead, healthy},
		Sessions:     sessions,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		KimiUpstream: mustParseURL(t, upstream.URL+"/coding/v1"),
		MaxBodyBytes: 1024,
		RefreshAccountFn: func(_ context.Context, acct accounts.Account) (accounts.Account, error) {
			if acct.ID == dead.ID {
				return acct, errors.New("Kimi OAuth refresh failed: invalid_grant")
			}
			return acct, nil
		},
	}
	request := httptest.NewRequest(http.MethodPost, "/kimi/v1/messages", strings.NewReader(`{"model":"kimi-for-coding","messages":[]}`))
	request.Header.Set("X-Subrouter-Agent", "kimi")
	request.Header.Set("X-Subrouter-Session", sessionID)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != `{}` {
		t.Fatalf("response status=%d body=%q, want progressive body delivery before EOF persistence failure", response.Code, response.Body.String())
	}
	assignment, ok := sessions.Get("kimi", sessionID)
	if !ok || assignment.AccountID != dead.ID {
		t.Fatalf("failed persistence changed sticky assignment: %+v", assignment)
	}
}

func TestUsageStatusesIncludesOAuthUsageSources(t *testing.T) {
	acct := accounts.Account{ID: "kimi-code", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Label: "work", Token: "access", Source: "credential file"}
	source := &stubOAuthUsageSource{
		stubOAuthSource: stubOAuthSource{provider: accounts.ProviderKimi, listed: []accounts.Account{acct}, refreshed: acct},
		plan:            "subscription",
		windows:         []accounts.UsageWindow{{Name: "5h", UsedPercent: 25, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)}},
	}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, http.DefaultClient)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	ref.oauthSources = []OAuthAccountSource{source}

	statuses := ref.UsageStatuses(context.Background())
	if len(statuses) != 1 {
		t.Fatalf("statuses = %+v, want one Kimi row", statuses)
	}
	got := statuses[0]
	if got.Provider != accounts.ProviderKimi || got.ID != "kimi-code" || got.PlanType != "subscription" || got.AccountIdentity != "work" || !got.AuthValid || got.Active || !got.UsageFresh {
		t.Fatalf("status = %+v", got)
	}
	if len(got.Windows) != 1 || got.Windows[0].UsedPercent != 25 {
		t.Fatalf("windows = %+v", got.Windows)
	}
}

func TestUsageStatusesUsesProviderVerifiedOAuthIdentityWithoutChangingStableID(t *testing.T) {
	acct := accounts.Account{ID: "antigravity-subscription:work", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth, Label: "work", Token: "access"}
	source := &stubIdentityOAuthUsageSource{
		stubOAuthUsageSource: stubOAuthUsageSource{
			stubOAuthSource: stubOAuthSource{provider: accounts.ProviderAntigravity, listed: []accounts.Account{acct}, refreshed: acct},
			plan:            "Google AI Ultra",
			windows:         []accounts.UsageWindow{{Name: "gemini 5h", UsedPercent: 25}},
		},
		email: "verified@example.com",
	}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, http.DefaultClient)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	ref.oauthSources = []OAuthAccountSource{source}

	statuses := ref.UsageStatuses(context.Background())
	if len(statuses) != 1 {
		t.Fatalf("statuses = %+v", statuses)
	}
	got := statuses[0]
	if got.ID != acct.ID || got.Email != "verified@example.com" || got.AccountIdentity != "verified@example.com" || got.PlanType != "Google AI Ultra" {
		t.Fatalf("status = %+v", got)
	}
}

func TestUsageStatusesBoundsAndParallelizesOAuthSourceAccounts(t *testing.T) {
	source := &concurrentOAuthUsageSource{
		accounts: []accounts.Account{
			{ID: "kimi-subscription:first", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth},
			{ID: "kimi-subscription:second", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth},
		},
		entered: make(chan string, 2),
		release: make(chan struct{}),
	}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, http.DefaultClient)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	ref.oauthSources = []OAuthAccountSource{source}

	result := make(chan []AccountUsageStatus, 1)
	go func() { result <- ref.UsageStatuses(context.Background()) }()
	for i := 0; i < 2; i++ {
		select {
		case <-source.entered:
		case <-time.After(time.Second):
			t.Fatal("OAuth account status checks did not run concurrently")
		}
	}
	close(source.release)

	var statuses []AccountUsageStatus
	select {
	case statuses = <-result:
	case <-time.After(time.Second):
		t.Fatal("OAuth account status sweep did not complete after release")
	}
	if len(statuses) != 2 || !statuses[0].AuthValid || !statuses[1].AuthValid {
		t.Fatalf("statuses = %+v, want two valid accounts", statuses)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if !source.sawListDeadline || source.sawRefreshDeadline != 2 || source.sawUsageDeadline != 2 {
		t.Fatalf("deadline coverage: list=%v refresh=%d usage=%d", source.sawListDeadline, source.sawRefreshDeadline, source.sawUsageDeadline)
	}
}

func TestUsageStatusesSharesOneConcurrencyBudgetAcrossKeyedAndOAuthSources(t *testing.T) {
	store := accounts.CodexStore{Dir: t.TempDir()}
	for i := 0; i < 3; i++ {
		if _, _, err := store.AddAPIKeyForProvider(
			"key-"+string(rune('a'+i)),
			"secret-"+string(rune('a'+i)),
			accounts.ProviderOpenRouter,
		); err != nil {
			t.Fatal(err)
		}
	}
	gate := &sharedStatusProbeGate{
		entered: make(chan struct{}, 16),
		release: make(chan struct{}),
	}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if err := gate.run(request.Context()); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    request,
		}, nil
	})
	oauthAccounts := make([]accounts.Account, 8)
	for i := range oauthAccounts {
		oauthAccounts[i] = accounts.Account{
			ID:       "kimi-subscription:" + string(rune('a'+i)),
			Provider: accounts.ProviderKimi,
			AuthMode: accounts.AuthModeOAuth,
		}
	}
	ref := NewAccountRef(store, nil, &http.Client{Transport: transport})
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	ref.apiKeyUpstreams = map[accounts.Provider]string{
		accounts.ProviderOpenRouter: "https://openrouter.test/api/v1",
	}
	ref.oauthSources = []OAuthAccountSource{&mixedOAuthStatusSource{gate: gate, accounts: oauthAccounts}}

	result := make(chan []AccountUsageStatus, 1)
	go func() { result <- ref.usageStatusesLive(context.Background()) }()
	for range accountFetchConcurrency {
		select {
		case <-gate.entered:
		case <-time.After(time.Second):
			close(gate.release)
			t.Fatal("shared status sweep did not fill its concurrency budget")
		}
	}
	select {
	case <-gate.entered:
		close(gate.release)
		t.Fatal("keyed and OAuth probes exceeded the shared concurrency budget")
	case <-time.After(50 * time.Millisecond):
	}
	close(gate.release)
	select {
	case statuses := <-result:
		if len(statuses) != 11 {
			t.Fatalf("statuses = %d, want 11", len(statuses))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shared status sweep did not complete after release")
	}
	if maximum := gate.max(); maximum > accountFetchConcurrency {
		t.Fatalf("maximum concurrent status probes = %d, want <= %d", maximum, accountFetchConcurrency)
	}
}

func TestUsageStatusesSharesOneDeadlineAcrossKeyedAndOAuthSources(t *testing.T) {
	store := accounts.CodexStore{Dir: t.TempDir()}
	if _, _, err := store.AddAPIKeyForProvider("work", "secret", accounts.ProviderOpenRouter); err != nil {
		t.Fatal(err)
	}
	var keyedDeadline time.Time
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		keyedDeadline, _ = request.Context().Deadline()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    request,
		}, nil
	})
	source := &deadlineOAuthStatusSource{}
	ref := NewAccountRef(store, nil, &http.Client{Transport: transport})
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	ref.apiKeyUpstreams = map[accounts.Provider]string{
		accounts.ProviderOpenRouter: "https://openrouter.test/api/v1",
	}
	ref.oauthSources = []OAuthAccountSource{source}

	ref.usageStatusesLive(context.Background())
	if keyedDeadline.IsZero() || source.listDeadline.IsZero() || source.refreshDeadline.IsZero() || source.usageDeadline.IsZero() {
		t.Fatalf(
			"status deadlines keyed=%v list=%v refresh=%v usage=%v, want all set",
			keyedDeadline, source.listDeadline, source.refreshDeadline, source.usageDeadline,
		)
	}
	for phase, deadline := range map[string]time.Time{
		"list": source.listDeadline, "refresh": source.refreshDeadline, "usage": source.usageDeadline,
	} {
		if !keyedDeadline.Equal(deadline) {
			t.Fatalf("status deadlines keyed=%v OAuth %s=%v, want one shared deadline", keyedDeadline, phase, deadline)
		}
	}
}

func TestUsageStatusesKeepsKimiAPIKeyHealthWhenQuotaIsForbidden(t *testing.T) {
	store := accounts.CodexStore{Dir: t.TempDir()}
	stored, _, err := store.AddAPIKeyForProvider("work", "test-kimi-key", accounts.ProviderKimi)
	if err != nil {
		t.Fatal(err)
	}
	const kimiUpstream = "https://custom.kimi.test/coding/v1"
	var healthURL string
	var quotaRequested bool
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		status := http.StatusForbidden
		body := `{}`
		if strings.HasSuffix(request.URL.Path, "/models") {
			healthURL = request.URL.String()
			status = http.StatusOK
			body = `{"data":[]}`
		} else {
			quotaRequested = true
		}
		return &http.Response{
			StatusCode: status,
			Status:     http.StatusText(status),
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
			Request:    request,
		}, nil
	})}
	ref := NewAccountRef(store, nil, client)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	_ = (Server{AccountRef: ref, KimiUpstream: mustParseURL(t, kimiUpstream)}).Handler()

	statuses := ref.UsageStatuses(context.Background())
	if len(statuses) != 1 {
		t.Fatalf("statuses = %+v, want one Kimi API-key row", statuses)
	}
	got := statuses[0]
	if got.ID != stored.Email || got.ProviderHealth != "auth ok" || !got.AuthChecked || !got.AuthValid {
		t.Fatalf("Kimi inference-key health was lost with unavailable quota: %+v", got)
	}
	if got.QuotaUsageKnown || got.UsageFresh {
		t.Fatalf("forbidden quota was reported as live: %+v", got)
	}
	if healthURL != kimiUpstream+"/models" {
		t.Fatalf("Kimi health probe URL = %q, want configured upstream", healthURL)
	}
	if quotaRequested {
		t.Fatal("custom-upstream Kimi key was sent to the vendor quota endpoint")
	}
}

func TestUsageStatusesIncludesOpenRouterKeyQuota(t *testing.T) {
	store := accounts.CodexStore{Dir: t.TempDir()}
	stored, _, err := store.AddAPIKeyForProvider("primary", "test-openrouter-key", accounts.ProviderOpenRouter)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/key" || request.Header.Get("Authorization") != "Bearer test-openrouter-key" {
			t.Errorf("unexpected OpenRouter probe path=%q auth=%q", request.URL.Path, request.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"data":{"limit":200,"limit_remaining":150,"limit_reset":"monthly"}}`)
	}))
	defer upstream.Close()

	ref := NewAccountRef(store, nil, upstream.Client())
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	_ = (Server{AccountRef: ref, OpenRouterUpstream: mustParseURL(t, upstream.URL+"/api/v1")}).Handler()
	statuses := ref.UsageStatuses(context.Background())
	if len(statuses) != 1 {
		t.Fatalf("statuses = %+v, want one OpenRouter row", statuses)
	}
	got := statuses[0]
	if got.ID != stored.Email || got.Provider != accounts.ProviderOpenRouter || got.ProviderHealth != "auth ok" || !got.AuthChecked || !got.AuthValid || got.QuotaStatus != "live" || !got.QuotaUsageKnown || !got.UsageFresh {
		t.Fatalf("OpenRouter status = %+v", got)
	}
	if len(got.Windows) != 1 || got.Windows[0].Name != "monthly" || got.Windows[0].UsedPercent != 25 {
		t.Fatalf("OpenRouter windows = %+v", got.Windows)
	}
	if got.Credits == nil || got.Credits.Balance != "150" {
		t.Fatalf("OpenRouter credits = %+v", got.Credits)
	}
}

func TestFetchUsageWindowsCachedUsesMatchingOAuthUsageSource(t *testing.T) {
	acct := accounts.Account{ID: "kimi-subscription:work", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "access"}
	want := []accounts.UsageWindow{{Name: "5h", UsedPercent: 37, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)}}
	source := &stubOAuthUsageSource{
		stubOAuthSource: stubOAuthSource{provider: accounts.ProviderKimi},
		windows:         want,
	}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{acct}, nil)
	ref.oauthSources = []OAuthAccountSource{source}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("Kimi usage must not fall through to the Codex HTTP endpoint")
		return nil, errors.New("unexpected Codex request")
	})}

	windows, fresh, err := ref.FetchUsageWindowsCached(context.Background(), client, acct)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh || len(windows) != 1 || windows[0].Name != "5h" || windows[0].UsedPercent != 37 {
		t.Fatalf("windows=%+v fresh=%v, want matching Kimi source data", windows, fresh)
	}
}

func TestFetchUsageWindowsCachedDoesNotSendCredentialOnlyOAuthTokenToCodex(t *testing.T) {
	acct := accounts.Account{
		ID: "grok-subscription", Provider: accounts.ProviderGrok,
		AuthMode: accounts.AuthModeOAuth, Token: "grok-access-token",
	}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{acct}, nil)
	ref.oauthSources = []OAuthAccountSource{&stubOAuthSource{provider: accounts.ProviderGrok}}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("credential-only Grok token was sent to %s", request.URL)
		return nil, errors.New("unexpected request")
	})}

	windows, fresh, err := ref.FetchUsageWindowsCached(t.Context(), client, acct)
	if err == nil || !strings.Contains(err.Error(), "usage unavailable") {
		t.Fatalf("windows=%+v fresh=%v err=%v, want unavailable without a request", windows, fresh, err)
	}
}

func TestLegacyUsageFetchRejectsNonCodexOAuthProvider(t *testing.T) {
	acct := accounts.Account{
		ID: "antigravity", Provider: accounts.ProviderAntigravity,
		AuthMode: accounts.AuthModeOAuth, Token: "antigravity-access-token",
	}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("non-Codex OAuth token was sent to %s", request.URL)
		return nil, errors.New("unexpected request")
	})}

	windows, err := fetchAccountUsageWindowsLive(t.Context(), client, acct)
	if err == nil || !strings.Contains(err.Error(), "usage unavailable") {
		t.Fatalf("windows=%+v err=%v, want unavailable without a request", windows, err)
	}
}

func TestUsageStatusesReportsPartialSourceErrorAndHealthyAccount(t *testing.T) {
	acct := accounts.Account{ID: "kimi-subscription:healthy", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Label: "healthy", Token: "access"}
	source := &stubOAuthUsageSource{
		stubOAuthSource: stubOAuthSource{
			provider:  accounts.ProviderKimi,
			listed:    []accounts.Account{acct},
			listErr:   errors.New("managed profile kimi-subscription:broken is unreadable"),
			refreshed: acct,
		},
		plan: "subscription",
	}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, http.DefaultClient)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	ref.oauthSources = []OAuthAccountSource{source}

	statuses := ref.UsageStatuses(context.Background())
	if len(statuses) != 2 {
		t.Fatalf("statuses = %d, want one error plus one healthy account", len(statuses))
	}
	if statuses[0].Error == "" || statuses[0].ID != string(accounts.ProviderKimi) {
		t.Fatal("partial source error was not surfaced independently")
	}
	if statuses[1].ID != acct.ID || !statuses[1].AuthValid {
		t.Fatal("healthy account from partial source was not status-checked")
	}
}
