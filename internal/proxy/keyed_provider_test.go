package proxy

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// Every registry entry has to be complete: a half-filled row silently routes to
// an empty upstream or advertises an empty lease path, which is exactly the
// class of mistake the registry exists to prevent.
func TestKeyedProviderRegistryEntriesAreComplete(t *testing.T) {
	seenProvider := map[accounts.Provider]bool{}
	seenPrefix := map[string]bool{}
	seenName := map[string]bool{}

	for _, entry := range keyedProviders() {
		name := string(entry.Provider)
		if name == "" || entry.PathPrefix == "" || entry.PlanLabel == "" ||
			entry.LeaseAPI == "" || entry.LeasePath == "" || entry.Upstream == nil {
			t.Fatalf("incomplete registry entry: %+v", entry)
		}
		if !strings.HasPrefix(entry.LeasePath, "/"+entry.PathPrefix+"/") {
			t.Fatalf("provider %s lease path %q is not under its own path prefix", name, entry.LeasePath)
		}
		if seenProvider[entry.Provider] {
			t.Fatalf("provider %s is registered twice", name)
		}
		seenProvider[entry.Provider] = true
		if seenPrefix[entry.PathPrefix] {
			t.Fatalf("path prefix %q is claimed twice", entry.PathPrefix)
		}
		seenPrefix[entry.PathPrefix] = true
		for _, candidate := range append([]string{name}, entry.Aliases...) {
			if seenName[candidate] {
				t.Fatalf("provider name %q is claimed twice", candidate)
			}
			seenName[candidate] = true
		}
	}
}

// Each lookup is a routing decision, so each must round-trip for every entry.
func TestKeyedProviderLookupsRoundTrip(t *testing.T) {
	for _, entry := range keyedProviders() {
		name := string(entry.Provider)

		if got, ok := keyedProviderFor(entry.Provider); !ok || got.PathPrefix != entry.PathPrefix {
			t.Fatalf("keyedProviderFor(%s) did not round-trip", name)
		}
		if got, ok := keyedProviderForPathPrefix(entry.PathPrefix); !ok || got.Provider != entry.Provider {
			t.Fatalf("keyedProviderForPathPrefix(%q) did not resolve to %s", entry.PathPrefix, name)
		}
		for _, candidate := range append([]string{name}, entry.Aliases...) {
			if got, ok := keyedProviderForName(candidate); !ok || got.Provider != entry.Provider {
				t.Fatalf("keyedProviderForName(%q) did not resolve to %s", candidate, name)
			}
		}
		if entry.ModelPrefix != "" {
			if got, ok := keyedProviderForModelPrefix(entry.ModelPrefix + "test"); !ok || got.Provider != entry.Provider {
				t.Fatalf("model prefix %q did not resolve to %s", entry.ModelPrefix, name)
			}
		}
		if !isKeyedProvider(entry.Provider) {
			t.Fatalf("%s should be a keyed provider", name)
		}
	}

	if isKeyedProvider(accounts.ProviderCodex) || isKeyedProvider(accounts.ProviderClaude) {
		t.Fatal("codex and claude are not path-prefixed API-key providers")
	}
	if _, ok := keyedProviderForPathPrefix("v1"); ok {
		t.Fatal("a bare API path must not resolve to a provider")
	}
	if _, ok := keyedProviderForName("gemini"); ok {
		t.Fatal("an unregistered provider name must not resolve")
	}
}

func TestValidateCredentialUpstreamsRejectsNonLoopbackCleartext(t *testing.T) {
	t.Parallel()

	server := Server{
		OpenRouterUpstream: mustParseURL(t, "http://openrouter.example/v1"),
	}
	if err := server.ValidateCredentialUpstreams(); err == nil || !strings.Contains(err.Error(), "OpenRouter") || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("ValidateCredentialUpstreams() error = %v, want OpenRouter HTTPS error", err)
	}
}

func TestValidateCredentialUpstreamsAllowsHTTPSAndLoopbackHTTP(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"https://openrouter.example/v1",
		"http://127.0.0.1:8080/v1",
		"http://[::1]:8080/v1",
		"http://localhost:8080/v1",
	} {
		t.Run(raw, func(t *testing.T) {
			server := Server{OpenRouterUpstream: mustParseURL(t, raw)}
			if err := server.ValidateCredentialUpstreams(); err != nil {
				t.Fatalf("ValidateCredentialUpstreams() error = %v", err)
			}
		})
	}
}

