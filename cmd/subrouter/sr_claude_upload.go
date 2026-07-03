package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/agents/claude"
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

func (r srRunner) uploadServerClaudeProfile(ctx context.Context, server srServerConfig, store claude.Store, profile claude.Profile, credential claude.CredentialInfo) error {
	stateSubdir, err := r.serverStateSubdir(ctx, server)
	if err != nil {
		return err
	}
	dir := filepath.Base(store.InstancePath(profile.Name))
	if dir == "" || dir == "." || dir == string(os.PathSeparator) {
		return fmt.Errorf("could not determine instance dir for profile %q", profile.Name)
	}
	body, err := json.MarshalIndent(map[string]claude.CredentialInfo{"claudeAiOauth": credential}, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	archive, err := buildClaudeCredentialArchive(stateSubdir, dir, body)
	if err != nil {
		return err
	}
	host := sshHostForServer(server)
	if host == "" {
		return fmt.Errorf("server %s has no SSH-able host in its URL", server.Name)
	}
	remoteCommand := claudeUploadRemoteCommand(server, stateSubdir, profile.Name, dir)
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=15",
		"-o", "LogLevel=ERROR",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		host,
		remoteCommand,
	}
	uploadCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if err := r.commandRunner().Run(uploadCtx, "ssh", args, bytes.NewReader(archive), r.out, r.errOut); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if attempt < 3 {
			if r.errOut != nil {
				fmt.Fprintf(r.errOut, "claude profile upload failed, retrying (%d/3): %v\n", attempt, lastErr)
			}
			timer := time.NewTimer(time.Duration(attempt) * time.Second)
			select {
			case <-uploadCtx.Done():
				timer.Stop()
				return fmt.Errorf("upload claude profile over ssh: %w", uploadCtx.Err())
			case <-timer.C:
			}
		}
	}
	return fmt.Errorf("upload claude profile over ssh: %w", lastErr)
}

func buildClaudeCredentialArchive(stateSubdir, dir string, credentialJSON []byte) ([]byte, error) {
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	relPath := filepath.Join(stateSubdir, "codex", "claude", dir, ".credentials.json")
	if err := tw.WriteHeader(&tar.Header{Name: relPath, Mode: 0o600, Size: int64(len(credentialJSON))}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(credentialJSON); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return archive.Bytes(), nil
}

func claudeUploadRemoteCommand(server srServerConfig, stateSubdir, profileName, dir string) string {
	createdAt := time.Now().UTC().Format(time.RFC3339)
	// The registration merge keeps any existing createdAt and other profiles
	// intact; only this profile's name/dir are overwritten.
	jqProgram := `.profiles[$name] = {name: $name, createdAt: (.profiles[$name].createdAt // $created), dir: $dir}`
	remotePath := fmt.Sprintf("/tmp/sr-claude-cred-%d.tgz", time.Now().UnixNano())
	claudeDir := remoteStatePath(stateSubdir, "codex/claude")
	registryPath := remoteStatePath(stateSubdir, "codex/claude.json")
	return strings.Join([]string{
		"set -euo pipefail",
		"cat > " + shellQuote(remotePath),
		"reload_status=$(curl -sS -o /dev/null -w '%{http_code}' http://127.0.0.1:31415/_subrouter/reload-accounts || true)",
		"if [ \"$reload_status\" != \"405\" ]; then echo " + shellQuote("Subrouter server is too old for hot account reload; run sr server install "+server.Name+" first.") + " >&2; exit 1; fi",
		"command -v jq >/dev/null || { echo 'jq is required on the server for claude profile registration' >&2; exit 1; }",
		"sudo install -d -o subrouter -g subrouter -m 0700 " + shellQuote(claudeDir),
		"sudo tar -C /var/lib/subrouter -xzf " + shellQuote(remotePath),
		"sudo rm -f " + shellQuote(remotePath),
		"sudo chmod 700 " + shellQuote(claudeDir+"/"+dir),
		"sudo chmod 600 " + shellQuote(claudeDir+"/"+dir+"/.credentials.json"),
		"sudo sh -c " + shellQuote("test -s "+registryPath+" || printf '{\"profiles\":{}}' > "+registryPath),
		"sudo jq --arg name " + shellQuote(profileName) + " --arg dir " + shellQuote(dir) + " --arg created " + shellQuote(createdAt) + " " + shellQuote(jqProgram) + " " + shellQuote(registryPath) + " | sudo tee " + shellQuote(registryPath+".new") + " >/dev/null",
		"sudo mv " + shellQuote(registryPath+".new") + " " + shellQuote(registryPath),
		"sudo chown -R subrouter:subrouter " + shellQuote(claudeDir) + " " + shellQuote(registryPath),
		"curl -fsS -X POST http://127.0.0.1:31415/_subrouter/reload-accounts >/dev/null",
	}, " && ")
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
