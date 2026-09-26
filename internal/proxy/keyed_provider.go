package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// ValidateCredentialUpstreams rejects configurations that could send a
// selected account credential over cleartext. HTTP remains available on
// loopback for local development and tests; every remote upstream must use
// HTTPS. Every URL field on Server is an upstream, so walking those fields
// keeps the startup gate complete when a provider adds another upstream.
func (s Server) ValidateCredentialUpstreams() error {
	value := reflect.ValueOf(s)
	typeOfURL := reflect.TypeOf((*url.URL)(nil))
	for i := 0; i < value.NumField(); i++ {
		field := value.Type().Field(i)
		if field.Type != typeOfURL || !strings.HasSuffix(field.Name, "Upstream") {
			continue
		}
		upstream, _ := value.Field(i).Interface().(*url.URL)
		if err := validateCredentialUpstream(field.Name, upstream); err != nil {
			return err
		}
	}
	return nil
}

func validateCredentialUpstream(name string, upstream *url.URL) error {
	if upstream == nil {
		return nil
	}
	host := strings.TrimSpace(upstream.Hostname())
	if host == "" {
		return fmt.Errorf("credential-bearing upstream %s must include a host", name)
	}
	if strings.EqualFold(upstream.Scheme, "https") {
		return nil
	}
	ip := net.ParseIP(host)
	loopback := strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
	if strings.EqualFold(upstream.Scheme, "http") && loopback {
		return nil
	}
	return fmt.Errorf("credential-bearing upstream %s must use HTTPS, except HTTP on loopback", name)
}

// authStyle names how a provider expects its API key to be presented.
type authStyle int

const (
	// authBearer sends Authorization: Bearer <key> and removes any client
	// X-Api-Key, which is the OpenAI-compatible convention.
	authBearer authStyle = iota
	// authBearerWithAnthropicVersion keeps bearer-only authentication while
	// supplying the protocol version required by Anthropic-shaped endpoints.
	authBearerWithAnthropicVersion
	// authBearerAndAnthropicKey sends both Authorization and X-Api-Key, and
	// defaults Anthropic-Version, for providers exposing an Anthropic-shaped API.
	authBearerAndAnthropicKey
)

// leaseEnvStyle names which SDK environment variables a lease hands to the
// sandbox so the client library points back at this proxy.
type leaseEnvStyle int

const (
	leaseEnvOpenAI leaseEnvStyle = iota
	leaseEnvAnthropic
)

// keyedProvider describes an API-key provider that the proxy routes by path
// prefix. Everything the router needs about such a provider lives in one entry,
// so adding a provider is a table row rather than an edit to a dozen switch
// statements that are easy to leave half-updated.
type keyedProvider struct {
	Provider accounts.Provider
	// PathPrefix is the first path segment that selects this provider, so
	// /<PathPrefix>/... routes here and the segment is stripped upstream.
	PathPrefix string
	// Aliases are the additional names accepted wherever a provider is named by
	// a human or an API caller. The canonical provider string is always accepted.
	Aliases []string
	// ModelPrefix infers this provider from a bare model id, e.g. "glm-" for ZAI.
	// Empty means no inference.
	ModelPrefix string
	// PlanLabel is how an API-key account of this provider is described in
	// account listings.
	PlanLabel string
	Auth      authStyle
	// CollapseVersionSegment marks a provider whose upstream base URL may already
	// end in /v1, so a client's own /v1 must not be forwarded twice.
	CollapseVersionSegment bool
	// VendorPrefixedModels marks a provider that addresses models as
	// vendor/model, where the segment before the slash belongs to the model id
	// rather than naming a provider.
	VendorPrefixedModels bool
	// LeaseAPI is the Pi adapter a lease advertises for this provider.
	LeaseAPI string
	// LeasePath is the single path a lease for this provider may call.
	LeasePath string
	LeaseEnv  leaseEnvStyle
	// Metering describes how the vendor charges this provider, so an operator
	// can tell an invocation-metered plan from a credit-metered one without
	// leaving the terminal. Vendors in this table expose no quota API, so this
	// is the only plan information available.
	Metering string
	// HealthPath is a GET that proves a key still works, relative to the
	// upstream base. Empty means the provider offers no such endpoint and its
	// state cannot be probed.
	HealthPath string
	// DefaultUpstream is the vendor's documented base URL for this provider. It
	// is the flag default and the address a health probe uses, so the two
	// cannot drift apart.
	DefaultUpstream string
	// AccountProvider names the provider whose accounts serve this one. A
	// vendor that exposes one subscription on two protocol endpoints gets an
	// entry per endpoint, but the subscription is still a single credential:
	// without this, the same key has to be stored once per entry, rotating it
	// means editing every copy, and account listings double-count one
	// subscription. Empty means the provider owns its own accounts.
	AccountProvider accounts.Provider
	// Upstream reads this provider's configured base URL off the server for
	// the account's auth mode. Most providers have one upstream regardless of
	// mode; a provider whose subscription and API-key traffic terminate at
	// different hosts (Grok: cli-chat-proxy.grok.com vs api.x.ai) switches on
	// the mode rather than growing a second registry entry for one provider.
	Upstream func(s Server, mode accounts.AuthMode) *url.URL
}

