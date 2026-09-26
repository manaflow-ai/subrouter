//go:build windows

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWindowsClaudeLaunchSettingsUsePrivateCredentialFreeTransport(t *testing.T) {
	if os.Getenv("SUBROUTER_CLAUDE_FILE_TEST_HELPER") == "1" {
		windowsClaudeLaunchSettingsHelper(t)
		return
	}
	tempRoot := t.TempDir()
	copyPath := filepath.Join(tempRoot, "observed-settings.json")
	readyPath := filepath.Join(tempRoot, "ready")
	secret := "srt_windows_must_not_leak"
	body, err := proxyClaudeLaunchSettings("https://proxy.example", secret, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsClaudeLaunchSettingsUsePrivateCredentialFreeTransport$")
	settingsArg, cleanup, err := attachClaudeLaunchSettings(cmd, body)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	cmd.Args = append(cmd.Args, "--", settingsArg, copyPath, readyPath)
	cmd.Env = append(claudeSettingsChildEnvironment([]string{
		"PATH=" + os.Getenv("PATH"),
		"SYSTEMROOT=" + os.Getenv("SYSTEMROOT"),
		"ANTHROPIC_BASE_URL=https://proxy.example",
		"ANTHROPIC_AUTH_TOKEN=" + secret,
	}, "https://proxy.example", filepath.Join(tempRoot, "config")), "SUBROUTER_CLAUDE_FILE_TEST_HELPER=1")
	if got := strings.Join(cmd.Args, "\x00"); strings.Contains(got, secret) || strings.Contains(got, "proxy.example") {
		t.Fatalf("Claude argv exposed routing secret: %q", got)
	}
	if got := strings.Join(cmd.Env, "\x00"); strings.Contains(got, secret) || strings.Contains(got, "proxy.example") {
		t.Fatalf("Claude environment exposed routing secret: %q", got)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForPath(t, readyPath)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	cleanup()
	observed, err := os.ReadFile(copyPath)
	if err != nil || !bytes.Equal(observed, body) {
		t.Fatalf("child settings body = %q, err=%v", observed, err)
	}
	if _, err := os.Stat(settingsArg); !os.IsNotExist(err) {
		t.Fatalf("settings survived cleanup: %v", err)
	}
}

func TestWindowsClaudeLaunchSettingsCleanupBeforeChildStarts(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	settingsArg, cleanup, err := attachClaudeLaunchSettings(cmd, []byte(`{"env":{"ANTHROPIC_AUTH_TOKEN":"secret"}}`))
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if _, err := os.Stat(settingsArg); !os.IsNotExist(err) {
		t.Fatalf("settings survived early cleanup: %v", err)
	}
}

func TestWindowsClaudeSettingsGuardianCommandContainsNoRoutingSecrets(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	secret := "srt_guardian_must_not_leak"
	cmd := newClaudeSettingsGuardianCommand(`C:\opaque\subrouter.exe`, filepath.Join(os.TempDir(), claudeSettingsDirPrefix+"opaque"), reader, []string{
		"TEMP=" + os.TempDir(),
		"SYSTEMROOT=" + os.Getenv("SYSTEMROOT"),
		"ANTHROPIC_AUTH_TOKEN=" + secret,
		"ANTHROPIC_BASE_URL=https://secret.invalid/t/" + secret,
	})
	if got := strings.Join(cmd.Args, "\x00"); strings.Contains(got, secret) || strings.Contains(got, "secret.invalid") {
		t.Fatalf("guardian argv exposed routing secret: %q", got)
	}
	if got := strings.Join(cmd.Env, "\x00"); strings.Contains(got, secret) || strings.Contains(got, "secret.invalid") {
		t.Fatalf("guardian environment exposed routing secret: %q", got)
	}
}

func windowsClaudeLaunchSettingsHelper(t *testing.T) {
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || len(os.Args) != separator+4 {
		t.Fatalf("helper args = %#v", os.Args)
	}
	body, err := os.ReadFile(os.Args[separator+1])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Args[separator+2], body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Args[separator+3], nil, 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Minute)
}

func waitForPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
