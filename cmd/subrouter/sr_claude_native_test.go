package main

import (
	"strings"
	"testing"
)

func TestParseClaudeDefaultRoute(t *testing.T) {
	for _, tt := range []struct {
		value string
		want claudeDefaultRoute
	}{
		{"", claudeDefaultPooled},
		{"pooled", claudeDefaultPooled},
		{" NATIVE ", claudeDefaultNative},
	} {
		got, err := parseClaudeDefaultRoute(tt.value)
		if err != nil || got != tt.want {
			t.Errorf("parseClaudeDefaultRoute(%q) = %q, %v; want %q", tt.value, got, err, tt.want)
		}
	}
	for _, raw := range []string{"api", "team-api", "proxy", "wat"} {
		if _, err := parseClaudeDefaultRoute(raw); err == nil {
			t.Errorf("parseClaudeDefaultRoute(%q) unexpectedly succeeded", raw)
		}
	}
}

func TestClaudeImplicitAgentLaunch(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want bool
	}{
		{nil, true},
		{[]string{}, true},
		{[]string{"-p", "hello"}, true},
		{[]string{"--resume", "session-id"}, true},
		{[]string{"--output-format", "stream-json"}, true},
		{[]string{"proxy", "-p", "hello"}, false},
		{[]string{"run", "work"}, false},
		{[]string{"login", "work"}, false},
		{[]string{"list"}, false},
		{[]string{"--help"}, false},
		{[]string{"-h"}, false},
	} {
		if got := claudeImplicitAgentLaunch(tt.args); got != tt.want {
			t.Errorf("claudeImplicitAgentLaunch(%q) = %t, want %t", tt.args, got, tt.want)
		}
	}
}

func TestClaudeNativeProfileName(t *testing.T) {
	for _, tt := range []struct {
		configured string
		active string
		want string
	}{
		{"", "", ""},
		{"", "local", "local"},
		{" work ", "personal", "work"},
	} {
		if got := claudeNativeProfileName(tt.configured, tt.active); got != tt.want {
			t.Errorf("claudeNativeProfileName(%q, %q) = %q, want %q", tt.configured, tt.active, got, tt.want)
		}
	}
}

func TestClaudeDirectChildEnvironment(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin", "ANTHROPIC_BASE_URL=https://some.proxy",
		"ANTHROPIC_API_KEY=secret", "CLAUDE_CODE_OAUTH_TOKEN=token",
		"SUBROUTER_ADMIN_TOKEN=secret", "CLAUDE_CONFIG_DIR=/other",
		"CLAUDE_CODE_USE_BEDROCK=1",
	}
	check := func(configDir string) {
		t.Helper()
		env := claudeDirectChildEnvironment(parent, configDir)
		values := map[string]string{}
		for _, item := range env {
			key, value, _ := strings.Cut(item, "=")
			values[key] = value
		}
		for _, key := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "SUBROUTER_ADMIN_TOKEN", "CLAUDE_CODE_USE_BEDROCK"} {
			if _, ok := values[key]; ok {
				t.Errorf("config %q leaked %s", configDir, key)
			}
		}
		if values["PATH"] != "/usr/bin" {
			t.Errorf("PATH lost: %v", values)
		}
		if configDir == "" {
			if _, ok := values["CLAUDE_CONFIG_DIR"]; ok {
				t.Errorf("default native login must leave CLAUDE_CONFIG_DIR absent: %v", values)
			}
		} else if values["CLAUDE_CONFIG_DIR"] != configDir || values["CLAUDE_CODE_CONFIG_DIR"] != configDir {
			t.Errorf("native profile %q does not pin both selectors: %v", configDir, values)
		}
	}
	check("")
	check("/home/teammate/.subrouter/claude/work")
}