// builtinKeyedProviders are the providers this build ships. Adding one means
// adding an entry here and a base-URL field on Server; the routing, auth,
// lease, import, and CLI paths all read from the registry. Operators can
// declare further OpenAI-compatible providers at startup — see
// ConfigureOpenAICompatibleProviders.
var builtinKeyedProviders = []keyedProvider{
	{
		Provider:               accounts.ProviderKimi,
		DefaultUpstream:        "https://api.kimi.com/coding/v1",
		Metering:               "subscription, per request",
		HealthPath:             "/models",
		PathPrefix:             "kimi",
		Aliases:                []string{"kimi-for-coding"},
		ModelPrefix:            "kimi-",
		PlanLabel:              "kimi api key",
		Auth:                   authBearerAndAnthropicKey,
		CollapseVersionSegment: true,
		LeaseAPI:               "anthropic-messages",
		LeasePath:              "/kimi/v1/messages",
		LeaseEnv:               leaseEnvAnthropic,
		Upstream:               func(s Server, _ accounts.AuthMode) *url.URL { return s.KimiUpstream },
	},
	{
		Provider:        accounts.ProviderZAI,
		DefaultUpstream: "https://api.z.ai/api/coding/paas/v4",
		Metering:        "subscription, per request",
		HealthPath:      "/models",
		PathPrefix:      "zai",
		Aliases:         []string{"glm", "glm-5.2"},
		ModelPrefix:     "glm-",
		PlanLabel:       "zai api key",
		Auth:            authBearer,
		LeaseAPI:        "openai-completions",
		LeasePath:       "/zai/chat/completions",
		LeaseEnv:        leaseEnvOpenAI,
		Upstream:        func(s Server, _ accounts.AuthMode) *url.URL { return s.ZAIUpstream },
	},
	{
		Provider:        accounts.ProviderOpenRouter,
		DefaultUpstream: "https://openrouter.ai/api/v1",
		Metering:        "credits, per token",
		HealthPath:      "/key",
		PathPrefix:      "openrouter",
		Aliases:         []string{"open-router"},
		PlanLabel:       "openrouter api key",
		Auth:            authBearer,
		// OpenRouter's base already ends in /v1 and it addresses every model as
		// vendor/model, e.g. anthropic/claude-opus-5.
		CollapseVersionSegment: true,
		VendorPrefixedModels:   true,
		LeaseAPI:               "openai-completions",
		LeasePath:              "/openrouter/chat/completions",
		LeaseEnv:               leaseEnvOpenAI,
		Upstream:               func(s Server, _ accounts.AuthMode) *url.URL { return s.OpenRouterUpstream },
	},
	{
		Provider:               accounts.ProviderDeepSeek,
		DefaultUpstream:        "https://api.deepseek.com",
		Metering:               "credits, per token",
		HealthPath:             "/models",
		PathPrefix:             "deepseek",
		ModelPrefix:            "deepseek-",
		PlanLabel:              "deepseek api key",
		Auth:                   authBearer,
		CollapseVersionSegment: true,
		LeaseAPI:               "openai-completions",
		LeasePath:              "/deepseek/chat/completions",
		LeaseEnv:               leaseEnvOpenAI,
		Upstream:               func(s Server, _ accounts.AuthMode) *url.URL { return s.DeepSeekUpstream },
	},
	{
		Provider:               accounts.ProviderTogether,
		DefaultUpstream:        "https://api.together.ai/v1",
		Metering:               "credits, per token",
		HealthPath:             "/models",
		PathPrefix:             "together",
		Aliases:                []string{"together-ai"},
		PlanLabel:              "together api key",
		Auth:                   authBearer,
		CollapseVersionSegment: true,
		VendorPrefixedModels:   true,
		LeaseAPI:               "openai-completions",
		LeasePath:              "/together/chat/completions",
		LeaseEnv:               leaseEnvOpenAI,
		Upstream:               func(s Server, _ accounts.AuthMode) *url.URL { return s.TogetherUpstream },
	},
	{
		Provider:               accounts.ProviderFireworks,
		DefaultUpstream:        "https://api.fireworks.ai/inference/v1",
		Metering:               "credits, per token",
		HealthPath:             "/models",
		PathPrefix:             "fireworks",
		Aliases:                []string{"fireworks-ai"},
		PlanLabel:              "fireworks api key",
		Auth:                   authBearer,
		CollapseVersionSegment: true,
		VendorPrefixedModels:   true,
		LeaseAPI:               "openai-completions",
		LeasePath:              "/fireworks/chat/completions",
		LeaseEnv:               leaseEnvOpenAI,
		Upstream:               func(s Server, _ accounts.AuthMode) *url.URL { return s.FireworksUpstream },
	},
	{
		Provider:               accounts.ProviderOpenCodeZen,
		DefaultUpstream:        "https://opencode.ai/zen/v1",
		Metering:               "credits, per request",
		HealthPath:             "/models",
		PathPrefix:             "opencode-zen",
		Aliases:                []string{"zen"},
		PlanLabel:              "opencode zen api key",
		Auth:                   authBearer,
		CollapseVersionSegment: true,
		LeaseAPI:               "openai-completions",
		LeasePath:              "/opencode-zen/chat/completions",
		LeaseEnv:               leaseEnvOpenAI,
		Upstream:               func(s Server, _ accounts.AuthMode) *url.URL { return s.OpenCodeZenUpstream },
	},
	{
		Provider:        accounts.ProviderGrok,
		DefaultUpstream: "https://api.x.ai/v1",
		Metering:        "api key, per token",
		HealthPath:      "/models",
		PathPrefix:      "grok",
		Aliases:         []string{"xai", "x-ai"},
		ModelPrefix:     "grok-",
		PlanLabel:       "grok api key",
		Auth:            authBearer,
		// api.x.ai/v1 already ends in /v1. xAI model ids are bare (grok-4), so
		// the vendor/model provider-selector rule still applies.
		CollapseVersionSegment: true,
		LeaseAPI:               "openai-completions",
		LeasePath:              "/grok/chat/completions",
		LeaseEnv:               leaseEnvOpenAI,
		// A subscription reached through device-code OAuth is served by the
		// Grok CLI's chat proxy, not the API-key host; the cli-chat-proxy base
		// also ends in /v1, so the collapse above stays right for both.
		Upstream: func(s Server, mode accounts.AuthMode) *url.URL {
			if mode == accounts.AuthModeOAuth {
				return s.GrokSubscriptionUpstream
			}
			return s.GrokUpstream
		},
	},
	{
		Provider:        accounts.ProviderQwen,
		DefaultUpstream: "https://coding-intl.dashscope.aliyuncs.com/v1",
		Metering:        "coding plan, per invocation",
		HealthPath:      "/models",
		PathPrefix:      "qwen",
		Aliases:         []string{"dashscope", "modelstudio"},
		ModelPrefix:     "qwen",
		PlanLabel:       "qwen coding plan key",
		Auth:            authBearer,
		// The Coding Plan endpoint already ends in /v1.
		CollapseVersionSegment: true,
		LeaseAPI:               "openai-completions",
		LeasePath:              "/qwen/chat/completions",
		LeaseEnv:               leaseEnvOpenAI,
		Upstream:               func(s Server, _ accounts.AuthMode) *url.URL { return s.QwenUpstream },
	},
	{
		Provider:        accounts.ProviderQwenToken,
		DefaultUpstream: "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1",
		Metering:        "token plan",
		HealthPath:      "/models",
		PathPrefix:      "qwen-token",
		Aliases:         []string{"tokenplan", "qwen-tokenplan"},
		PlanLabel:       "qwen token plan key",
		Auth:            authBearer,
		// The Token Plan endpoint ends in /compatible-mode/v1.
		CollapseVersionSegment: true,
		LeaseAPI:               "openai-completions",
		LeasePath:              "/qwen-token/chat/completions",
		LeaseEnv:               leaseEnvOpenAI,
		Upstream:               func(s Server, _ accounts.AuthMode) *url.URL { return s.QwenTokenUpstream },
	},
	{
		Provider:        accounts.ProviderQwenAnthropic,
		DefaultUpstream: "https://token-plan.ap-southeast-1.maas.aliyuncs.com/apps/anthropic",
		Metering:        "token plan",
		HealthPath:      "",
		// One Token Plan subscription, reachable over two protocols.
		AccountProvider: accounts.ProviderQwenToken,
		PathPrefix:      "qwen-anthropic",
		Aliases:         []string{"qwen-token-anthropic", "tokenplan-anthropic"},
		PlanLabel:       "qwen token plan key",
		Auth:            authBearerWithAnthropicVersion,
		// The Anthropic base stops at /apps/anthropic and the client appends
		// /v1/messages itself, so the version segment must survive: collapsing
		// it here is what produces the /v1/v1 404 the vendor documents.
		CollapseVersionSegment: false,
		LeaseAPI:               "anthropic-messages",
		LeasePath:              "/qwen-anthropic/v1/messages",
		LeaseEnv:               leaseEnvAnthropic,
		Upstream:               func(s Server, _ accounts.AuthMode) *url.URL { return s.QwenAnthropicUpstream },
	},
}

