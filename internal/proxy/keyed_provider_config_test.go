package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func resetConfiguredProviders(t *testing.T) {
	t.Helper()
	configuredMu.Lock()
	configuredProviders = nil
	configuredFrozen = false
	configuredMu.Unlock()
	t.Cleanup(func() {
		configuredMu.Lock()
		configuredProviders = nil
		configuredFrozen = false
		configuredMu.Unlock()
	})
}

// A declared provider must route end to end without any code shipping for it:
// that is the whole point of declaring one.
func TestConfiguredProviderRoutesEndToEnd(t *testing.T) {
	resetConfiguredProviders(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("upstream path = %q, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer declared-key" {
			t.Fatalf("Authorization = %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	if err := ConfigureOpenAICompatibleProviders([]OpenAICompatibleProvider{
		{Name: "acme-relay", Aliases: []string{"acme"}, BaseURL: upstream.URL + "/v1"},
	}); err != nil {
		t.Fatalf("configure failed: %v", err)
	}

	store, err := session.NewStore(t.TempDir() + "/sessions.json")
	if err != nil {
		t.Fatal(err)
	}
	server := Server{
		Accounts: []accounts.Account{{
			ID: "acme-relay:main", Provider: accounts.Provider("acme-relay"),
			AuthMode: accounts.AuthModeAPIKey, Token: "declared-key",
		}},
		Sessions:     store,
		MaxBodyBytes: 1024,
	}
	req := httptest.NewRequest(http.MethodPost, "/acme-relay/v1/chat/completions", strings.NewReader(`{"model":"qwen3-max"}`))
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}

	// Its aliases and lookups behave like a built-in entry's.
	for _, name := range []string{"acme-relay", "acme"} {
		if got, ok := keyedProviderForName(name); !ok || string(got.Provider) != "acme-relay" {
			t.Fatalf("keyedProviderForName(%q) did not resolve", name)
		}
	}
	if !isKeyedProvider(accounts.Provider("acme-relay")) {
		t.Fatal("a declared provider must be a keyed provider")
	}
	// The standalone CLI cannot recover the serving process's alias map, so an
	// account stored under an alias must still join the canonical provider pool.
	aliasAccounts := []accounts.Account{{
		ID: "acme:main", Provider: accounts.Provider("acme"),
		AuthMode: accounts.AuthModeAPIKey, Token: "declared-key",
	}}
	if got := filterAccountsForProvider(aliasAccounts, accounts.Provider("acme-relay")); len(got) != 1 || got[0].Provider != accounts.Provider("acme-relay") {
		t.Fatalf("alias-stored accounts = %+v, want one canonical provider account", got)
	}
	statuses := server.withKeyedProviderHealth(context.Background(), []AccountUsageStatus{{
		AccountStatus: AccountStatus{ID: "acme-relay:main", Provider: accounts.Provider("acme-relay"), AuthMode: accounts.AuthModeAPIKey},
	}})
	if len(statuses) != 1 || !slices.Equal(statuses[0].ProviderEndpoints, []string{"/acme-relay"}) {
		t.Fatalf("declared-provider endpoints = %+v, want /acme-relay", statuses)
	}
}

func TestConfiguredProviderFailsOverAndCommitsStickyAccount(t *testing.T) {
	resetConfiguredProviders(t)
	var attempts []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.Header.Get("Authorization") {
		case "Bearer primary-key":
			attempts = append(attempts, "primary")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"quota exceeded"}}`)
		case "Bearer secondary-key":
			attempts = append(attempts, "secondary")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
		default:
			t.Error("upstream received an unexpected configured-provider key")
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer upstream.Close()

	if err := ConfigureOpenAICompatibleProviders([]OpenAICompatibleProvider{{
		Name: "acme-relay", BaseURL: upstream.URL + "/v1",
	}}); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	provider := accounts.Provider("acme-relay")
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	server := Server{
		Accounts: []accounts.Account{
			{ID: "acme-relay:a-primary", Provider: provider, AuthMode: accounts.AuthModeAPIKey, Token: "primary-key"},
			{ID: "acme-relay:z-secondary", Provider: provider, AuthMode: accounts.AuthModeAPIKey, Token: "secondary-key"},
		},
		Sessions: store, SchedulerRef: schedulerRef, MaxBodyBytes: 1024,
	}
	req := httptest.NewRequest(http.MethodPost, "/acme-relay/chat/completions", strings.NewReader(`{"model":"acme-model","messages":[]}`))
	req.Header.Set("X-Subrouter-Session", "acme-failover")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if want := []string{"primary", "secondary"}; !reflect.DeepEqual(attempts, want) {
		t.Fatalf("configured-provider attempts = %v, want %v", attempts, want)
	}
	if !schedulerRef.Get().Exhausted(provider, "acme-relay:a-primary") {
		t.Fatal("quota-limited configured-provider key was not exhausted")
	}
	assignment, ok := store.Get("acme-relay", "acme-failover")
	if !ok || assignment.AccountID != "acme-relay:z-secondary" {
		t.Fatalf("configured-provider sticky assignment = %+v, want secondary key", assignment)
	}
}

func TestUsageStatusProbesKeyThroughConfiguredUpstream(t *testing.T) {
	resetConfiguredProviders(t)
	var gotAuthorization string
	var healthRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healthRequests.Add(1)
		gotAuthorization = r.Header.Get("Authorization")
		if r.URL.Path != "/models" {
			t.Fatalf("health path = %q, want /models", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[{},{}]}`))
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	store := accounts.CodexStore{Dir: t.TempDir()}
	// Preserve a legacy endpoint-named record: health still comes from the
	// shared credential owner's Token Plan upstream.
	stored, _, err := store.AddProviderAPIKey(accounts.ProviderQwenAnthropic, "main", "test-provider-key")
	if err != nil {
		t.Fatal(err)
	}
	account, ok := stored.Account(stored.SourcePath(store))
	if !ok {
		t.Fatal("stored API key did not produce a routing account")
	}
	ref := NewAccountRef(store, []accounts.Account{account}, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	handler := Server{AccountRef: ref, AdminToken: "admin", QwenTokenUpstream: upstreamURL}.Handler()
	req := httptest.NewRequest(http.MethodGet, "/_subrouter/usage-status", nil)
	req.Header.Set("Authorization", "Bearer admin")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", resp.Code, resp.Body.String())
	}
	var statuses []AccountUsageStatus
	if err := json.Unmarshal(resp.Body.Bytes(), &statuses); err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Provider != accounts.ProviderQwenToken || statuses[0].ProviderHealth != "auth ok" || statuses[0].ProviderModels == nil || *statuses[0].ProviderModels != 2 || !slices.Equal(statuses[0].ProviderEndpoints, []string{"/qwen-anthropic", "/qwen-token"}) {
		t.Fatalf("usage statuses = %+v, want healthy key with 2 models", statuses)
	}
	if gotAuthorization != "Bearer test-provider-key" {
		t.Fatal("configured upstream did not receive the selected provider credential")
	}
	if got := healthRequests.Load(); got != 1 {
		t.Fatalf("configured upstream health requests = %d, want 1", got)
	}
}

