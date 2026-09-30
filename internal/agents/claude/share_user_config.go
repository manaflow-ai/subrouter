package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Credentials, .claude.json, caches and settings.json remain account-local.
// Settings are merged into sr's private launch overlay, never symlinked.
var claudeUserConfigEntries = []string{
	"projects", "skills", "plugins", "agents", "commands", "hooks",
	"CLAUDE.md", "keybindings.json", "output-styles",
}

// ShareUserConfigDir reconciles a proxy home on every launch. The complete
// original entry is retained beside the new link. Destination conflicts are
// preserved with a legacy suffix; existing shared content always wins.
func (s Store) ShareUserConfigDir(configDir string) (err error) {
	if strings.TrimSpace(s.SharedStateDir) == "" || sameConfigPath(configDir, s.SharedStateDir) {
		return nil
	}
	if err := os.MkdirAll(s.SharedStateDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return err
	}
	if err := validateMigrationSourceParents(configDir); err != nil {
		return err
	}
	sharedLock, err := lockProfileCredential(context.Background(), filepath.Join(s.SharedStateDir, ".subrouter-shared-state-migration"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, sharedLock.Close()) }()
	profileLock, err := lockProfileCredential(context.Background(), configDir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, profileLock.Close()) }()
	source, err := os.OpenRoot(configDir)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := openMigrationDirectoryRoot(s.SharedStateDir, true)
	if err != nil {
		return err
	}
	defer target.Close()
	for _, name := range claudeUserConfigEntries {
		if err := reconcileConfigEntry(source, target, s.SharedStateDir, name); err != nil {
			return fmt.Errorf("share %s: %w", name, err)
		}
	}
	return nil
}

func reconcileConfigEntry(source, target *os.Root, userDir, name string) error {
	destination := filepath.Join(userDir, name)
	info, err := source.Lstat(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		link, err := source.Readlink(name)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(link) {
			link = filepath.Join(source.Name(), link)
		}
		if sameConfigPath(link, destination) {
			return nil
		}
	}
	// Directory targets exist even on a fresh install. File links can be dangling
	// until the user creates the shared file, so edits always reach ~/.claude.
	if name != "CLAUDE.md" && name != "keybindings.json" {
		if err := target.MkdirAll(name, 0o700); err != nil {
			return err
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		return source.Symlink(destination, name)
	}
	backup := name + ".subrouter-backup-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	if _, err := source.Lstat(backup); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return fmt.Errorf("backup already exists: %s", backup)
		}
		return err
	}
	if err := source.Rename(name, backup); err != nil {
		return err
	}
	// Copy from the backup. Never mutate it, including on a failed migration.
	// An old symlink's referent stays untouched; only the old link is backed up.
	if info.Mode()&os.ModeSymlink == 0 {
		if err := mergeConfigEntry(source, target, backup, name); err != nil {
			return errors.Join(err, source.Rename(backup, name))
		}
	}
	if err := source.Symlink(destination, name); err != nil {
		return errors.Join(err, source.Rename(backup, name))
	}
	return nil
}

// mergeConfigEntry copies without following source or destination symlinks.
// Git checkouts are indivisible: interleaving two marketplace repos corrupts
// the checkout even when every file conflict is retained separately.
func mergeConfigEntry(source, target *os.Root, from, to string) error {
	info, err := source.Lstat(from)
	if err != nil {
		return err
	}
	existing, statErr := target.Lstat(to)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if statErr == nil {
		if info.IsDir() && existing.IsDir() && !configGitCheckout(source, from) && !configGitCheckout(target, to) {
			entries, err := readRootDirectory(source, from)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if err := mergeConfigEntry(source, target, filepath.Join(from, entry.Name()), filepath.Join(to, entry.Name())); err != nil {
					return err
				}
			}
			return nil
		}
		if info.Mode().IsRegular() && existing.Mode().IsRegular() {
			equal, err := rootFilesEqual(source, target, from, to, info)
			if err != nil {
				return err
			}
			if equal {
				return nil
			}
		}
		to, err = availableRootPath(target, to)
		if err != nil {
			return err
		}
	}
	return copyConfigEntry(source, target, from, to)
}

func configGitCheckout(root *os.Root, path string) bool {
	_, err := root.Lstat(filepath.Join(path, ".git"))
	return err == nil
}

func copyConfigEntry(source, target *os.Root, from, to string) error {
	info, err := source.Lstat(from)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		link, err := source.Readlink(from)
		if err != nil {
			return err
		}
		return target.Symlink(link, to)
	}
	if info.IsDir() {
		if err := target.Mkdir(to, 0o700); err != nil {
			return err
		}
		entries, err := readRootDirectory(source, from)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyConfigEntry(source, target, filepath.Join(from, entry.Name()), filepath.Join(to, entry.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported config entry %q", from)
	}
	in, err := source.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := target.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	err = errors.Join(copyErr, out.Sync(), out.Close())
	if err != nil {
		return errors.Join(err, target.Remove(to))
	}
	return nil
}

func backupConfigEntryPath(path string) error {
	base := path + ".subrouter-backup-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	backup := base
	for index := 1; ; index++ {
		if _, err := os.Lstat(backup); errors.Is(err, os.ErrNotExist) {
			break
		} else if err != nil {
			return err
		}
		backup = fmt.Sprintf("%s-%d", base, index)
	}
	return copyConfigTreePath(path, backup)
}

func copyConfigTreePath(source, target string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		link, err := os.Readlink(source)
		if err != nil {
			return err
		}
		return os.Symlink(link, target)
	}
	if info.IsDir() {
		if err := os.MkdirAll(target, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyConfigTreePath(filepath.Join(source, entry.Name()), filepath.Join(target, entry.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported config entry %q", source)
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	return errors.Join(copyErr, out.Close())
}