// keyedProviders returns the effective registry: the providers this build ships
// followed by any the operator declared. Declared providers cannot shadow a
// built-in one, which is enforced when they are configured.
func keyedProviders() []keyedProvider {
	configuredMu.RLock()
	declared := configuredProviders
	configuredMu.RUnlock()
	if len(declared) == 0 {
		return builtinKeyedProviders
	}
	all := make([]keyedProvider, 0, len(builtinKeyedProviders)+len(declared))
	all = append(all, builtinKeyedProviders...)
	all = append(all, declared...)
	return all
}

// keyedProviderFor returns the registry entry for a provider.
func keyedProviderFor(provider accounts.Provider) (keyedProvider, bool) {
	for _, entry := range keyedProviders() {
		if entry.Provider == provider {
			return entry, true
		}
	}
	return keyedProvider{}, false
}

// keyedProviderForPathPrefix resolves the first path segment to a provider.
func keyedProviderForPathPrefix(segment string) (keyedProvider, bool) {
	for _, entry := range keyedProviders() {
		if entry.PathPrefix == segment {
			return entry, true
		}
	}
	return keyedProvider{}, false
}

// keyedProviderForName resolves a provider named by a human or an API caller,
// accepting the canonical provider string and any registered alias.
func keyedProviderForName(name string) (keyedProvider, bool) {
	for _, entry := range keyedProviders() {
		if name == string(entry.Provider) {
			return entry, true
		}
		for _, alias := range entry.Aliases {
			if name == alias {
				return entry, true
			}
		}
	}
	return keyedProvider{}, false
}

