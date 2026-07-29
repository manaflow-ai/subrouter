package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLaunchAgentPlistUsesMacOSLocalDefaults(t *testing.T) {
	home := "/Users/alice"
	t.Setenv(
		"SUBROUTER_CLOUD_CONFIG",
		"/Users/alice/private/subrouter-team.json",
	)
	config := daemonConfig{
		Label:            defaultDaemonLabel,
		Addr:             "127.0.0.1:31415",
		InstallPath:      "/Users/alice/bin/subrouter",
		LogDir:           "/Users/alice/Library/Logs",
		WorkingDirectory: "/Users/alice/fun/subrouter",
		SRSwitchInterval: "10m",
		Path:             defaultDaemonPath("/Users/alice/bin/subrouter"),
	}
	plist, err := launchAgentPlist(config, home)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<string>ai.manaflow.subrouter</string>",
		"<string>/Users/alice/bin/subrouter</string>",
		"<string>serve</string>",
		"<string>--addr</string>",
		"<string>127.0.0.1:31415</string>",
		"<string>--sr-switch-interval</string>",
		"<string>10m</string>",
		"<string>--cloud-config</string>",
		"<string>/Users/alice/private/subrouter-team.json</string>",
		"<string>/Users/alice/fun/subrouter</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist missing %q:\n%s", want, plist)
		}
	}
	if strings.Contains(plist, "<string>--transcripts</string>") {
		t.Fatalf("plist enabled transcripts by default:\n%s", plist)
	}
}

func TestLaunchAgentPlistIncludesConfiguredTranscripts(t *testing.T) {
	home := "/Users/alice"
	config := daemonConfig{
		Label:            defaultDaemonLabel,
		Addr:             "127.0.0.1:31415",
		InstallPath:      "/Users/alice/bin/subrouter",
		TranscriptsDir:   "/Users/alice/.subrouter/transcripts",
		LogDir:           "/Users/alice/Library/Logs",
		WorkingDirectory: "/Users/alice/fun/subrouter",
		SRSwitchInterval: "10m",
		Path:             defaultDaemonPath("/Users/alice/bin/subrouter"),
	}
	plist, err := launchAgentPlist(config, home)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<string>--transcripts</string>",
		"<string>/Users/alice/.subrouter/transcripts</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist missing %q:\n%s", want, plist)
		}
	}
}

func TestInstallDaemonWritesPlistAndInstallsExecutableWithoutStarting(t *testing.T) {
	home := t.TempDir()
	config := daemonConfig{
		Label:                defaultDaemonLabel,
		Addr:                 "127.0.0.1:31415",
		InstallPath:          filepath.Join(home, "bin", "subrouter"),
		TranscriptsDir:       filepath.Join(home, ".subrouter", "transcripts"),
		LogDir:               filepath.Join(home, "Library", "Logs"),
		WorkingDirectory:     home,
		SRSwitchInterval:     "10m",
		Path:                 defaultDaemonPath(filepath.Join(home, "bin", "subrouter")),
		InstallSRAlias:       true,
		SRAliasPath:          filepath.Join(home, "bin", "sr"),
		InstallLegacyCXAlias: true,
		LegacyCXAliasPath:    filepath.Join(home, "bin", "cx"),
		Start:                false,
	}
	if err := installDaemonWithConfig(config, home, commandRunner{}); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		config.InstallPath,
		config.SRAliasPath,
		config.LegacyCXAliasPath,
		config.TranscriptsDir,
		filepath.Join(home, "Library", "LaunchAgents", "ai.manaflow.subrouter.plist"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected %s: %v", path, err)
		}
	}

	body, err := os.ReadFile(filepath.Join(home, "Library", "LaunchAgents", "ai.manaflow.subrouter.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "<string>10m</string>") {
		t.Fatalf("plist did not preserve auto-switch interval:\n%s", body)
	}
	target, err := os.Readlink(config.SRAliasPath)
	if err != nil {
		t.Fatal(err)
	}
	if target != config.InstallPath {
		t.Fatalf("sr alias target = %q, want %q", target, config.InstallPath)
	}
	target, err = os.Readlink(config.LegacyCXAliasPath)
	if err != nil {
		t.Fatal(err)
	}
	if target != config.InstallPath {
		t.Fatalf("cx alias target = %q, want %q", target, config.InstallPath)
	}
}
