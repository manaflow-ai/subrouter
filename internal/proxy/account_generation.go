package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const accountDiskGenerationFile = ".account-generation"

func accountDiskGenerationPath(storeDir string) string {
	return filepath.Join(storeDir, accountDiskGenerationFile)
}

func readAccountDiskGeneration(storeDir string) (string, error) {
	body, err := os.ReadFile(accountDiskGenerationPath(storeDir))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// advanceAccountDiskGeneration publishes one completed disk mutation to every
// overlapping supervisor worker. Callers hold the cross-process import lock,
// so truncation cannot expose a partial generation to another reload.
func advanceAccountDiskGeneration(storeDir string) (err error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return fmt.Errorf("generate account state generation: %w", err)
	}
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(storeDir, ".account-generation-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer func() {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.WriteString(hex.EncodeToString(value)); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	closeErr := file.Close()
	file = nil
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(tempPath, accountDiskGenerationPath(storeDir)); err != nil {
		return err
	}
	if dir, openErr := os.Open(storeDir); openErr == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func (r *AccountRef) reloadIfDiskGenerationChanged() (reloaded bool, err error) {
	if r == nil {
		return false, nil
	}
	diskGeneration, err := readAccountDiskGeneration(r.store.StoreDir())
	if err != nil {
		return false, err
	}
	r.mu.RLock()
	unchanged := diskGeneration == r.diskGeneration
	r.mu.RUnlock()
	if unchanged {
		return false, nil
	}

	r.installMu.Lock()
	defer r.installMu.Unlock()
	lock, err := lockAccountImportTransaction(r.store.StoreDir())
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := lock.Close(); err == nil {
			err = closeErr
		}
	}()
	diskGeneration, err = readAccountDiskGeneration(r.store.StoreDir())
	if err != nil {
		return false, err
	}
	r.mu.RLock()
	unchanged = diskGeneration == r.diskGeneration
	r.mu.RUnlock()
	if unchanged {
		return false, nil
	}
	if _, err := r.Reload(); err != nil {
		return false, err
	}
	return true, nil
}