func TestKeyedProviderHealthPreservesSpecificPlan(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	server := Server{
		Accounts: []accounts.Account{{
			ID: "qwen-token:main", Provider: accounts.ProviderQwenToken,
			AuthMode: accounts.AuthModeAPIKey, Token: "test-provider-key",
		}},
		QwenTokenUpstream: upstreamURL,
	}
	statuses := server.withKeyedProviderHealth(t.Context(), []AccountUsageStatus{{
		AccountStatus: AccountStatus{
			ID: "qwen-token:main", Provider: accounts.ProviderQwenToken,
			AuthMode: accounts.AuthModeAPIKey,
		},
		PlanType: "Pro",
	}})
	if len(statuses) != 1 || statuses[0].PlanType != "Pro" {
		t.Fatalf("enriched plan = %+v, want Pro", statuses)
	}
}

// A declared provider must not be able to shadow Codex, Claude, or a built-in,
// which would silently redirect traffic that already had a home.
func TestConfigureRejectsShadowingAnExistingProvider(t *testing.T) {
	resetConfiguredProviders(t)
	cases := []struct {
		name     string
		declared OpenAICompatibleProvider
	}{
		{name: "reserved provider name", declared: OpenAICompatibleProvider{Name: "claude", BaseURL: "https://x.test/v1"}},
		{name: "legacy API-key namespace", declared: OpenAICompatibleProvider{Name: "apikey", BaseURL: "https://x.test/v1"}},
		{name: "Bedrock outer route", declared: OpenAICompatibleProvider{Name: "bedrock", BaseURL: "https://x.test/v1"}},
		{name: "tenant outer route", declared: OpenAICompatibleProvider{Name: "t", BaseURL: "https://x.test/v1"}},
		{name: "internal outer route", declared: OpenAICompatibleProvider{Name: "internal", BaseURL: "https://x.test/v1"}},
		{name: "reserved path segment", declared: OpenAICompatibleProvider{Name: "v1", BaseURL: "https://x.test/v1"}},
		{name: "chat completions route", declared: OpenAICompatibleProvider{Name: "chat", BaseURL: "https://x.test/v1"}},
		{name: "legacy completions route", declared: OpenAICompatibleProvider{Name: "completions", BaseURL: "https://x.test/v1"}},
		{name: "embeddings route", declared: OpenAICompatibleProvider{Name: "embeddings", BaseURL: "https://x.test/v1"}},
		{name: "built-in name", declared: OpenAICompatibleProvider{Name: "kimi", BaseURL: "https://x.test/v1"}},
		{name: "built-in alias", declared: OpenAICompatibleProvider{Name: "fine", Aliases: []string{"glm"}, BaseURL: "https://x.test/v1"}},
		{name: "reserved alias", declared: OpenAICompatibleProvider{Name: "fine", Aliases: []string{"anthropic"}, BaseURL: "https://x.test/v1"}},
		{name: "legacy API-key alias", declared: OpenAICompatibleProvider{Name: "fine", Aliases: []string{"apikey"}, BaseURL: "https://x.test/v1"}},
		{name: "antigravity route", declared: OpenAICompatibleProvider{Name: "antigravity", BaseURL: "https://x.test/v1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ConfigureOpenAICompatibleProviders([]OpenAICompatibleProvider{tc.declared}); err == nil {
				t.Fatal("declaration should have been rejected")
			}
		})
	}
}

