package proxy

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// OpenAICompatibleProvider declares an OpenAI-compatible endpoint that this
// build did not ship an entry for. A provider whose only distinguishing feature
// is its base URL — a subscription plan on its own host, a self-hosted gateway,
// a relay — does not need code, so operators can declare one instead of waiting
// for a release.
type OpenAICompatibleProvider struct {
	// Name is the provider id and the path prefix: /<Name>/... routes here.
	Name string
	// BaseURL is the endpoint requests are forwarded to.
	BaseURL string
	// Aliases are additional names accepted wherever a provider is named.
	Aliases []string
}

// configuredProviders holds the entries declared for this process. The registry
// is read on every request and written once during startup, before the server
// begins serving, so a mutex is enough and no request-time coordination is
// needed. It is deliberately not reconfigurable while serving: a provider
// disappearing under an in-flight request would route it nowhere.
var (
	configuredMu        sync.RWMutex
	configuredProviders []keyedProvider
	configuredFrozen    bool
)

// reservedProviderNames are names and path segments owned by core or outer
// routers. A configured provider claiming one would be shadowed or redirect an
// existing route, so it is rejected rather than silently accepted.
var reservedProviderNames = map[string]bool{
	"apikey": true,
	"codex":  true, "openai": true, "openai-codex": true,
	"claude": true, "anthropic": true,
	"gemini": true, "bedrock": true,
	"antigravity": true,
	"internal":    true, "_subrouter": true, "t": true,
	"v1": true, "backend-api": true, "messages": true, "responses": true,
	"chat": true, "completions": true, "embeddings": true,
	"models": true, "alpha": true, "ps": true, "plugins": true,
}

// ConfigureOpenAICompatibleProviders registers operator-declared providers.
// It must be called during startup, before the server serves any request, and
// replaces any previous declaration so a caller cannot accumulate duplicates by
// being invoked twice.
func ConfigureOpenAICompatibleProviders(declared []OpenAICompatibleProvider) error {
	configuredMu.Lock()
	defer configuredMu.Unlock()
	if configuredFrozen {
		return fmt.Errorf("openai-compatible providers cannot change after serving starts")
	}
	entries := make([]keyedProvider, 0, len(declared))
	seen := map[string]bool{}
	for _, item := range declared {
		name := strings.ToLower(strings.TrimSpace(item.Name))
		if name == "" {
			return fmt.Errorf("openai-compatible provider needs a name")
		}
		if !validDeclaredProviderIdentifier(name) {
			return fmt.Errorf("openai-compatible provider name %q must be 1-63 characters, start with a letter or digit, and use only lowercase letters, digits, '.', '_', or '-'", item.Name)
		}
		base, err := parseProviderBaseURL(item.BaseURL, name)
		if err != nil {
			return err
		}
		names := append([]string{name}, normalizeAliases(item.Aliases)...)
		for _, candidate := range names {
			if !validDeclaredProviderIdentifier(candidate) {
				return fmt.Errorf("openai-compatible provider %q has invalid alias %q: aliases must use only lowercase letters, digits, '.', '_', or '-'", item.Name, candidate)
			}
			if reservedProviderNames[candidate] {
				return fmt.Errorf("openai-compatible provider %q claims the reserved name %q", item.Name, candidate)
			}
			if builtinClaimsName(candidate) {
				return fmt.Errorf("openai-compatible provider %q claims %q, which a built-in provider already uses", item.Name, candidate)
			}
			if seen[candidate] {
				return fmt.Errorf("openai-compatible provider name %q is declared twice", candidate)
			}
			seen[candidate] = true
		}
		entries = append(entries, keyedProvider{
			Provider:   accounts.Provider(name),
			PathPrefix: name,
			Aliases:    names[1:],
			PlanLabel:  name + " api key",
			Auth:       authBearer,
			// The declared base may or may not end in /v1, and an
			// OpenAI-compatible client sends /v1/... either way.
			CollapseVersionSegment: true,
			LeaseAPI:               "openai-completions",
			LeasePath:              "/" + name + "/chat/completions",
			LeaseEnv:               leaseEnvOpenAI,
			Upstream:               func(Server, accounts.AuthMode) *url.URL { return base },
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].PathPrefix < entries[j].PathPrefix })
	configuredProviders = entries
	return nil
}

// FreezeOpenAICompatibleProviders prevents routing declarations from changing
// once a handler can observe them.
func FreezeOpenAICompatibleProviders() {
	configuredMu.Lock()
	configuredFrozen = true
	configuredMu.Unlock()
}

func builtinClaimsName(candidate string) bool {
	for _, entry := range builtinKeyedProviders {
		if candidate == string(entry.Provider) {
			return true
		}
		for _, alias := range entry.Aliases {
			if candidate == alias {
				return true
			}
		}
	}
	return false
}

// ValidDeclaredProviderName reports whether a standalone CLI may safely carry
// a provider name to a serving process that owns the actual declaration. The
// server still rejects imports for names it did not declare.
func ValidDeclaredProviderName(raw string) bool {
	name := strings.ToLower(strings.TrimSpace(raw))
	return validDeclaredProviderIdentifier(name) &&
		!reservedProviderNames[name] && !builtinClaimsName(name)
}

func validDeclaredProviderIdentifier(name string) bool {
	// Keep provider-scoped account identifiers comfortably inside the 255-byte
	// filename-component limit after the label, JSON suffix, lock prefix, and
	// atomic-write suffix are added by the credential store.
	if name == "" || len(name) > 63 {
		return false
	}
	first := name[0]
	if !(first >= 'a' && first <= 'z' || first >= '0' && first <= '9') {
		return false
	}
	for _, char := range name {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			continue
		}
		switch char {
		case '-', '_', '.':
			continue
		default:
			return false
		}
	}
	return true
}

func parseProviderBaseURL(raw, name string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("openai-compatible provider %q needs a base URL", name)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("openai-compatible provider %q has an unparseable base URL: %w", name, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("openai-compatible provider %q base URL must be http or https, got %q", name, parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("openai-compatible provider %q base URL has no host", name)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("openai-compatible provider %q base URL must not contain userinfo", name)
	}
	if err := validateCredentialUpstream(name, parsed); err != nil {
		return nil, fmt.Errorf("openai-compatible provider %q: %w", name, err)
	}
	return parsed, nil
}

func normalizeAliases(aliases []string) []string {
	out := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		trimmed := strings.ToLower(strings.TrimSpace(alias))
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// ParseOpenAICompatibleFlag reads one `name=https://host/v1` declaration, with
// optional comma-separated aliases after the name: `name|alias=https://...`.
func ParseOpenAICompatibleFlag(value string) (OpenAICompatibleProvider, error) {
	name, base, found := strings.Cut(value, "=")
	if !found {
		return OpenAICompatibleProvider{}, fmt.Errorf("openai-compatible provider %q must be name=BASE_URL", value)
	}
	parts := strings.Split(name, "|")
	return OpenAICompatibleProvider{
		Name:    parts[0],
		Aliases: parts[1:],
		BaseURL: base,
	}, nil
}
