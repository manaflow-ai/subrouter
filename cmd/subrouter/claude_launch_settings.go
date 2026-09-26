package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	claudeSettingsCleanupMode = "__subrouter-claude-settings-cleanup"
	claudeSettingsDirPrefix   = "subrouter-claude-settings-"
)

var claudeSettingsAfterWriteHook = func(string) {}

// The cleanup helper must also work when the executable is a Go test binary,
// whose testing harness owns main. Exact private-mode arguments are handled
// during initialization before either main can inspect them.
func init() {
	if len(os.Args) == 3 && os.Args[1] == claudeSettingsCleanupMode {
		os.Exit(runClaudeSettingsCleanupGuardian(os.Stdin, os.Args[2]))
	}
}

// attachClaudeLaunchSettings gives Claude a private regular settings file.
// Claude canonicalizes and reopens --settings paths, so descriptors, pipes,
// and unlinked inodes are not compatible. A separate copy of subrouter watches
// a CLOEXEC pipe and removes the scoped directory if this process is killed.
func attachClaudeLaunchSettings(_ *exec.Cmd, body []byte) (string, func(), error) {
	dir, err := createSecureClaudeSettingsDir()
	if err != nil {
		return "", func() {}, err
	}
	settingsPath := filepath.Join(dir, "settings.json")
	remove := func() {
		_ = os.Remove(settingsPath)
		_ = os.Remove(dir)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		remove()
		return "", func() {}, fmt.Errorf("create Claude settings cleanup signal: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		remove()
		return "", func() {}, fmt.Errorf("locate Claude settings cleanup helper: %w", err)
	}
	guardian := newClaudeSettingsGuardianCommand(executable, dir, reader, os.Environ())
	if err := guardian.Start(); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		remove()
		return "", func() {}, fmt.Errorf("start Claude settings cleanup guardian: %w", err)
	}
	_ = reader.Close()

	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			_ = writer.Close()
			_ = guardian.Wait()
			remove()
		})
	}
	if err := os.WriteFile(settingsPath, body, 0o600); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("write private Claude launch settings: %w", err)
	}
	file, err := os.OpenFile(settingsPath, os.O_RDWR, 0)
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("open private Claude launch settings: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("sync private Claude launch settings: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("close private Claude launch settings: %w", err)
	}
	claudeSettingsAfterWriteHook(settingsPath)
	return settingsPath, cleanup, nil
}

func newClaudeSettingsGuardianCommand(executable, dir string, signal *os.File, environ []string) *exec.Cmd {
	guardian := exec.Command(executable, claudeSettingsCleanupMode, dir)
	guardian.Stdin = signal
	guardian.Env = claudeSettingsGuardianEnvironment(environ)
	return guardian
}

func claudeSettingsGuardianEnvironment(environ []string) []string {
	keep := make([]string, 0, 2)
	for _, entry := range environ {
		name := entry
		if index := strings.IndexByte(entry, '='); index >= 0 {
			name = entry[:index]
		}
		switch strings.ToUpper(name) {
		case "SYSTEMROOT", "WINDIR", "TMPDIR", "TEMP", "TMP":
			keep = append(keep, entry)
		}
	}
	return keep
}

func runClaudeSettingsCleanupGuardian(signal io.Reader, dir string) int {
	_, _ = io.Copy(io.Discard, signal)
	if !validClaudeSettingsDir(dir) {
		return 2
	}
	settingsPath := filepath.Join(dir, "settings.json")
	deadline := time.Now().Add(2 * time.Minute)
	for {
		fileErr := os.Remove(settingsPath)
		if fileErr == nil || errors.Is(fileErr, os.ErrNotExist) {
			dirErr := os.Remove(dir)
			if dirErr == nil || errors.Is(dirErr, os.ErrNotExist) {
				return 0
			}
		}
		if time.Now().After(deadline) {
			return 1
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func validClaudeSettingsDir(dir string) bool {
	clean := filepath.Clean(dir)
	if !filepath.IsAbs(clean) || !strings.HasPrefix(filepath.Base(clean), claudeSettingsDirPrefix) {
		return false
	}
	tempRoot, err := filepath.Abs(os.TempDir())
	return err == nil && filepath.Clean(filepath.Dir(clean)) == filepath.Clean(tempRoot)
}
