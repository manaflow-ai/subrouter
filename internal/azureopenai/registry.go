package azureopenai

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/manaflow-ai/subrouter/account"
)

const accountIDPrefix = "azure:"

type Registry struct {
	profiles map[string]Profile
	tokens   map[string]TokenSource
}

func NewRegistry(profiles []Profile, runner CommandRunner) (*Registry, error) {
	return NewRegistryWithTokenFactory(profiles, func(profile Profile) TokenSource {
		return &CachedCLITokenSource{Profile: profile, Runner: runner}
	})
}

func NewRegistryWithTokenFactory(profiles []Profile, factory func(Profile) TokenSource) (*Registry, error) {
	if factory == nil {
		return nil, fmt.Errorf("Azure OpenAI token factory is required")
	}
	registry := &Registry{
		profiles: make(map[string]Profile, len(profiles)),
		tokens:   make(map[string]TokenSource, len(profiles)),
	}
	for _, raw := range profiles {
		profile, err := NormalizeProfile(raw)
		if err != nil {
			return nil, err
		}
		if _, exists := registry.profiles[profile.Name]; exists {
			return nil, fmt.Errorf("duplicate Azure OpenAI profile %q", profile.Name)
		}
		source := factory(profile)
		if source == nil {
			return nil, fmt.Errorf("Azure OpenAI profile %q has no token source", profile.Name)
		}
		registry.profiles[profile.Name] = profile
		registry.tokens[profile.Name] = source
	}
	return registry, nil
}

func AccountID(name string) string {
	return accountIDPrefix + strings.ToLower(strings.TrimSpace(name))
}

func ProfileNameFromAccountID(id string) (string, bool) {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(id)), accountIDPrefix) {
		return "", false
	}
	name := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(id)), accountIDPrefix)
	return name, profileNamePattern.MatchString(name)
}

func (r *Registry) Profile(name string) (Profile, bool) {
	if r == nil {
		return Profile{}, false
	}
	profile, ok := r.profiles[strings.ToLower(strings.TrimSpace(name))]
	return profile, ok
}

func (r *Registry) ProfileForAccount(id string) (Profile, bool) {
	name, ok := ProfileNameFromAccountID(id)
	if !ok {
		return Profile{}, false
	}
	return r.Profile(name)
}

func (r *Registry) Token(ctx context.Context, accountID string) (string, error) {
	name, ok := ProfileNameFromAccountID(accountID)
	if !ok || r == nil {
		return "", errorsForUnknownProfile(accountID)
	}
	source, ok := r.tokens[name]
	if !ok {
		return "", errorsForUnknownProfile(accountID)
	}
	return source.Token(ctx)
}

func errorsForUnknownProfile(value string) error {
	return fmt.Errorf("Azure OpenAI profile for account %q is not configured", value)
}

func (r *Registry) Accounts(source string) []account.Account {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.profiles))
	for name := range r.profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]account.Account, 0, len(names))
	for _, name := range names {
		profile := r.profiles[name]
		out = append(out, account.Account{
			ID:       AccountID(name),
			Provider: account.ProviderAzureOpenAI,
			AuthMode: account.AuthModeAzureCLI,
			Label:    profile.Name,
			Source:   source,
		})
	}
	return out
}