func TestConfigureRejectsMalformedDeclarations(t *testing.T) {
	resetConfiguredProviders(t)
	cases := []struct {
		name     string
		declared OpenAICompatibleProvider
	}{
		{name: "no name", declared: OpenAICompatibleProvider{BaseURL: "https://x.test/v1"}},
		{name: "no base url", declared: OpenAICompatibleProvider{Name: "thing"}},
		{name: "name with a slash", declared: OpenAICompatibleProvider{Name: "a/b", BaseURL: "https://x.test/v1"}},
		{name: "name with a colon", declared: OpenAICompatibleProvider{Name: "apikey:team", BaseURL: "https://x.test/v1"}},
		{name: "hidden storage name", declared: OpenAICompatibleProvider{Name: ".acme", BaseURL: "https://x.test/v1"}},
		{name: "alias with a slash", declared: OpenAICompatibleProvider{Name: "thing", Aliases: []string{"a/b"}, BaseURL: "https://x.test/v1"}},
		{name: "alias with a colon", declared: OpenAICompatibleProvider{Name: "thing", Aliases: []string{"apikey:team"}, BaseURL: "https://x.test/v1"}},
		{name: "hidden storage alias", declared: OpenAICompatibleProvider{Name: "thing", Aliases: []string{".acme"}, BaseURL: "https://x.test/v1"}},
		{name: "alias with whitespace", declared: OpenAICompatibleProvider{Name: "thing", Aliases: []string{"a b"}, BaseURL: "https://x.test/v1"}},
		{name: "alias with control", declared: OpenAICompatibleProvider{Name: "thing", Aliases: []string{"a\nb"}, BaseURL: "https://x.test/v1"}},
		{name: "alias with URL delimiter", declared: OpenAICompatibleProvider{Name: "thing", Aliases: []string{"a?b"}, BaseURL: "https://x.test/v1"}},
		{name: "oversized name", declared: OpenAICompatibleProvider{Name: strings.Repeat("a", 64), BaseURL: "https://x.test/v1"}},
		{name: "non-http scheme", declared: OpenAICompatibleProvider{Name: "thing", BaseURL: "ftp://x.test/v1"}},
		{name: "no host", declared: OpenAICompatibleProvider{Name: "thing", BaseURL: "https:///v1"}},
		{name: "URL userinfo", declared: OpenAICompatibleProvider{Name: "thing", BaseURL: "https://user:password@x.test/v1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ConfigureOpenAICompatibleProviders([]OpenAICompatibleProvider{tc.declared}); err == nil {
				t.Fatal("declaration should have been rejected")
			}
		})
	}
	if err := ConfigureOpenAICompatibleProviders([]OpenAICompatibleProvider{
		{Name: "dupe", BaseURL: "https://a.test/v1"},
		{Name: "dupe", BaseURL: "https://b.test/v1"},
	}); err == nil {
		t.Fatal("two declarations of the same name should have been rejected")
	}
}

