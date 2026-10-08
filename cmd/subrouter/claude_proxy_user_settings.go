package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/agents/claude"
)

// Pooled Claude launches point CLAUDE_CONFIG_DIR at a per-account proxy
// directory, so Claude reads that directory's settings.json as the user
// settings and never sees the user's own ~/.claude/settings.json: hooks,
// permissions, auto-mode rules and the rest silently stopped applying under
// `sr claude`. sr already hands Claude one private --settings file for
// routing, so the user's settings ride in that same file (no extra argument
// and no extra settings file for wrappers to merge).
//
// sr owns routing and credentials. Its env values are applied last, so a
// user env entry can never replace a routing key, and settings that would
// substitute a different credential source are dropped.
var claudeProxyUserSettingsDropped = []string{
	"apiKeyHelper",
	"awsAuthRefresh",
	"awsCredentialExport",
	"gcpAuthRefresh",
	"forceLoginMethod",
	"forceLoginOrgUUID",
}

// claudeProxyUserSettingsEnv set to 0, false or off launches pooled Claude
// without the user's settings. Claude rejects a whole settings file when one
// field fails its schema (an invalid permissions block, for example), and the
// routing values share that file, so a broken user settings.json makes the
// launch fail with "Not logged in" until it is fixed or this is set.
const claudeProxyUserSettingsEnv = "SUBROUTER_CLAUDE_USER_SETTINGS"

// claudeProxyUserSettingsPath returns the user's own Claude settings file for
// the real default store, and "" for hermetic stores, which must never read
// the user's real Claude home, or when the user turned the merge off.
func claudeProxyUserSettingsPath(storeDir string) string {
	if disabled := strings.TrimSpace(os.Getenv(claudeProxyUserSettingsEnv)); disabled == "0" || strings.EqualFold(disabled, "false") || strings.EqualFold(disabled, "off") {
		return ""
	}
	store := claude.DefaultStore()
	if filepath.Clean(storeDir) != filepath.Clean(store.Dir) || strings.TrimSpace(store.SharedStateDir) == "" {
		return ""
	}
	return filepath.Join(store.SharedStateDir, "settings.json")
}

func claudeProxyOwnSettingsPath(configDir string) string {
	if strings.TrimSpace(configDir) == "" {
		return ""
	}
	return filepath.Join(configDir, "settings.json")
}

// withClaudeUserSettings rebuilds the private overlay for every launch. Shared
// settings win scalar conflicts; account-only values and collection entries
// saved by Claude survive. Neither settings file is overwritten.
func withClaudeUserSettings(settingsBody []byte, userSettingsPath, proxySettingsPath string) ([]byte, error) {
	if strings.TrimSpace(userSettingsPath) == "" {
		return settingsBody, nil
	}
	merged, ok := readClaudeSettingsObject(userSettingsPath)
	if !ok {
		return settingsBody, nil
	}
	var launch map[string]any
	if err := json.Unmarshal(settingsBody, &launch); err != nil {
		return nil, err
	}
	if strings.TrimSpace(proxySettingsPath) != "" {
		if own, ok := readClaudeSettingsObject(proxySettingsPath); ok {
			mergeClaudeSettingsMap(own, merged)
			merged = own
		}
	}
	return overlayClaudeLaunchSettings(merged, launch)
}

// overlayClaudeLaunchSettings applies sr's launch settings over a lower
// precedence settings object. Credential-source settings are dropped from the
// base, every launch env key wins (case-insensitively), and launch maps merge
// into the base so collections such as hooks keep both sides.
func overlayClaudeLaunchSettings(base, launch map[string]any) ([]byte, error) {
	for _, key := range claudeProxyUserSettingsDropped {
		delete(base, key)
	}
	launchEnv, _ := launch["env"].(map[string]any)
	owned := make(map[string]bool, len(launchEnv))
	for key := range launchEnv {
		owned[strings.ToUpper(key)] = true
	}
	env := map[string]any{}
	if baseEnv, ok := base["env"].(map[string]any); ok {
		for key, value := range baseEnv {
			// Windows environment names are case-insensitive: a base entry
			// that differs from a routing key only in case must not survive
			// beside it.
			if !owned[strings.ToUpper(key)] {
				env[key] = value
			}
		}
	}
	for key, value := range launchEnv {
		env[key] = value
	}
	for key, value := range launch {
		if launchMap, ok := value.(map[string]any); ok {
			if existing, ok := base[key].(map[string]any); ok {
				mergeClaudeSettingsMap(existing, launchMap)
				continue
			}
		}
		base[key] = value
	}
	if len(env) > 0 {
		base["env"] = env
	}
	return json.Marshal(base)
}

// The grant is independent of user settings opt-out and missing/invalid JSON.
func withClaudeProxyMemoryDirectory(body []byte, sharedDir string) ([]byte, error) {
	if sharedDir == "" {
		return body, nil
	}
	projects, err := filepath.EvalSymlinks(filepath.Join(sharedDir, "projects"))
	if err != nil {
		return nil, err
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		return nil, err
	}
	permissions, _ := settings["permissions"].(map[string]any)
	if permissions == nil {
		permissions = map[string]any{}
		settings["permissions"] = permissions
	}
	mergeClaudeSettingsMap(permissions, map[string]any{"additionalDirectories": []any{projects}})
	// Claude Code derives one project-specific memory directory below this
	// root. This environment setting makes the root explicit while preserving
	// the per-project layout; autoMemoryDirectory would flatten all projects.
	env, _ := settings["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
		settings["env"] = env
	}
	env["CLAUDE_CODE_REMOTE_MEMORY_DIR"] = sharedDir
	return json.Marshal(settings)
}

func mergeClaudeSettingsMap(dst, src map[string]any) {
	for key, value := range src {
		// These are individual command definitions, not collection maps.
		if key == "statusLine" {
			dst[key] = value
			continue
		}
		if srcMap, ok := value.(map[string]any); ok {
			if dstMap, ok := dst[key].(map[string]any); ok {
				mergeClaudeSettingsMap(dstMap, srcMap)
				continue
			}
		}
		if srcList, ok := value.([]any); ok {
			if dstList, ok := dst[key].([]any); ok {
				for _, item := range srcList {
					found := false
					for _, previous := range dstList {
						if reflect.DeepEqual(previous, item) {
							found = true
							break
						}
					}
					if !found {
						dstList = append(dstList, item)
					}
				}
				dst[key] = dstList
				continue
			}
		}
		dst[key] = value
	}
}

func readClaudeSettingsObject(path string) (map[string]any, bool) {
	body, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("read Claude settings for proxy launch", "path", path, "error", err)
		}
		return nil, false
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil || settings == nil {
		slog.Warn("ignore Claude settings for proxy launch: not a JSON object", "path", path)
		return nil, false
	}
	return settings, true
}
