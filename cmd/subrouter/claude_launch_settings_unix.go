//go:build !windows

package main

import (
	"fmt"
	"os"
)

func createSecureClaudeSettingsDir() (string, error) {
	dir, err := os.MkdirTemp("", claudeSettingsDirPrefix)
	if err != nil {
		return "", fmt.Errorf("create private Claude settings directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.Remove(dir)
		return "", fmt.Errorf("secure Claude settings directory: %w", err)
	}
	return dir, nil
}