func canonicalKeyedProvider(provider accounts.Provider) (accounts.Provider, bool) {
	entry, ok := keyedProviderForName(strings.ToLower(strings.TrimSpace(string(provider))))
	if !ok {
		return "", false
	}
	return entry.Provider, true
}

// keyedProviderForModelPrefix infers a provider from a bare model id.
func keyedProviderForModelPrefix(lowerModel string) (keyedProvider, bool) {
	for _, entry := range keyedProviders() {
		if entry.ModelPrefix != "" && len(lowerModel) >= len(entry.ModelPrefix) &&
			lowerModel[:len(entry.ModelPrefix)] == entry.ModelPrefix {
			return entry, true
		}
	}
	return keyedProvider{}, false
}

// isKeyedProvider reports whether a provider is routed through the registry.
func isKeyedProvider(provider accounts.Provider) bool {
	_, ok := canonicalKeyedProvider(provider)
	return ok
}

// keyedProviderNames lists every canonical provider name in the registry, in
// registry order, for advertising and for error messages.
func keyedProviderNames() []string {
	all := keyedProviders()
	names := make([]string, 0, len(all))
	for _, entry := range all {
		names = append(names, string(entry.Provider))
	}
	return names
}

// applyKeyedProviderAuth presents the account's API key the way this provider
// expects it. Authorization is already set to the bearer form by the caller.
func applyKeyedProviderAuth(headers http.Header, account accounts.Account, entry keyedProvider, model string) {
	if entry.Provider == accounts.ProviderGrok {
		// The subscription proxy rejects a normal API bearer token unless the
		// request is explicitly identified as Grok CLI OAuth. Always clear these
		// headers first so a client cannot smuggle them onto an API-key request or
		// leave them behind when failover changes auth modes.
		headers.Del("X-XAI-Token-Auth")
		headers.Del("X-Grok-Model-Override")
		if account.AuthMode == accounts.AuthModeOAuth {
			headers.Set("X-XAI-Token-Auth", "xai-grok-cli")
			if model = strings.TrimSpace(model); model != "" {
				headers.Set("X-Grok-Model-Override", model)
			}
		}
	}
	switch entry.Auth {
	case authBearerWithAnthropicVersion:
		headers.Del("X-Api-Key")
		if headers.Get("Anthropic-Version") == "" {
			headers.Set("Anthropic-Version", "2023-06-01")
		}
	case authBearerAndAnthropicKey:
		headers.Set("Authorization", account.AuthorizationHeader())
		headers.Set("X-Api-Key", account.Token)
		if headers.Get("Anthropic-Version") == "" {
			headers.Set("Anthropic-Version", "2023-06-01")
		}
	default:
		headers.Del("X-Api-Key")
	}
}

