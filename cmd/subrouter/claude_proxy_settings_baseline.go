package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/fsutil"
)

// A proxy config directory's own settings.json is what Claude reads as the
// user settings whenever it runs with CLAUDE_CONFIG_DIR pointing there, and
// sr's --settings overlay (withClaudeUserSettings) exists only for launches
// sr makes itself. So the directory also carries a baseline of the user's
// permission rules and hooks: a new directory starts with them, and an
// existing one gains any it is missing each time sr prepares it.
//
// Rule lists and hook groups are unioned without duplicates. sr records each
// entry it adds in a sidecar file next to the proxy settings, so an entry the
// user later deletes from their own file, or a hook command they edit, is
// removed from the directory too: otherwise a revoked allow rule would keep
// working in pooled sessions with no file the user owns to revoke it from.
// Entries the directory had for any other reason (Claude saving a rule from
// a pooled session) are never removed, and every other key (theme, autoMode,
// a /config change) is left exactly as the directory has it. Only permissions and hooks are
// copied: they hold no credentials, and keys that could substitute a
// credential source never leave the user's file.
//
// SUBROUTER_CLAUDE_USER_SETTINGS=0 turns this off together with the launch
// overlay, because claudeProxyUserSettingsPath returns "" for it.
var claudeProxySettingsBaselineKeys = []string{"permissions", "hooks"}

// claudeProxyPermissionListKeys are the permissions lists that are unioned.
// Any other key under permissions, list or not, stays the directory's own.
var claudeProxyPermissionListKeys = []string{"allow", "deny", "ask", "additionalDirectories"}

// seedClaudeProxySettingsBaseline merges the baseline keys of the user's
// settings file into configDir/settings.json. A missing or unparsable user
// file, or an unparsable proxy file, is left alone; errors are for logging
// only and never block a launch.
func seedClaudeProxySettingsBaseline(userSettingsPath, configDir string) error {
	proxySettingsPath := claudeProxyOwnSettingsPath(configDir)
	if strings.TrimSpace(userSettingsPath) == "" || proxySettingsPath == "" {
		return nil
	}
	if sameClaudeConfigFile(userSettingsPath, proxySettingsPath) {
		return nil
	}
	user, ok := readClaudeSettingsObject(userSettingsPath)
	if !ok {
		// Missing or mid-edit: revoking everything sr seeded because the
		// user's file is briefly unreadable would be worse than waiting.
		return nil
	}
	seededPath := claudeProxySeededSettingsPath(configDir)
	seeded := readClaudeProxySeeded(seededPath)
	userPermissions, _ := user["permissions"].(map[string]any)
	userHooks, _ := user["hooks"].(map[string]any)
	if len(userPermissions) == 0 && len(userHooks) == 0 && seeded.empty() {
		return nil
	}
	// A dotfile manager may link the file elsewhere; write through the link.
	if resolved, err := filepath.EvalSymlinks(proxySettingsPath); err == nil {
		proxySettingsPath = resolved
	}
	release, locked := lockClaudeConfig(proxySettingsPath)
	if !locked {
		return nil
	}
	defer release()
	info, statErr := os.Stat(proxySettingsPath)
	body, err := os.ReadFile(proxySettingsPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		body = []byte("{}")
	case err != nil:
		return err
	}
	var own map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber() // keep the directory's own numbers exactly as written
	if err := decoder.Decode(&own); err != nil {
		// Claude reports a broken settings file itself; rewriting it here
		// would destroy whatever the user was in the middle of editing.
		return nil
	}
	if own == nil {
		own = map[string]any{}
	}
	next := claudeProxySeeded{Permissions: map[string][]string{}, Hooks: map[string][]string{}}
	changed := false
	if merged, ok := reconcileClaudeProxyLists(own["permissions"], userPermissions, claudeProxyPermissionListKeys, seeded.Permissions, next.Permissions); ok {
		own["permissions"] = merged
		changed = true
	}
	if merged, ok := reconcileClaudeProxyLists(own["hooks"], userHooks, nil, seeded.Hooks, next.Hooks); ok {
		own["hooks"] = merged
		changed = true
	}
	if changed {
		var out bytes.Buffer
		encoder := json.NewEncoder(&out)
		encoder.SetEscapeHTML(false) // hook commands keep their literal > and &
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(own); err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if statErr == nil {
			mode = info.Mode().Perm()
		}
		if err := fsutil.WriteFileAtomic(proxySettingsPath, out.Bytes(), mode); err != nil {
			return err
		}
	}
	if !next.equal(seeded) {
		return writeClaudeProxySeeded(seededPath, next)
	}
	return nil
}

