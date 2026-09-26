//go:build windows

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func createSecureClaudeSettingsDir() (string, error) {
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("identify Claude settings directory owner: %w", err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;" + tokenUser.User.Sid.String() + ")")
	if err != nil {
		return "", fmt.Errorf("secure Claude settings directory: %w", err)
	}
	attributes := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	for attempt := 0; attempt < 10; attempt++ {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return "", fmt.Errorf("name Claude settings directory: %w", err)
		}
		dir := filepath.Join(os.TempDir(), claudeSettingsDirPrefix+hex.EncodeToString(random))
		name, err := windows.UTF16PtrFromString(dir)
		if err != nil {
			return "", fmt.Errorf("encode Claude settings directory name: %w", err)
		}
		if err := windows.CreateDirectory(name, &attributes); err == nil {
			return dir, nil
		} else if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return "", fmt.Errorf("create private Claude settings directory: %w", err)
		}
	}
	return "", errors.New("could not allocate a unique Claude settings directory")
}
