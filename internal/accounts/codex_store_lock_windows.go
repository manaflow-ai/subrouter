//go:build windows

package accounts

import (
	"os"
	"path/filepath"
)

type accountFileLock struct {
	file *os.File
}

func (s CodexStore) lockStoredAccount(email string) (*accountFileLock, error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(s.Dir, "."+accountLockFilename(email)+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return &accountFileLock{file: file}, nil
}

func (l *accountFileLock) Close() error {
	return l.file.Close()
}