// reconcileClaudeProxyLists brings each named list of ownValue in line with
// the user's: entries sr seeded that the user no longer has are removed, and
// user entries the directory lacks are appended. keys limits which lists are
// considered (nil means every list key either side names). It records in
// nextSeeded the entries that are now present because sr put them there, and
// reports whether ownValue changed. A non-object ownValue is left untouched.
func reconcileClaudeProxyLists(ownValue any, user map[string]any, keys []string, seeded, nextSeeded map[string][]string) (map[string]any, bool) {
	own, ok := ownValue.(map[string]any)
	if ownValue != nil && !ok {
		return nil, false
	}
	if own == nil {
		own = map[string]any{}
	}
	if keys == nil {
		names := map[string]bool{}
		for key := range user {
			names[key] = true
		}
		for key := range seeded {
			names[key] = true
		}
		for key := range names {
			keys = append(keys, key)
		}
		sort.Strings(keys)
	}
	changed := false
	for _, key := range keys {
		userList, _ := user[key].([]any)
		existing, ok := own[key].([]any)
		if own[key] != nil && !ok {
			continue
		}
		merged, stillSeeded, listChanged := reconcileJSONList(existing, userList, seeded[key])
		if len(stillSeeded) > 0 {
			nextSeeded[key] = stillSeeded
		}
		if !listChanged {
			continue
		}
		changed = true
		if len(merged) == 0 && own[key] != nil && len(existing) > 0 && userList == nil {
			delete(own, key)
			continue
		}
		own[key] = merged
	}
	return own, changed
}

// reconcileJSONList drops from base each entry sr seeded (seededKeys) that
// user no longer holds, then appends each user entry base lacks. Entries are
// compared as canonical JSON so object key order does not matter. It returns
// the list, the canonical keys sr is now responsible for, and whether the
// list changed.
func reconcileJSONList(base, user []any, seededKeys []string) ([]any, []string, bool) {
	wanted := make(map[string]bool, len(user))
	for _, entry := range user {
		if key, err := json.Marshal(entry); err == nil {
			wanted[string(key)] = true
		}
	}
	wasSeeded := make(map[string]bool, len(seededKeys))
	for _, key := range seededKeys {
		wasSeeded[key] = true
	}
	changed := false
	present := make(map[string]bool, len(base)+len(user))
	out := make([]any, 0, len(base)+len(user))
	var responsible []string
	for _, entry := range base {
		raw, err := json.Marshal(entry)
		if err != nil {
			out = append(out, entry)
			continue
		}
		key := string(raw)
		if wasSeeded[key] && !wanted[key] {
			changed = true
			continue
		}
		if present[key] {
			out = append(out, entry)
			continue
		}
		present[key] = true
		if wasSeeded[key] {
			responsible = append(responsible, key)
		}
		out = append(out, entry)
	}
	for _, entry := range user {
		raw, err := json.Marshal(entry)
		if err != nil || present[string(raw)] {
			continue
		}
		present[string(raw)] = true
		responsible = append(responsible, string(raw))
		out = append(out, entry)
		changed = true
	}
	return out, responsible, changed
}

// claudeProxySeededSettingsFile sits next to a proxy directory's
// settings.json and lists, as canonical JSON, each entry sr seeded there.
const claudeProxySeededSettingsFile = "settings.subrouter-seeded.json"

type claudeProxySeeded struct {
	Permissions map[string][]string `json:"permissions,omitempty"`
	Hooks       map[string][]string `json:"hooks,omitempty"`
}

func (s claudeProxySeeded) empty() bool {
	return len(s.Permissions) == 0 && len(s.Hooks) == 0
}

func (s claudeProxySeeded) equal(other claudeProxySeeded) bool {
	a, _ := json.Marshal(s)
	b, _ := json.Marshal(other)
	return bytes.Equal(a, b)
}

func claudeProxySeededSettingsPath(configDir string) string {
	return filepath.Join(configDir, claudeProxySeededSettingsFile)
}

// readClaudeProxySeeded returns the recorded seeded entries. A missing or
// unreadable record means sr seeded nothing it can prove, so nothing is
// removed: only entries sr knows it added are ever taken away.
func readClaudeProxySeeded(path string) claudeProxySeeded {
	body, err := os.ReadFile(path)
	if err != nil {
		return claudeProxySeeded{}
	}
	var seeded claudeProxySeeded
	if err := json.Unmarshal(body, &seeded); err != nil {
		return claudeProxySeeded{}
	}
	return seeded
}

func writeClaudeProxySeeded(path string, seeded claudeProxySeeded) error {
	if seeded.empty() {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	body, err := json.MarshalIndent(seeded, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, append(body, '\n'), 0o600)
}
