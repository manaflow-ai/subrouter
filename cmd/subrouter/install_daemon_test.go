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

// A tailnet bind is the whole point of team sharing, so it must be installable —
// but only in a state `serve` would itself accept at startup. install-daemon used
// to refuse every non-loopback address outright, which made the chosen topology
// unreachable through the supported install path.
func TestValidateDaemonConfigRefusesNonLoopbackBindWithoutAdminToken(t *testing.T) {
	base := daemonConfig{
		Label:            defaultDaemonLabel,
		InstallPath:      "/Users/alice/bin/subrouter",
		LogDir:           "/Users/alice/Library/Logs",
		WorkingDirectory: "/Users/alice/fun/subrouter",
		SRSwitchInterval: "10m",
	}
	for _, addr := range []string{
		"100.101.102.103:31415",     // tailnet IPv4
		"mac.tail1234.ts.net:31415", // MagicDNS: unresolvable at validation time
		":31415",                    // every interface
	} {
		config := base
		config.Addr = addr
		err := validateDaemonConfig(config)
		if err == nil {
			t.Fatalf("addr %q: expected refusal without an admin token", addr)
		}
		if !strings.Contains(err.Error(), "admin token") {
			t.Fatalf("addr %q: error should name the missing admin token, got %v", addr, err)
		}
	}
}

// writeAdminTokenFile creates a token file the daemon could actually read, so
// validation exercises the real reader rather than a path that happens to
// look plausible.
func writeAdminTokenFile(t *testing.T, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "admin-token")
	if err := os.WriteFile(path, []byte("secret-admin-token\n"), mode); err != nil {
		t.Fatalf("write admin token file: %v", err)
	}
	return path
}

func TestValidateDaemonConfigAllowsLoopbackAndTokenedTailnetBinds(t *testing.T) {
	tokenFile := writeAdminTokenFile(t, 0o600)
	base := daemonConfig{
		Label:            defaultDaemonLabel,
		InstallPath:      "/Users/alice/bin/subrouter",
		LogDir:           "/Users/alice/Library/Logs",
		WorkingDirectory: "/Users/alice/fun/subrouter",
		SRSwitchInterval: "10m",
	}
	cases := []struct {
		name   string
		mutate func(*daemonConfig)
	}{
		{"loopback needs no token", func(c *daemonConfig) { c.Addr = "127.0.0.1:31415" }},
		{"localhost needs no token", func(c *daemonConfig) { c.Addr = "localhost:31415" }},
		{"tailnet with token file", func(c *daemonConfig) {
			c.Addr = "100.101.102.103:31415"
			c.AdminTokenFile = tokenFile
		}},
		{"tailnet with inline token", func(c *daemonConfig) {
			c.Addr = "100.101.102.103:31415"
			c.AdminToken = "secret-admin-token"
		}},
	}
	for _, tc := range cases {
		config := base
		tc.mutate(&config)
		if err := validateDaemonConfig(config); err != nil {
			t.Fatalf("%s: unexpected error %v", tc.name, err)
		}
	}
}

func TestValidateDaemonConfigRejectsBothTokenForms(t *testing.T) {
	config := daemonConfig{
		Label:            defaultDaemonLabel,
		Addr:             "100.101.102.103:31415",
		AdminToken:       "secret-admin-token",
		AdminTokenFile:   writeAdminTokenFile(t, 0o600),
		InstallPath:      "/Users/alice/bin/subrouter",
		LogDir:           "/Users/alice/Library/Logs",
		WorkingDirectory: "/Users/alice/fun/subrouter",
		SRSwitchInterval: "10m",
	}
	if err := validateDaemonConfig(config); err == nil {
		t.Fatal("expected an error when both token forms are passed")
	}
}