func TestValidDeclaredProviderNameRejectsStorageAndRoutingNamespaces(t *testing.T) {
	for _, value := range []string{
		"apikey", "apikey:team", "codex", "anthropic", "qwen", "a/b", "a b",
		"models", "alpha", "ps", "plugins", "chat", "completions", "embeddings",
	} {
		if ValidDeclaredProviderName(value) {
			t.Fatalf("ValidDeclaredProviderName(%q) = true, want false", value)
		}
	}
	if !ValidDeclaredProviderName("acme-relay") {
		t.Fatal("a non-reserved provider identifier should be valid")
	}
	if !ValidDeclaredProviderName(strings.Repeat("a", 63)) {
		t.Fatal("a 63-byte provider identifier should fit the credential store")
	}
}

// Configuring replaces rather than accumulates, so a caller invoked twice does
// not end up with duplicate entries.
func TestConfigureReplacesPreviousDeclarations(t *testing.T) {
	resetConfiguredProviders(t)
	if err := ConfigureOpenAICompatibleProviders([]OpenAICompatibleProvider{{Name: "first", BaseURL: "https://a.test/v1"}}); err != nil {
		t.Fatal(err)
	}
	if err := ConfigureOpenAICompatibleProviders([]OpenAICompatibleProvider{{Name: "second", BaseURL: "https://b.test/v1"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := keyedProviderForName("first"); ok {
		t.Fatal("the earlier declaration should have been replaced")
	}
	if _, ok := keyedProviderForName("second"); !ok {
		t.Fatal("the current declaration should resolve")
	}
	if err := ConfigureOpenAICompatibleProviders([]OpenAICompatibleProvider{{Name: "second", BaseURL: "https://b.test/v1"}}); err != nil {
		t.Fatalf("re-applying the same declaration must be accepted: %v", err)
	}
	// A failed declaration must not disturb what is already configured.
	if err := ConfigureOpenAICompatibleProviders([]OpenAICompatibleProvider{{Name: "claude", BaseURL: "https://c.test/v1"}}); err == nil {
		t.Fatal("expected rejection")
	}
	if _, ok := keyedProviderForName("second"); !ok {
		t.Fatal("a rejected declaration must leave the previous configuration intact")
	}
}

func TestConfigureRejectsChangesAfterHandlerStarts(t *testing.T) {
	resetConfiguredProviders(t)
	if err := ConfigureOpenAICompatibleProviders([]OpenAICompatibleProvider{{Name: "first", BaseURL: "https://a.test/v1"}}); err != nil {
		t.Fatal(err)
	}
	_ = (Server{}).Handler()
	if err := ConfigureOpenAICompatibleProviders([]OpenAICompatibleProvider{{Name: "second", BaseURL: "https://b.test/v1"}}); err == nil {
		t.Fatal("configuration after Handler starts must be rejected")
	}
}

func TestParseOpenAICompatibleFlag(t *testing.T) {
	declared, err := ParseOpenAICompatibleFlag("acme-relay|acme=https://coding.example/v1")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if declared.Name != "acme-relay" {
		t.Fatalf("Name = %q", declared.Name)
	}
	if len(declared.Aliases) != 1 || declared.Aliases[0] != "acme" {
		t.Fatalf("Aliases = %v", declared.Aliases)
	}
	if declared.BaseURL != "https://coding.example/v1" {
		t.Fatalf("BaseURL = %q", declared.BaseURL)
	}
	// A base URL containing '=' still parses, because only the first is a
	// separator.
	if declared, err = ParseOpenAICompatibleFlag("x=https://h.test/v1?a=b"); err != nil || declared.BaseURL != "https://h.test/v1?a=b" {
		t.Fatalf("query strings must survive: %+v %v", declared, err)
	}
	if _, err := ParseOpenAICompatibleFlag("no-equals-sign"); err == nil {
		t.Fatal("a declaration without = must be rejected")
	}
}

func TestConfigureOpenAICompatibleProvidersRejectsRemoteCleartext(t *testing.T) {
	resetConfiguredProviders(t)

	err := ConfigureOpenAICompatibleProviders([]OpenAICompatibleProvider{{
		Name: "cleartext", BaseURL: "http://provider.example/v1",
	}})
	if err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("remote cleartext base URL error = %v, want HTTPS rejection", err)
	}
	if err := ConfigureOpenAICompatibleProviders([]OpenAICompatibleProvider{{
		Name: "local", BaseURL: "http://127.0.0.1:8080/v1",
	}}); err != nil {
		t.Fatalf("loopback development base URL rejected: %v", err)
	}
}

// A vendor that exposes one subscription on two protocol endpoints must not
// require the key to be stored twice. Registering it once against the owning
// provider has to serve both entries, or rotating the key means editing every
// copy and account listings double-count one subscription.
func TestOneSubscriptionServesBothProtocolEntries(t *testing.T) {
	if got := accountProviderFor(accounts.ProviderQwenAnthropic); got != accounts.ProviderQwenToken {
		t.Fatalf("accountProviderFor(qwen-anthropic) = %q, want qwen-token", got)
	}
	if got := accountProviderFor(accounts.ProviderQwenToken); got != accounts.ProviderQwenToken {
		t.Fatalf("a provider that owns its accounts must resolve to itself, got %q", got)
	}
	if got := accountProviderFor(accounts.ProviderCodex); got != accounts.ProviderCodex {
		t.Fatalf("a non-registry provider must resolve to itself, got %q", got)
	}

	// One stored account, under the owning provider only.
	stored := []accounts.Account{{
		ID: "qwen-token:main", Provider: accounts.ProviderQwenToken,
		AuthMode: accounts.AuthModeAPIKey, Token: "sk-sp-one",
	}}
	for _, provider := range []accounts.Provider{accounts.ProviderQwenToken, accounts.ProviderQwenAnthropic} {
		got := filterAccountsForProvider(stored, provider)
		if len(got) != 1 || got[0].Token != "sk-sp-one" {
			t.Fatalf("provider %q selected %d accounts, want the single shared credential", provider, len(got))
		}
		// The selected account must carry the REQUESTED provider, since
		// upstream selection and path rewriting key on it. Carrying the
		// credential owner's provider routes the Anthropic endpoint through the
		// OpenAI upstream and 404s.
		if got[0].Provider != provider {
			t.Fatalf("selected account carries provider %q, want the requested %q", got[0].Provider, provider)
		}
		if got[0].ID != "qwen-token:main" {
			t.Fatalf("account id changed to %q; stickiness and forced selection key on it", got[0].ID)
		}
	}
	// A credential stored against the protocol endpoint rather than the owning
	// provider must still be found, or a key added that way is silently
	// orphaned.
	underEndpoint := []accounts.Account{{
		ID: "qwen-anthropic:main", Provider: accounts.ProviderQwenAnthropic,
		AuthMode: accounts.AuthModeAPIKey, Token: "sk-sp-endpoint",
	}}
	for _, provider := range []accounts.Provider{accounts.ProviderQwenAnthropic, accounts.ProviderQwenToken} {
		if got := filterAccountsForProvider(underEndpoint, provider); len(got) != 1 {
			t.Fatalf("provider %q found %d accounts stored under the endpoint name, want 1", provider, len(got))
		}
	}

	// Stamping must not mutate the caller's slice.
	if stored[0].Provider != accounts.ProviderQwenToken {
		t.Fatalf("the stored account was mutated to %q", stored[0].Provider)
	}

	// Sharing must not leak across unrelated providers.
	if got := filterAccountsForProvider(stored, accounts.ProviderGrok); len(got) != 0 {
		t.Fatalf("grok selected %d qwen accounts, want none", len(got))
	}
	if got := filterAccountsForProvider(stored, accounts.ProviderCodex); len(got) != 0 {
		t.Fatalf("codex selected %d qwen accounts, want none", len(got))
	}
}

func TestQwenAnthropicSchedulingUsesSharedTokenPlanSessionLoad(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("qwen-anthropic", "existing", "qwen-token:a-busy", ""); err != nil {
		t.Fatal(err)
	}
	scheduler := selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "qwen-token:a-busy", Provider: accounts.ProviderQwenToken, Headroom: 1, ShortHeadroom: 1},
		{AccountID: "qwen-token:z-idle", Provider: accounts.ProviderQwenToken, Headroom: 1, ShortHeadroom: 1},
	}).WithSessionCounts(SchedulerSessionCounts(store))
	candidates := filterAccountsForProvider([]accounts.Account{
		{ID: "qwen-token:a-busy", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey, Token: "busy"},
		{ID: "qwen-token:z-idle", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey, Token: "idle"},
	}, accounts.ProviderQwenAnthropic)
	picked, err := scheduler.PickBest(SchedulerAccounts(candidates))
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "qwen-token:z-idle" {
		t.Fatalf("shared Token Plan session load picked %q, want idle account", picked.ID)
	}
	candidate := candidates[0]
	if normalized := schedulerAccount(candidate); normalized.Provider != accounts.ProviderQwenToken {
		t.Fatalf("scheduler provider = %q, want Token Plan owner", normalized.Provider)
	}
	if candidate.Provider != accounts.ProviderQwenAnthropic {
		t.Fatalf("transport provider = %q, want Anthropic endpoint", candidate.Provider)
	}
}
