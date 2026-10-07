package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/agents/claude"
)

// The default remains "pooled" for backward compatibility. Team deployments
// can select "native" once per developer, without changing every invocation.
const (
	claudeDefaultRouteEnv = "SUBROUTER_CLAUDE_DEFAULT_ROUTE"
	claudeNativeProfileEnv = "SUBROUTER_CLAUDE_NATIVE_PROFILE"
)

type claudeDefaultRoute string

const (
	claudeDefaultPooled claudeDefaultRoute = "pooled"
	claudeDefaultNative claudeDefaultRoute = "native"
)

func parseClaudeDefaultRoute(raw string) (claudeDefaultRoute, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "pooled":
		return claudeDefaultPooled, nil
	case "native":
		return claudeDefaultNative, nil
	default:
		return "", fmt.Errorf("invalid %s=%q: expected pooled or native", claudeDefaultRouteEnv, raw)
	}
}

// Only implicit agent launches obey the configured default. Explicit proxy,
// profile management, and help commands always keep their existing behavior.
func claudeImplicitAgentLaunch(args []string) bool {
	if len(args) == 0 {
		return true
	}
	if args[0] == "-h" || args[0] == "--help" {
		return false
	}
	return strings.HasPrefix(args[0], "-")
}

func claudeNativeProfileName(configured, active string) string {
	if name := strings.TrimSpace(configured); name != "" {
		return name
	}
	return strings.TrimSpace(active)
}

func (r srRunner) routeDefaultClaudeLaunch(ctx context.Context, args []string) (bool, error) {
	if !claudeImplicitAgentLaunch(args) {
		return false, nil
	}
	route, err := parseClaudeDefaultRoute(os.Getenv(claudeDefaultRouteEnv))
	if err != nil {
		return true, err
	}
	if route != claudeDefaultNative {
		return false, nil
	}
	return true, r.launchNativeClaude(ctx, args)
}

// launchNativeClaude selects only a profile in this user's local Claude store.
// It never hands credentials to Subrouter's serving daemon or account pool.
// Each process is the real Claude Code CLI, with independent session state.
func (r srRunner) launchNativeClaude(ctx context.Context, args []string) error {
	store := claude.DefaultStore()
	name := claudeNativeProfileName(os.Getenv(claudeNativeProfileEnv), store.ActiveProfile())
	if name == "" {
		return r.claudeDirect(ctx, args) // standard per-user ~/.claude login
	}
	profile, ok, err := store.MatchProfile(name)
	if err != nil {
		return fmt.Errorf("select native Claude profile: %w", err)
	}
	if !ok {
		return fmt.Errorf("native Claude profile %q is missing; log in with 'sr claude login %s' or unset %s", name, name, claudeNativeProfileEnv)
	}
	return r.claudeDirectWithConfigDir(ctx, args, store.ClaudeConfigDir(profile.Name))
}

// claudeDirectChildEnvironment removes inherited API/gateway routes and
// Subrouter's private control-plane variables before launching native Claude.
// Only the chosen local profile directory is reintroduced.
func claudeDirectChildEnvironment(parent []string, configDir string) []string {
	env := envWithout(envWithoutSubrouterControl(parent), claudeRoutingEnvKeys)
	if configDir != "" {
		env = upsertEnv(env, "CLAUDE_CONFIG_DIR", configDir)
		env = upsertEnv(env, "CLAUDE_CODE_CONFIG_DIR", configDir)
	}
	return env
}
