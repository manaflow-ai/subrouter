package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/tenant"
)

// pushClaudeProfileToServer uploads a local Claude profile's credential to the
// default remote server, registers it in the server's claude.json, hot-reloads
// accounts, and switches the local profile to proxy-mode env so the server
// becomes the single owner of the rotating OAuth refresh token. Anthropic
// rotates refresh tokens on use, so a credential refreshed from two places
// invalidates one of them; after the push, local runs route through the proxy
// instead of refreshing directly.
func (r srRunner) pushClaudeProfileToServer(ctx context.Context, name string) error {
	return r.pushClaudeProfile(ctx, name, true)
}

// pushClaudeProfileAfterAdd is the auto-upload hook for 'sr claude add': a
// missing default server is a silent no-op (purely local setups stay local).
func (r srRunner) pushClaudeProfileAfterAdd(ctx context.Context, name string) error {
	return r.pushClaudeProfile(ctx, name, false)
}

func (r srRunner) pushClaudeProfile(ctx context.Context, name string, requireServer bool) error {
	config, err := cloudModeConfig()
	if err != nil {
		return err
	}
	switch source := config.EffectiveCredentialSource(); source {
	case broker.CredentialSourceTeam, broker.CredentialSourceHosted:
		if requireServer {
			label := "team storage"
			if source == broker.CredentialSourceHosted {
				label = "hosted cmux"
			}
			return fmt.Errorf(
				"%s uses '%s account import --only claude:%s'",
				label,
				r.programOrSubrouter(),
				name,
			)
		}
		return nil
	case broker.CredentialSourceLocal:
		if requireServer {
			return fmt.Errorf("credential storage is local; the profile already stays on this machine")
		}
		return nil
	}
	server, ok, err := r.defaultRemoteServer()
	if err != nil {
		return err
	}
	if !ok {
		if !requireServer {
			return nil
		}
		return fmt.Errorf("no default Subrouter server configured; run '%s server use <name>'", r.programOrSubrouter())
	}
	store := claude.DefaultStore()
	profile, ok, err := store.MatchProfile(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("Claude profile %q not found", name)
	}
	configDir := store.ClaudeConfigDir(profile.Name)
	credential, err := store.ReadCredential(ctx, configDir)
	if err != nil {
		return err
	}
	if credential == nil || credential.AccessToken == "" {
		return fmt.Errorf("Claude profile %q has no credential to upload", profile.Name)
	}
	if err := r.uploadServerClaudeProfile(ctx, server, store, profile, *credential); err != nil {
		return err
	}
	if err := writeClaudeProxyEnv(configDir, serverProxyRootURL(server), strings.TrimSpace(server.TenantKey)); err != nil {
		return fmt.Errorf("profile uploaded, but writing proxy env to settings.json failed: %w", err)
	}
	fmt.Fprintf(r.out, "Uploaded Claude profile %s to server %s and switched local runs to the server pool.\n", profile.Name, server.Name)
	return nil
}

func (r srRunner) uploadServerClaudeProfile(ctx context.Context, server srServerConfig, _ claude.Store, profile claude.Profile, credential claude.CredentialInfo) error {
	return r.uploadServerClaudeAccount(ctx, server, profile.Name, credential)
}

// writeClaudeProxyEnv merges the Subrouter proxy env into the profile's
// settings.json, preserving unrelated settings. With no tenant key, a dummy
// auth token satisfies Claude Code's auth requirement and the server replaces
// it with pooled credentials; with a tenant key, the key itself is the auth
// token so the server can scope the request to the tenant's pool.
func writeClaudeProxyEnv(configDir, baseURL, tenantKey string) error {
	settingsPath := filepath.Join(configDir, "settings.json")
	settings := map[string]any{}
	if body, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(body, &settings); err != nil {
			return fmt.Errorf("parse %s: %w", settingsPath, err)
		}
	}
	env, _ := settings["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	env["ANTHROPIC_BASE_URL"] = strings.TrimRight(baseURL, "/")
	if tenantKey != "" {
		env["ANTHROPIC_AUTH_TOKEN"] = tenantKey
	} else if existing, ok := env["ANTHROPIC_AUTH_TOKEN"].(string); !ok || tenant.ValidKeyFormat(existing) {
		// Absent, or a stale tenant key left over from a previous
		// tenant-scoped server: reset to the dummy token. Unrelated custom
		// tokens are preserved.
		env["ANTHROPIC_AUTH_TOKEN"] = "subrouter"
	}
	settings["env"] = env
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(settingsPath, out, 0o600)
}
