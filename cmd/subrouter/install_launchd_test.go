package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const testLaunchDaemonPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>/var/lib/subrouter</string>
		<key>SUBROUTER_STATE_DIR</key>
		<string>/var/lib/subrouter</string>
		<key>UserName</key>
		<string>decoy-inside-a-nested-dict</string>
	</dict>
	<key>KeepAlive</key>
	<true/>
	<key>Label</key>
	<string>ai.manaflow.subrouter-team</string>
	<key>ProgramArguments</key>
	<array>
		<string>/usr/local/libexec/subrouter-supervisor</string>
		<string>supervise</string>
		<string>--addr</string>
		<string>0.0.0.0:31415</string>
		<string>--worker-bin</string>
		<string>/usr/local/bin/subrouter</string>
		<string>--</string>
		<string>--bedrock</string>
		<string>--bedrock-profiles</string>
		<string>aw1</string>
	</array>
	<key>UserName</key>
	<string>_subrouter</string>
</dict>
</plist>
`

func TestPlistTopLevelStringIgnoresNestedKeys(t *testing.T) {
	user, found, err := plistTopLevelString([]byte(testLaunchDaemonPlist), "UserName")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("UserName not found")
	}
	if user != "_subrouter" {
		t.Fatalf("UserName = %q, want _subrouter (the nested value must not win)", user)
	}
}

func TestPlistTopLevelStringReportsMissingKey(t *testing.T) {
	_, found, err := plistTopLevelString([]byte(testLaunchDaemonPlist), "GroupName")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("GroupName should not be found")
	}
}

func TestLaunchdPlistCommandsReplaceInlineTokensWithFiles(t *testing.T) {
	config := launchdConfig{
		Label:              defaultLaunchdServerLabel,
		StateDir:           "/var/lib/subrouter",
		AdminToken:         "admin",
		AccountImportToken: "import",
	}
	commands := strings.Join(launchdPlistCommands(config), "\n")

	for _, want := range []string{
		"Delete :EnvironmentVariables:SUBROUTER_ADMIN_TOKEN",
		"Delete :EnvironmentVariables:SUBROUTER_ACCOUNT_IMPORT_TOKEN",
		"Add :EnvironmentVariables:SUBROUTER_ADMIN_TOKEN_FILE string /var/lib/subrouter/admin-token",
		"Add :EnvironmentVariables:SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE string /var/lib/subrouter/account-import-token",
	} {
		if !strings.Contains(commands, want) {
			t.Fatalf("commands missing %q:\n%s", want, commands)
		}
	}
	// The token values themselves belong in files, never in the plist, which is
	// world readable.
	if strings.Contains(commands, "admin") && strings.Contains(commands, "string admin\n") {
		t.Fatalf("commands leaked a token value:\n%s", commands)
	}
}

func TestValidateLaunchdConfigRequiresBothTokens(t *testing.T) {
	base := launchdConfig{Label: "x", StateDir: "/var/lib/subrouter"}
	for name, config := range map[string]launchdConfig{
		"no tokens": base,
		"admin only": func() launchdConfig {
			c := base
			c.AdminToken = "a"
			return c
		}(),
		"import only": func() launchdConfig {
			c := base
			c.AccountImportToken = "i"
			return c
		}(),
		"relative state dir": {Label: "x", StateDir: "relative", AdminToken: "a", AccountImportToken: "i"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateLaunchdConfig(config); err == nil {
				t.Fatal("expected validation to fail")
			}
		})
	}
	full := base
	full.AdminToken = "a"
	full.AccountImportToken = "i"
	if err := validateLaunchdConfig(full); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestInstallLaunchdDryRunPrintsPlanWithoutTouchingTheHost(t *testing.T) {
	var out bytes.Buffer
	config := launchdConfig{
		Label:              defaultLaunchdServerLabel,
		PlistPath:          filepath.Join(t.TempDir(), "absent.plist"),
		StateDir:           "/var/lib/subrouter",
		AdminToken:         "admin",
		AccountImportToken: "import",
		DryRun:             true,
	}
	if err := installLaunchdWithConfig(config, commandRunner{}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "/var/lib/subrouter/account-import-token (0600)") {
		t.Fatalf("dry run did not describe the credential files:\n%s", out.String())
	}
	if _, err := os.Stat(config.PlistPath); !os.IsNotExist(err) {
		t.Fatal("dry run created the plist")
	}
}

// The patch has to be surgical: a host carries flags nobody remembers (Bedrock
// profiles, KeepAlive, its service user), and losing them on a credential
// rotation is how a routine change turns into an outage.
func TestPlistBuddyPatchPreservesEverythingElse(t *testing.T) {
	if _, err := os.Stat(plistBuddyPath); err != nil {
		t.Skipf("PlistBuddy unavailable: %v", err)
	}
	plist := filepath.Join(t.TempDir(), "ai.manaflow.subrouter-team.plist")
	if err := os.WriteFile(plist, []byte(testLaunchDaemonPlist), 0o644); err != nil {
		t.Fatal(err)
	}
	config := launchdConfig{
		Label:              defaultLaunchdServerLabel,
		PlistPath:          plist,
		StateDir:           "/var/lib/subrouter",
		AdminToken:         "admin",
		AccountImportToken: "import",
	}
	runner := commandRunner{}
	for _, command := range launchdPlistCommands(config) {
		if strings.HasPrefix(command, "Delete ") {
			_ = runner.Run(plistBuddyPath, "-c", command, plist)
			continue
		}
		if err := runner.Run(plistBuddyPath, "-c", command, plist); err != nil {
			t.Fatal(err)
		}
	}
	if err := runner.Run("plutil", "-lint", plist); err != nil {
		t.Fatalf("patched plist is invalid: %v", err)
	}

	patched, err := exec.Command("plutil", "-convert", "xml1", "-o", "-", plist).Output()
	if err != nil {
		t.Fatal(err)
	}
	body := string(patched)
	for _, want := range []string{
		"SUBROUTER_ADMIN_TOKEN_FILE",
		"/var/lib/subrouter/admin-token",
		"SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE",
		"/var/lib/subrouter/account-import-token",
		"--bedrock-profiles",
		"<string>aw1</string>",
		"<key>KeepAlive</key>",
		"<string>_subrouter</string>",
		"<string>/usr/local/libexec/subrouter-supervisor</string>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("patched plist lost %q:\n%s", want, body)
		}
	}
	user, found, err := plistTopLevelString(patched, "UserName")
	if err != nil || !found || user != "_subrouter" {
		t.Fatalf("service user after patch = %q (found=%v, err=%v)", user, found, err)
	}
}

func TestWriteCredentialFileIsPrivateAndAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, adminTokenFileName)
	if err := writeCredentialFile(path, "  secret\n", "", commandRunner{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "secret\n" {
		t.Fatalf("body = %q, want the trimmed token", string(body))
	}
	if _, err := os.Stat(path + ".new"); !os.IsNotExist(err) {
		t.Fatal("temporary credential file survived")
	}
}
