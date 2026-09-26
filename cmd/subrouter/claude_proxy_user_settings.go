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

// claudeProxyUserSettingsPath returns the user's own Claude settings file for
// the real default store, and "" for hermetic stores, which must never read
// the user's real Claude home.
func claudeProxyUserSettingsPath(storeDir string) string {
	store := claude.DefaultStore()
	if filepath.Clean(storeDir) != filepath.Clean(store.Dir) || strings.TrimSpace(store.SharedStateDir) == "" {
		return ""
	}
	return filepath.Join(store.SharedStateDir, "settings.json")
}

// withClaudeUserSettings layers sr's launch settings over the user's settings
// file. A missing or unreadable user file leaves the launch settings as they
// were; a launch never depends on it.
func withClaudeUserSettings(settingsBody []byte, userSettingsPath string) ([]byte, error) {
	if strings.TrimSpace(userSettingsPath) == "" {
		return settingsBody, nil
	}
	userBody, err := os.ReadFile(userSettingsPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("read Claude user settings for proxy launch", "error", err)
		}
		return settingsBody, nil
	}
	var merged map[string]any
	if err := json.Unmarshal(userBody, &merged); err != nil || merged == nil {
		slog.Warn("ignore Claude user settings for proxy launch: not a JSON object")
		return settingsBody, nil
	}
	var launch map[string]any
	if err := json.Unmarshal(settingsBody, &launch); err != nil {
		return nil, err
	}
	for _, key := range claudeProxyUserSettingsDropped {
		delete(merged, key)
	}
	env := map[string]any{}
	if userEnv, ok := merged["env"].(map[string]any); ok {
		for key, value := range userEnv {
			env[key] = value
		}
	}
	if launchEnv, ok := launch["env"].(map[string]any); ok {
		for key, value := range launchEnv {
			env[key] = value
		}
	}
	for key, value := range launch {
		merged[key] = value
	}
	if len(env) > 0 {
		merged["env"] = env
	}
	return json.Marshal(merged)
}