func TestValidateCredentialUpstreamsRejectsMissingHost(t *testing.T) {
	t.Parallel()

	server := Server{OpenRouterUpstream: mustParseURL(t, "https:///v1")}
	if err := server.ValidateCredentialUpstreams(); err == nil || !strings.Contains(err.Error(), "host") {
		t.Fatalf("ValidateCredentialUpstreams() error = %v, want missing-host error", err)
	}
}

func TestValidateCredentialUpstreamsIncludesAntigravityOAuth(t *testing.T) {
	t.Parallel()

	server := Server{
		AntigravityUpstream: mustParseURL(t, "http://cloudcode.example"),
	}
	if err := server.ValidateCredentialUpstreams(); err == nil || !strings.Contains(err.Error(), "Antigravity") {
		t.Fatalf("ValidateCredentialUpstreams() error = %v, want Antigravity upstream error", err)
	}
}

func TestAPIKeyProviderListNamesEveryProvider(t *testing.T) {
	list := APIKeyProviderList()
	for _, want := range append([]string{"codex", "claude"}, keyedProviderNames()...) {
		if !strings.Contains(list, want) {
			t.Fatalf("provider list %q omits %q", list, want)
		}
	}
	if !strings.Contains(list, ", or ") {
		t.Fatalf("provider list %q should read as a sentence", list)
	}
}

func TestProviderHealthURLRequiresTheConfiguredUpstream(t *testing.T) {
	if got := ProviderHealthURL(accounts.ProviderQwenToken, ""); got != "" {
		t.Fatalf("unknown configured upstream produced health URL %q; this could disclose a gateway key", got)
	}
	if got := ProviderHealthURL(accounts.ProviderQwenToken, "https://gateway.example/v1"); got != "https://gateway.example/v1/models" {
		t.Fatalf("configured gateway health URL = %q", got)
	}
	if got := ProviderHealthURL(accounts.ProviderQwenToken, "https://gateway.example/v1?tenant=alpha#ignored"); got != "https://gateway.example/v1/models?tenant=alpha" {
		t.Fatalf("configured gateway health URL with query = %q", got)
	}
}

func TestApplyKeyedProviderAuthPresentsTheKeyPerStyle(t *testing.T) {
	account := accounts.Account{Provider: accounts.ProviderZAI, AuthMode: accounts.AuthModeAPIKey, Token: "key"}

	bearer := http.Header{}
	bearer.Set("X-Api-Key", "client-supplied")
	applyKeyedProviderAuth(bearer, account, keyedProvider{Auth: authBearer}, "")
	if bearer.Get("X-Api-Key") != "" {
		t.Fatal("a bearer provider must not forward a client X-Api-Key")
	}

	anthropic := http.Header{}
	applyKeyedProviderAuth(anthropic, account, keyedProvider{Auth: authBearerAndAnthropicKey}, "")
	if anthropic.Get("X-Api-Key") != "key" {
		t.Fatalf("X-Api-Key = %q, want the account key", anthropic.Get("X-Api-Key"))
	}
	if anthropic.Get("Authorization") != "Bearer key" {
		t.Fatalf("Authorization = %q, want the bearer form", anthropic.Get("Authorization"))
	}
	if anthropic.Get("Anthropic-Version") != "2023-06-01" {
		t.Fatalf("Anthropic-Version = %q, want a default", anthropic.Get("Anthropic-Version"))
	}

	explicit := http.Header{}
	explicit.Set("Anthropic-Version", "2024-01-01")
	applyKeyedProviderAuth(explicit, account, keyedProvider{Auth: authBearerAndAnthropicKey}, "")
	if explicit.Get("Anthropic-Version") != "2024-01-01" {
		t.Fatal("a client's Anthropic-Version must not be overwritten")
	}
}

