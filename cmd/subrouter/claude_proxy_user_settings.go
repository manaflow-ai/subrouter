package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
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

// withClaudeUserSettings layers sr's launch settings over the user's settings
// file. A single-value key the proxy directory's own settings.json sets (a
// /config change made inside a pooled session) is left to that file, so the
// user's value does not hide it. A missing or unreadable user file leaves the
// launch settings as they were; a launch never depends on it.
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
	for _, key := range claudeProxyUserSettingsDropped {
		delete(merged, key)
	}
	if strings.TrimSpace(proxySettingsPath) != "" {
		if own, ok := readClaudeSettingsObject(proxySettingsPath); ok {
			for key, value := range own {
				// Only single values are hidden. Claude combines objects
				// and lists (hooks, permissions, enabledPlugins) across
				// both files, so the user's entries must stay.
				switch value.(type) {
				case map[string]any, []any:
				default:
					delete(merged, key)
				}
			}
		}
	}
	launchEnv, _ := launch["env"].(map[string]any)
	owned := make(map[string]bool, len(launchEnv))
	for key := range launchEnv {
		owned[strings.ToUpper(key)] = true
	}
	env := map[string]any{}
	if userEnv, ok := merged["env"].(map[string]any); ok {
		for key, value := range userEnv {
			// Windows environment names are case-insensitive: a user entry
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
		merged[key] = value
	}
	if len(env) > 0 {
		merged["env"] = env
	}
	return json.Marshal(merged)
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
