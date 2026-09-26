//go:build !windows

package qwen

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestConsoleCredentialVersionRejectsSpecialFile(t *testing.T) {
	root := t.TempDir()
	accountID := "qwen-token:unsafe-special"
	dir := ConsoleConfigDirIn(root, accountID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "credential.pipe"), 0o600); err != nil {
		t.Skipf("named pipes unavailable: %v", err)
	}
	found, version, err := ConsoleCredentialVersionIn(root, accountID)
	if err == nil || found || version != "" || !strings.Contains(err.Error(), "unsafe special file") {
		t.Fatalf("unsafe-special version = found %v version %q err %v", found, version, err)
	}
}