// collapseDuplicateVersionSegment drops a leading /v1 from an already
// prefix-stripped path when the upstream base URL ends in /v1, so a client that
// sends /<provider>/v1/... does not reach /v1/v1/... upstream. The collapse is
// conditional on the configured base, so an unversioned override still forwards
// the client's own /v1.
func collapseDuplicateVersionSegment(path string, upstream *url.URL) string {
	upstreamPath := ""
	if upstream != nil {
		upstreamPath = strings.TrimRight(upstream.Path, "/")
	}
	if !strings.HasSuffix(upstreamPath, "/v1") {
		return path
	}
	if path == "/v1" {
		return "/"
	}
	if strings.HasPrefix(path, "/v1/") {
		return strings.TrimPrefix(path, "/v1")
	}
	return path
}

// APIKeyProviderForName resolves a provider named on the CLI or in an API
// payload, accepting the canonical name and any registered alias. Exported so
// the CLI does not carry a second copy of the alias list.
func APIKeyProviderForName(name string) (accounts.Provider, bool) {
	entry, ok := keyedProviderForName(name)
	if !ok {
		return "", false
	}
	return entry.Provider, true
}

// APIKeyAccountProvider returns the canonical owner of a provider's stored
// credential. Protocol variants such as qwen-anthropic share one subscription
// and must not create a duplicate account record.
func APIKeyAccountProvider(provider accounts.Provider) accounts.Provider {
	return accountProviderFor(provider)
}

