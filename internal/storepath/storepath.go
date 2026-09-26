package storepath

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/fsutil"
)

const stateDirEnv = "SUBROUTER_STATE_DIR"

// legacyCodexMigrationMarker records inside the Codex target that the one-time
// ~/.codex-accounts import has already been decided. Without it, deleting
// every account would make the next start import the legacy credentials again.
// The name starts with "." and has no ".json" suffix, so account listing
// ignores it.
const legacyCodexMigrationMarker = ".legacy-codex-migrated"

func StateDir() string {
	if dir := strings.TrimSpace(os.Getenv(stateDirEnv)); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".subrouter"
	}
	return filepath.Join(home, ".subrouter")
}

func CodexDir() string {
	dir := filepath.Join(StateDir(), "codex")
	_ = MigrateLegacyCodexDir(dir)
	return dir
}

// CodexDirForReadOnlyInspection returns the directory CodexDir would serve
// after its best-effort legacy migration, without performing that migration.
func CodexDirForReadOnlyInspection() string {
	return CodexDirForStateRootReadOnlyInspection(StateDir())
}

// CodexDirForStateRootReadOnlyInspection resolves the effective Codex source
// for an explicit state root without copying or locking either source.
func CodexDirForStateRootReadOnlyInspection(stateRoot string) string {
	target := filepath.Join(stateRoot, "codex")
	source, importsLegacy, err := codexMigrationSource(target, LegacyCodexDir())
	if err != nil || !importsLegacy {
		// CodexDir ignores migration failures and serves the candidate target.
		return filepath.Clean(target)
	}
	return source
}

func LegacyCodexDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".codex-accounts"
	}
	return filepath.Join(home, ".codex-accounts")
}

func MigrateLegacyCodexDir(target string) error {
	return MigrateCodexDir(target, LegacyCodexDir())
}

func MigrateCodexDir(target, legacy string) error {
	source, importsLegacy, err := codexMigrationSource(target, legacy)
	if err != nil {
		return err
	}
	target = filepath.Clean(target)
	if !importsLegacy {
		// A target that already holds accounts next to a legacy dir was
		// migrated before the marker existed (or populated independently);
		// record that so emptying it later cannot trigger an import.
		if source == target && legacyDirExists(target, legacy) {
			if marked, err := pathExists(filepath.Join(target, legacyCodexMigrationMarker)); err == nil && !marked {
				return writeMigrationMarker(target)
			}
		}
		return nil
	}
	legacy = source
	if exists, err := pathExists(target); err != nil {
		return err
	} else if exists {
		if err := copyDirContents(legacy, target); err != nil {
			return err
		}
		return writeMigrationMarker(target)
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(target), "."+filepath.Base(target)+".migrate-*")
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(tmp)
		}
	}()
	if err := copyDirContents(legacy, tmp); err != nil {
		return err
	}
	if err := writeMigrationMarker(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		nonEmpty, checkErr := dirNonEmpty(target)
		if checkErr == nil && nonEmpty {
			return nil
		}
		return err
	}
	cleanup = false
	return fsutil.SyncDir(filepath.Dir(target))
}

func legacyDirExists(target, legacy string) bool {
	if filepath.Clean(legacy) == target {
		return false
	}
	info, err := os.Stat(legacy)
	return err == nil && info.IsDir()
}

func writeMigrationMarker(dir string) error {
	return fsutil.WriteFileAtomic(filepath.Join(dir, legacyCodexMigrationMarker), []byte("imported from ~/.codex-accounts\n"), 0o600)
}

// codexMigrationSource is the single eligibility decision used by both the
// mutating migration and read-only inspection. importsLegacy means an empty or
// absent candidate would be populated from the returned legacy directory.
func codexMigrationSource(target, legacy string) (source string, importsLegacy bool, err error) {
	target = filepath.Clean(target)
	legacy = filepath.Clean(legacy)
	if target == legacy {
		return target, false, nil
	}
	info, err := os.Stat(legacy)
	if err != nil {
		if os.IsNotExist(err) {
			return target, false, nil
		}
		return target, false, err
	}
	if !info.IsDir() {
		return target, false, nil
	}
	if marked, err := pathExists(filepath.Join(target, legacyCodexMigrationMarker)); err != nil {
		return target, false, err
	} else if marked {
		return target, false, nil
	}
	nonEmpty, err := dirNonEmpty(target)
	if err != nil {
		return target, false, err
	}
	if nonEmpty {
		return target, false, nil
	}
	return legacy, true, nil
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func dirNonEmpty(dir string) (bool, error) {
	nonEmpty := false
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == dir {
			return nil
		}
		if shouldSkipLegacyEntry(entry.Name()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		nonEmpty = true
		return filepath.SkipAll
	})
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return nonEmpty, nil
}

func copyDirContents(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == src {
			return nil
		}
		if shouldSkipLegacyEntry(entry.Name()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			mode := info.Mode().Perm()
			if mode == 0 {
				mode = 0o700
			}
			return os.MkdirAll(target, mode)
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func shouldSkipLegacyEntry(name string) bool {
	return strings.HasPrefix(name, "._") || strings.HasSuffix(name, ".lock") || (strings.HasPrefix(name, ".") && strings.Contains(name, ".tmp-"))
}

func copyFile(src, dst string, mode os.FileMode) error {
	if mode == 0 {
		mode = 0o600
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		_ = out.Close()
		if cleanup {
			_ = os.Remove(dst)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	cleanup = false
	return nil
}
