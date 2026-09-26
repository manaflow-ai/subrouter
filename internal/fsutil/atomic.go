package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
)

// ReplaceFile atomically replaces path with body: it writes a uniquely named
// temporary sibling, fsyncs it, and renames it over path. It does not sync
// the parent directory; callers that need the rename itself to survive power
// loss follow it with SyncDir (or use WriteFileAtomic).
func ReplaceFile(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

// SyncDir fsyncs a directory so renames and creations inside it are durable.
// Windows cannot fsync a directory handle and persists renames with the
// metadata journal, so it is a no-op there.
func SyncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

// WriteFileAtomic durably replaces path with body so a crash leaves either the
// previous contents or the new contents, never a truncated file.
func WriteFileAtomic(path string, body []byte, mode os.FileMode) error {
	if err := ReplaceFile(path, body, mode); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}
