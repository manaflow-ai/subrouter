package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/tenant"
)

const (
	managedClaudeBlockedBaseURL   = "http://127.0.0.1:1"
	managedClaudeServerURLEnv     = "SUBROUTER_CLAUDE_SERVER_URL"
	managedClaudeTailscaleNodeEnv = "SUBROUTER_CLAUDE_TAILSCALE_NODE_ID"
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

// addClaudeToServer retains the selected target throughout enrollment and upload.
func (r srRunner) addClaudeToServer(ctx context.Context, server srServerConfig, args []string) error {
	store := claude.DefaultStore()
	cr := claudeRunner{
		store: store, in: r.in, out: r.out, errOut: r.errOut, client: r.client,
		pushAfterAdd: func(ctx context.Context, name string) error {
			profile, ok, err := store.MatchProfile(name)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("Claude profile was not created")
			}
			credential, err := store.ReadCredential(ctx, store.ClaudeConfigDir(profile.Name))
			if err != nil {
				return err
			}
			if credential == nil {
				return fmt.Errorf("Claude credential was not created")
			}
			return r.uploadServerClaudeProfile(ctx, server, store, profile, *credential)
		},
	}
	return cr.run(ctx, append([]string{"add"}, args...))
}

// pushClaudeProfileAfterAdd is the auto-upload hook for 'sr add claude': a
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
	if err := requireManagedClaudeServerIdentity(server); err != nil {
		return err
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
	if err := writeClaudeProxyEnvForServer(configDir, server); err != nil {
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
	_, err := secureTenantProxyURL(context.Background(), baseURL, tenantKey)
	if err != nil {
		return err
	}
	return writeClaudeProxyEnvCanonical(configDir, baseURL, tenantKey)
}

func writeClaudeProxyEnvForServer(configDir string, server srServerConfig) error {
	return writeClaudeProxyEnvForServerWithResolvers(configDir, server, net.DefaultResolver.LookupIPAddr, defaultTailscaleStatusLoader)
}

func writeClaudeProxyEnvForServerWithResolvers(configDir string, server srServerConfig, lookup serverIPLookup, load tailscaleStatusLoader) error {
	if err := requireManagedClaudeServerIdentity(server); err != nil {
		return err
	}
	root := canonicalServerProxyRootURL(server)
	if _, err := secureTenantServerURLWithResolvers(context.Background(), root, server, lookup, load); err != nil {
		return err
	}
	parsed, _ := url.Parse(root)
	if parsed != nil && strings.EqualFold(parsed.Scheme, "http") && !isLoopbackServerHost(parsed.Hostname()) {
		// A plain `claude` launch cannot revalidate the saved Tailscale node.
		// Leave it on a closed loopback endpoint. The same atomic settings
		// generation records the non-credential server identity; `sr claude
		// run` verifies that exact node and overrides the base URL per process.
		return writeClaudeProxyEnvCanonicalForServer(configDir, managedClaudeBlockedBaseURL, strings.TrimSpace(server.TenantKey), &server)
	}
	return writeClaudeProxyEnvCanonical(configDir, root, strings.TrimSpace(server.TenantKey))
}

func requireManagedClaudeServerIdentity(server srServerConfig) error {
	parsed, _ := url.Parse(canonicalServerProxyRootURL(server))
	if parsed != nil && strings.EqualFold(parsed.Scheme, "http") && !isLoopbackServerHost(parsed.Hostname()) && strings.TrimSpace(server.TailscaleNodeID) == "" {
		return fmt.Errorf("plaintext remote Claude server %q has no exact Tailscale node identity; re-add the server before uploading credentials", server.Name)
	}
	return nil
}

func writeClaudeProxyEnvCanonical(configDir, baseURL, tenantKey string) error {
	return writeClaudeProxyEnvCanonicalForServer(configDir, baseURL, tenantKey, nil)
}

func writeClaudeProxyEnvCanonicalForServer(configDir, baseURL, tenantKey string, server *srServerConfig) error {
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
	env["ANTHROPIC_CUSTOM_HEADERS"] = "X-Subrouter-Agent: claude"
	if server != nil {
		env[managedClaudeServerURLEnv] = strings.TrimRight(strings.TrimSpace(server.URL), "/")
		env[managedClaudeTailscaleNodeEnv] = strings.TrimSpace(server.TailscaleNodeID)
	} else {
		delete(env, managedClaudeServerURLEnv)
		delete(env, managedClaudeTailscaleNodeEnv)
	}
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
	tmp, err := os.CreateTemp(configDir, ".settings.json.*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, settingsPath)
}