// The security property that motivates the env-var plumbing: launchd
// ProgramArguments are visible in `ps` to every local user, so an admin token
// must never be an argv element.
func TestLaunchAgentPlistKeepsAdminTokenOutOfProgramArguments(t *testing.T) {
	home := "/Users/alice"
	config := daemonConfig{
		Label:            defaultDaemonLabel,
		Addr:             "100.101.102.103:31415",
		AdminToken:       "super-secret-admin-token",
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
	if !strings.Contains(plist, "<key>SUBROUTER_ADMIN_TOKEN</key>") {
		t.Fatal("expected the token to be exported as an environment variable")
	}
	args := plist[strings.Index(plist, "<key>ProgramArguments</key>"):]
	args = args[:strings.Index(args, "</array>")]
	if strings.Contains(args, config.AdminToken) {
		t.Fatal("admin token leaked into ProgramArguments, where ps exposes it to every local user")
	}
	if mode := launchAgentMode(config); mode != 0o600 {
		t.Fatalf("a plist carrying an inline token must be 0600, got %#o", mode)
	}
}

func TestLaunchAgentPlistPrefersTokenFileAndLeavesPlistReadable(t *testing.T) {
	home := "/Users/alice"
	tokenFile := writeAdminTokenFile(t, 0o600)
	config := daemonConfig{
		Label:            defaultDaemonLabel,
		Addr:             "mac.tail1234.ts.net:31415",
		AdminTokenFile:   tokenFile,
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
	if !strings.Contains(plist, "<key>SUBROUTER_ADMIN_TOKEN_FILE</key>") ||
		!strings.Contains(plist, "<string>"+tokenFile+"</string>") {
		t.Fatal("expected SUBROUTER_ADMIN_TOKEN_FILE in the plist environment")
	}
	if strings.Contains(plist, "<key>SUBROUTER_ADMIN_TOKEN</key>") {
		t.Fatal("token-file mode must not also emit an inline SUBROUTER_ADMIN_TOKEN")
	}
	// No secret in the plist, so it does not need tightening.
	if mode := launchAgentMode(config); mode != 0o644 {
		t.Fatalf("token-file mode should leave the plist at 0644, got %#o", mode)
	}
}

// An admin token file the daemon cannot read has to fail here, not at boot.
// The plist sets KeepAlive, so serve would exit, launchd would restart it, and
// the operator's only signal would be churn in subrouter.err.log.
func TestValidateDaemonConfigRejectsUnusableAdminTokenFile(t *testing.T) {
	dir := t.TempDir()

	empty := filepath.Join(dir, "empty-token")
	if err := os.WriteFile(empty, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
	}{
		{"missing", filepath.Join(dir, "does-not-exist")},
		{"directory", dir},
		{"empty once trimmed", empty},
	}
	for _, tc := range cases {
		config := daemonConfig{
			Label:            defaultDaemonLabel,
			Addr:             "100.101.102.103:31415",
			AdminTokenFile:   tc.path,
			InstallPath:      "/Users/alice/bin/subrouter",
			LogDir:           "/Users/alice/Library/Logs",
			WorkingDirectory: "/Users/alice/fun/subrouter",
			SRSwitchInterval: "10m",
		}
		if err := validateDaemonConfig(config); err == nil {
			t.Fatalf("%s: expected install to refuse an unusable admin token file", tc.name)
		}
	}
}

// launchd resolves relative paths against WorkingDirectory, not the shell the
// installer ran in, so the plist has to carry an absolute path or it silently
// reads a different file — or none.
func TestResolveAdminTokenFileReturnsAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "admin-token"), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	resolved, err := resolveAdminTokenFile("admin-token")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(resolved) {
		t.Fatalf("resolved token path %q is not absolute", resolved)
	}
	if filepath.Base(resolved) != "admin-token" {
		t.Fatalf("resolved token path %q lost the file name", resolved)
	}
}

// --admin-token-file exists to keep the secret out of the plist. A token file
// every other local user can read gives that back, so it must not pass quietly.
func TestAdminTokenFileWarningFlagsGroupAndWorldReadableModes(t *testing.T) {
	if adminTokenFileWarning("/tmp/admin-token", 0o600) != "" {
		t.Fatal("0600 should not warn")
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o666} {
		if adminTokenFileWarning("/tmp/admin-token", mode) == "" {
			t.Fatalf("mode %04o should warn", mode)
		}
	}
}
