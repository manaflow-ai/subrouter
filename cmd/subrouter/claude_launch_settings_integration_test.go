//go:build !windows

package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstalledClaudeAcceptsPrivateSettingsTransport(t *testing.T) {
	t.Parallel()
	path, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("Claude CLI unavailable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path)
	settingsArg, cleanup, err := attachClaudeLaunchSettings(cmd, []byte(`{"env":{"SUBROUTER_TRANSPORT_SMOKE":"1"}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	cmd.Args = []string{path, "--settings", settingsArg, "--version"}
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Claude CLI rejected private regular settings file: %v\n%s", err, out)
	}
}

func TestClaudeSettingsGuardianRemovesFileAfterLauncherSIGKILL(t *testing.T) {
	if os.Getenv("SUBROUTER_CLAUDE_GUARDIAN_LAUNCHER_HELPER") == "1" {
		marker := os.Getenv("SUBROUTER_CLAUDE_GUARDIAN_MARKER")
		claudeSettingsAfterWriteHook = func(settingsPath string) {
			if err := os.WriteFile(marker, []byte(settingsPath), 0o600); err != nil {
				t.Fatal(err)
			}
			select {}
		}
		_, _, err := attachClaudeLaunchSettings(exec.Command("true"), []byte(`{"env":{"TOKEN":"secret"}}`))
		t.Fatalf("attach unexpectedly escaped after-write hook: %v", err)
	}
	tempRoot := t.TempDir()
	marker := filepath.Join(tempRoot, "settings-path")
	cmd := exec.Command(os.Args[0], "-test.run=^TestClaudeSettingsGuardianRemovesFileAfterLauncherSIGKILL$")
	cmd.Env = append(os.Environ(),
		"TMPDIR="+tempRoot,
		"SUBROUTER_CLAUDE_GUARDIAN_LAUNCHER_HELPER=1",
		"SUBROUTER_CLAUDE_GUARDIAN_MARKER="+marker,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitUntil := time.Now().Add(5 * time.Second)
	var settingsPath string
	for time.Now().Before(waitUntil) {
		if body, err := os.ReadFile(marker); err == nil {
			settingsPath = strings.TrimSpace(string(body))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if settingsPath == "" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("launcher helper did not publish settings path")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	waitUntil = time.Now().Add(5 * time.Second)
	for time.Now().Before(waitUntil) {
		if _, err := os.Stat(settingsPath); os.IsNotExist(err) {
			if _, err := os.Stat(filepath.Dir(settingsPath)); os.IsNotExist(err) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("guardian left settings after launcher SIGKILL: %s", settingsPath)
}

func TestConcurrentClaudeSettingsTransportsAreIsolated(t *testing.T) {
	firstBody := []byte(`{"env":{"TOKEN":"first"}}`)
	secondBody := []byte(`{"env":{"TOKEN":"second"}}`)
	firstPath, cleanupFirst, err := attachClaudeLaunchSettings(exec.Command("true"), firstBody)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupFirst()
	secondPath, cleanupSecond, err := attachClaudeLaunchSettings(exec.Command("true"), secondBody)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupSecond()
	if firstPath == secondPath || filepath.Dir(firstPath) == filepath.Dir(secondPath) {
		t.Fatalf("concurrent settings shared a scope: %q %q", firstPath, secondPath)
	}
	if got, err := os.ReadFile(firstPath); err != nil || !bytes.Equal(got, firstBody) {
		t.Fatalf("first settings = %q, err=%v", got, err)
	}
	if got, err := os.ReadFile(secondPath); err != nil || !bytes.Equal(got, secondBody) {
		t.Fatalf("second settings = %q, err=%v", got, err)
	}
	cleanupFirst()
	if _, err := os.Stat(firstPath); !os.IsNotExist(err) {
		t.Fatalf("first settings survived cleanup: %v", err)
	}
	if got, err := os.ReadFile(secondPath); err != nil || !bytes.Equal(got, secondBody) {
		t.Fatalf("first cleanup damaged second settings: %q, err=%v", got, err)
	}
}

func TestClaudeSettingsGuardianCommandContainsNoRoutingSecrets(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	secret := "srt_guardian_must_not_leak"
	cmd := newClaudeSettingsGuardianCommand("/opaque/subrouter", filepath.Join(os.TempDir(), claudeSettingsDirPrefix+"opaque"), reader, []string{
		"TMPDIR=" + os.TempDir(),
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