// APIKeyProviderList renders the providers a user may name, for flag help and
// for the error returned when they name something else.
func APIKeyProviderList() string {
	names := append([]string{"codex", "claude"}, keyedProviderNames()...)
	if len(names) == 1 {
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + ", or " + names[len(names)-1]
}

// accountProviderFor resolves which provider's accounts serve a request for
// this provider. Providers that share a subscription across protocol endpoints
// resolve to the one that owns the credential.
func accountProviderFor(provider accounts.Provider) accounts.Provider {
	if canonical, ok := canonicalKeyedProvider(provider); ok {
		provider = canonical
	}
	if entry, ok := keyedProviderFor(provider); ok && entry.AccountProvider != "" {
		return entry.AccountProvider
	}
	return provider
}

// schedulerAccount keeps transport identity separate from quota identity. A
// Qwen Anthropic request must still route through the Anthropic upstream, but
// both protocols spend and load-balance the same Token Plan account pool.
func schedulerAccount(account accounts.Account) accounts.Account {
	account.Provider = accountProviderFor(account.Provider)
	return account
}

// SchedulerAccounts returns a copy whose providers are normalized to the
// credential owner used by quota scores and exhaustion state.
func SchedulerAccounts(accountsIn []accounts.Account) []accounts.Account {
	out := make([]accounts.Account, len(accountsIn))
	for i, account := range accountsIn {
		out[i] = schedulerAccount(account)
	}
	return out
}

func pickRoutingAccount(scheduler selectacct.Scheduler, candidates []accounts.Account) (accounts.Account, error) {
	picked, err := scheduler.Pick(SchedulerAccounts(candidates))
	if err != nil {
		return accounts.Account{}, err
	}
	for _, candidate := range candidates {
		if candidate.ID == picked.ID && accountProviderFor(candidate.Provider) == picked.Provider {
			return candidate, nil
		}
	}
	return accounts.Account{}, fmt.Errorf("scheduler returned unknown account %q", picked.ID)
}

func schedulerAccountProvider(provider accounts.Provider) accounts.Provider {
	return accountProviderFor(provider)
}

// ProviderDefaultUpstream returns a provider's documented base URL, so a flag
// default and a health probe read the same value.
func ProviderDefaultUpstream(provider accounts.Provider) string {
	entry, ok := keyedProviderForName(string(provider))
	if !ok {
		return ""
	}
	return entry.DefaultUpstream
}

func providerUpstreamUsesDefaultAuthority(provider accounts.Provider, rawUpstream string) bool {
	upstream, err := url.Parse(strings.TrimSpace(rawUpstream))
	if err != nil || upstream.Hostname() == "" {
		return false
	}
	defaultUpstream, err := url.Parse(ProviderDefaultUpstream(provider))
	if err != nil || defaultUpstream.Hostname() == "" {
		return false
	}
	return strings.EqualFold(upstream.Scheme, defaultUpstream.Scheme) &&
		strings.EqualFold(upstream.Hostname(), defaultUpstream.Hostname()) &&
		effectiveURLPort(upstream) == effectiveURLPort(defaultUpstream)
}

func effectiveURLPort(value *url.URL) string {
	if port := value.Port(); port != "" {
		return port
	}
	switch strings.ToLower(value.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	default:
		return ""
	}
}

// ProviderMetering describes how a vendor charges this provider. It remains
// useful plan context even when a provider also exposes key-scoped quota.
func ProviderMetering(provider accounts.Provider) string {
	entry, ok := keyedProviderForName(string(provider))
	if !ok {
		return ""
	}
	return entry.Metering
}

// ProviderHealthURL returns the URL that proves a key still works. The caller
// must supply the actual configured upstream: silently falling back to a vendor
// default can disclose a gateway-specific credential to an unrelated host.
func ProviderHealthURL(provider accounts.Provider, upstream string) string {
	entry, ok := keyedProviderForName(string(provider))
	if !ok || entry.HealthPath == "" || strings.TrimSpace(upstream) == "" {
		return ""
	}
	parsed, err := url.Parse(strings.TrimSpace(upstream))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + entry.HealthPath
	parsed.RawPath = ""
	parsed.Fragment = ""
	return parsed.String()
}

// ProviderEndpoints lists every path prefix served by one provider's
// credential, so a subscription reachable over two protocols reads as one
// account rather than two.
func ProviderEndpoints(provider accounts.Provider) []string {
	owner := accountProviderFor(provider)
	paths := make([]string, 0, 2)
	for _, entry := range keyedProviders() {
		if accountProviderFor(entry.Provider) == owner {
			paths = append(paths, "/"+entry.PathPrefix)
		}
	}
	sort.Strings(paths)
	return paths
}