func TestCollapseDuplicateVersionSegment(t *testing.T) {
	versioned := &url.URL{Scheme: "https", Host: "example.test", Path: "/api/v1"}
	bare := &url.URL{Scheme: "https", Host: "example.test"}
	cases := []struct {
		name     string
		path     string
		upstream *url.URL
		want     string
	}{
		{name: "collapses against a versioned base", path: "/v1/chat/completions", upstream: versioned, want: "/chat/completions"},
		{name: "collapses a bare version path", path: "/v1", upstream: versioned, want: "/"},
		{name: "keeps the version against a bare base", path: "/v1/chat/completions", upstream: bare, want: "/v1/chat/completions"},
		{name: "leaves an unversioned path alone", path: "/chat/completions", upstream: versioned, want: "/chat/completions"},
		{name: "tolerates a nil upstream", path: "/v1/chat/completions", upstream: nil, want: "/v1/chat/completions"},
		{name: "does not collapse a lookalike segment", path: "/v10/chat", upstream: versioned, want: "/v10/chat"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := collapseDuplicateVersionSegment(tc.path, tc.upstream); got != tc.want {
				t.Fatalf("collapseDuplicateVersionSegment(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestProviderUpstreamUsesDefaultAuthority(t *testing.T) {
	for _, test := range []struct {
		name     string
		upstream string
		want     bool
	}{
		{name: "documented upstream", upstream: "https://api.kimi.com/coding/v1", want: true},
		{name: "explicit default port", upstream: "https://API.KIMI.COM:443/gateway", want: true},
		{name: "custom gateway", upstream: "https://custom.kimi.test/coding/v1", want: false},
		{name: "different port", upstream: "https://api.kimi.com:8443/coding/v1", want: false},
		{name: "cleartext", upstream: "http://api.kimi.com/coding/v1", want: false},
		{name: "missing", upstream: "", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := providerUpstreamUsesDefaultAuthority(accounts.ProviderKimi, test.upstream); got != test.want {
				t.Fatalf("providerUpstreamUsesDefaultAuthority(%q) = %v, want %v", test.upstream, got, test.want)
			}
		})
	}
}

// A provider that addresses models as vendor/model must keep the whole id,
// while every other provider keeps reading the leading segment as a provider
// selector. OpenRouter uses this flag; the synthetic entry below also proves
// the behavior independently of that registry row.
func TestSessionLeaseProviderHonoursVendorPrefixedModels(t *testing.T) {
	provider, model, err := sessionLeaseProvider("claude", "anthropic/claude-opus-5")
	if err != nil || provider != accounts.ProviderClaude || model != "claude-opus-5" {
		t.Fatalf("got (%q, %q, %v), want (claude, claude-opus-5, nil)", provider, model, err)
	}
	if _, _, err := sessionLeaseProvider("claude", "openai/gpt-5"); err == nil {
		t.Fatal("a mismatched model provider must still be rejected")
	}

	original := builtinKeyedProviders
	t.Cleanup(func() { builtinKeyedProviders = original })
	builtinKeyedProviders = append(append([]keyedProvider{}, original...), keyedProvider{
		Provider:             accounts.Provider("vendorcase"),
		PathPrefix:           "vendorcase",
		PlanLabel:            "vendorcase api key",
		Auth:                 authBearer,
		VendorPrefixedModels: true,
		LeaseAPI:             "openai-completions",
		LeasePath:            "/vendorcase/chat/completions",
		LeaseEnv:             leaseEnvOpenAI,
		Upstream:             func(Server, accounts.AuthMode) *url.URL { return nil },
	})

	provider, model, err = sessionLeaseProvider("vendorcase", "anthropic/claude-opus-5")
	if err != nil {
		t.Fatalf("a vendor-prefixed model must be accepted: %v", err)
	}
	if provider != accounts.Provider("vendorcase") || model != "anthropic/claude-opus-5" {
		t.Fatalf("got (%q, %q), want the vendor prefix preserved", provider, model)
	}
	provider, model, err = sessionLeaseProvider("", "vendorcase/vendor-model")
	if err != nil || provider != accounts.Provider("vendorcase") || model != "vendorcase/vendor-model" {
		t.Fatalf("inferred provider got (%q, %q, %v), want the full vendor-prefixed model", provider, model, err)
	}
}
