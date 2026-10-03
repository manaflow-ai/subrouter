package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Claude accepts one --settings value, and sr must pass its own private
// overlay there to pin routing. managedClaudeLaunchArgs therefore removes the
// caller's --settings arguments; withClaudeCLISettings carries their content
// into the overlay instead, so hooks, permissions and env from --settings
// still reach Claude. Precedence matches Claude's: --settings sits above the
// user settings file, and sr's routing values sit above both. Project and
// local settings files are still read by Claude itself, and Claude runs the
// hooks from every source.

// claudeCLISettingsValues returns the caller's --settings values in order. It
// scans exactly as managedClaudeLaunchArgs does: nothing after "--" is an
// option.
func claudeCLISettingsValues(args []string) ([]string, error) {
	var values []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		switch {
		case arg == "--settings":
			if i+1 >= len(args) || args[i+1] == "--" || strings.HasPrefix(args[i+1], "-") {
				return nil, fmt.Errorf("%s requires a value", arg)
			}
			values = append(values, args[i+1])
			i++
		case strings.HasPrefix(arg, "--settings="):
			_, value, _ := strings.Cut(arg, "=")
			if strings.TrimSpace(value) == "" {
				return nil, fmt.Errorf("--settings requires a value")
			}
			values = append(values, value)
		}
	}
	return values, nil
}

// readClaudeCLISettings reads one --settings value the way Claude does: a
// JSON object string, or else the path of a JSON settings file.
func readClaudeCLISettings(value string) (map[string]any, error) {
	body := []byte(value)
	if !strings.HasPrefix(strings.TrimSpace(value), "{") {
		fileBody, err := os.ReadFile(value)
		if err != nil {
			return nil, fmt.Errorf("read --settings file: %w", err)
		}
		body = fileBody
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil || settings == nil {
		return nil, fmt.Errorf("--settings %q is not a JSON settings object", value)
	}
	return settings, nil
}

// validateClaudeCLISettings reads every --settings value without using it,
// so a pooled launch can reject a bad value before it records itself in the
// session ledger or contacts a server.
func validateClaudeCLISettings(args []string) error {
	values, err := claudeCLISettingsValues(args)
	if err != nil {
		return err
	}
	for _, value := range values {
		if _, err := readClaudeCLISettings(value); err != nil {
			return err
		}
	}
	return nil
}

// withClaudeCLISettings merges the caller's --settings values under sr's
// launch settings body. Later --settings values win scalar conflicts; lists
// such as hook groups are combined.
func withClaudeCLISettings(launchBody []byte, args []string) ([]byte, error) {
	values, err := claudeCLISettingsValues(args)
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return launchBody, nil
	}
	cli := map[string]any{}
	for _, value := range values {
		settings, err := readClaudeCLISettings(value)
		if err != nil {
			return nil, err
		}
		mergeClaudeSettingsMap(cli, settings)
	}
	// --settings may not select a route or a config directory, even where the
	// launch body leaves a key absent on purpose (direct mode keeps
	// CLAUDE_CONFIG_DIR unset so Claude uses the normal login).
	if env, ok := cli["env"].(map[string]any); ok {
		routing := make(map[string]bool, len(claudeRoutingEnvKeys))
		for _, key := range claudeRoutingEnvKeys {
			routing[strings.ToUpper(key)] = true
		}
		for key := range env {
			if routing[strings.ToUpper(key)] {
				delete(env, key)
			}
		}
	}
	launch := map[string]any{}
	if len(bytes.TrimSpace(launchBody)) > 0 {
		if err := json.Unmarshal(launchBody, &launch); err != nil {
			return nil, err
		}
	}
	return overlayClaudeLaunchSettings(cli, launch)
}
