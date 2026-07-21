package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// sharedStateDirNames are the per-profile Claude Code state dirs that hold
// conversation state keyed by session UUID (or referenced from transcripts):
// transcripts, per-session env, file history, todos, shell snapshots, task
// lists, and plans. Sharing them across profiles makes every session
// resumable from every profile; only credentials and account identity (the
// keychain entry and .claude.json) stay per-profile.
var sharedStateDirNames = []string{
	"projects",
	"session-env",
	"file-history",
	"todos",
	"shell-snapshots",
	"tasks",
	"plans",
}

const (
	sharedDirName       = "_shared"
	sharedConflictsName = ".conflicts"
)

// SharedStateReport summarizes a consolidation or layout-repair pass.
type SharedStateReport struct {
	Profiles  int
	Moved     int
	Conflicts []string
}

func (r *SharedStateReport) merge(other SharedStateReport) {
	r.Profiles += other.Profiles
	r.Moved += other.Moved
	r.Conflicts = append(r.Conflicts, other.Conflicts...)
}

// SharedDir returns the shared session store directory, a sibling of the
// profile config dirs. It lives under the same root the profile config dirs
// resolve to (the legacy ~/.codex-accounts location when that is still the
// preferred home of the instance dirs), so `ls` next to the profiles shows it.
func (s Store) SharedDir() string {
	return filepath.Join(s.preferredInstancesRoot(), sharedDirName)
}

// preferredInstancesRoot mirrors PreferredInstancePath for the instances root
// itself: when the store lives in ~/.subrouter/codex but the legacy
// ~/.codex-accounts tree is still the preferred home of instance dirs, the
// shared store must live there too, next to the dirs it is linked from.
func (s Store) preferredInstancesRoot() string {
	instances := s.InstancesDir()
	cleanDir := filepath.Clean(s.Dir)
	if filepath.Base(cleanDir) != "codex" || filepath.Base(filepath.Dir(cleanDir)) != ".subrouter" {
		return instances
	}
	home := filepath.Dir(filepath.Dir(cleanDir))
	candidate := filepath.Join(home, ".codex-accounts", "claude")
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return candidate
	}
	return instances
}

// SharedStoreEnabled reports whether the shared session store has been
// created (via `sr claude consolidate`). Until then profiles keep fully
// isolated state and nothing changes.
func (s Store) SharedStoreEnabled() bool {
	info, err := os.Stat(s.SharedDir())
	return err == nil && info.IsDir()
}

// ConsolidateSharedState creates the shared session store and converges every
// instance dir onto it: per-instance session state moves (renames, same
// volume) into the shared store and each instance dir gets symlinks in its
// place. Besides registered profiles this sweeps every other dir in the
// instances root (temp instances from `sr claude add`, dirs orphaned by
// removed profiles), since those hold resumable sessions too. Re-running is a
// cheap no-op for already-converged dirs. When the same file exists in two
// instances with different content, the newer copy wins and the older one is
// quarantined under _shared/.conflicts/<instance-dir>/ rather than deleted.
func (s Store) ConsolidateSharedState() (SharedStateReport, error) {
	var report SharedStateReport
	if err := os.MkdirAll(s.SharedDir(), 0o700); err != nil {
		return report, err
	}
	seen := map[string]bool{}
	for _, profile := range s.ListProfiles() {
		configDir := filepath.Clean(s.ClaudeConfigDir(profile.Name))
		profileReport, err := s.EnsureSharedLayout(configDir)
		if err != nil {
			return report, fmt.Errorf("profile %q: %w", profile.Name, err)
		}
		seen[configDir] = true
		report.merge(profileReport)
		report.Profiles++
	}
	for _, root := range []string{s.preferredInstancesRoot(), s.InstancesDir()} {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || entry.Name() == sharedDirName || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			configDir := filepath.Clean(filepath.Join(root, entry.Name()))
			if seen[configDir] {
				continue
			}
			instanceReport, err := s.EnsureSharedLayout(configDir)
			if err != nil {
				return report, fmt.Errorf("instance %s: %w", configDir, err)
			}
			seen[configDir] = true
			report.merge(instanceReport)
			report.Profiles++
		}
	}
	return report, nil
}

// EnsureSharedLayout converges one profile config dir onto the shared store:
// for each shared state dir, a missing entry becomes a symlink, an existing
// real dir is merged into the shared store (then replaced by a symlink), and
// an existing symlink is left alone. Safe to call on every launch; when the
// profile is already converged this is a handful of lstats.
func (s Store) EnsureSharedLayout(configDir string) (SharedStateReport, error) {
	var report SharedStateReport
	sharedDir := s.SharedDir()
	cleanConfig := filepath.Clean(configDir)
	if cleanConfig == filepath.Clean(sharedDir) || filepath.Base(cleanConfig) == sharedDirName {
		return report, fmt.Errorf("refusing to converge the shared store onto itself: %s", configDir)
	}
	if err := os.MkdirAll(cleanConfig, 0o700); err != nil {
		return report, err
	}
	for _, name := range sharedStateDirNames {
		target := filepath.Join(sharedDir, name)
		if err := os.MkdirAll(target, 0o700); err != nil {
			return report, err
		}
		link := filepath.Join(configDir, name)
		info, err := os.Lstat(link)
		if os.IsNotExist(err) {
			if err := symlinkIdempotent(target, link); err != nil {
				return report, err
			}
			continue
		}
		if err != nil {
			return report, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if !info.IsDir() {
			return report, fmt.Errorf("%s exists and is not a directory", link)
		}
		conflictDir := filepath.Join(sharedDir, sharedConflictsName, filepath.Base(cleanConfig), name)
		moved, conflicts, err := mergeMoveDir(link, target, conflictDir)
		report.Moved += moved
		report.Conflicts = append(report.Conflicts, conflicts...)
		if err != nil {
			return report, err
		}
		if err := os.Remove(link); err != nil {
			return report, fmt.Errorf("replace %s with symlink: %w", link, err)
		}
		if err := symlinkIdempotent(target, link); err != nil {
			return report, err
		}
	}
	return report, nil
}

// symlinkIdempotent creates link -> target, tolerating a concurrent process
// having already created an equivalent symlink.
func symlinkIdempotent(target, link string) error {
	err := os.Symlink(target, link)
	if err == nil || !os.IsExist(err) {
		return err
	}
	info, lerr := os.Lstat(link)
	if lerr == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	return err
}

// mergeMoveDir moves the contents of src into dst, merging directories.
// Files move by rename (no data copies). When dst already has an entry with
// the same name and either side is not a directory, the entry with the newer
// mtime ends up in dst and the older one moves under conflictDir. Returns the
// number of renamed entries and the quarantined conflict paths.
func mergeMoveDir(src, dst, conflictDir string) (int, []string, error) {
	entries, err := os.ReadDir(src)
	if err != nil {
		return 0, nil, err
	}
	moved := 0
	var conflicts []string
	for _, entry := range entries {
		name := entry.Name()
		srcPath := filepath.Join(src, name)
		dstPath := filepath.Join(dst, name)
		dstInfo, err := os.Lstat(dstPath)
		if os.IsNotExist(err) {
			if renameErr := os.Rename(srcPath, dstPath); renameErr == nil {
				moved++
				continue
			}
			// A concurrent converge may have landed the entry between the
			// lstat and the rename; re-check before treating it as fatal.
			dstInfo, err = os.Lstat(dstPath)
		}
		if err != nil {
			return moved, conflicts, err
		}
		srcInfo, err := os.Lstat(srcPath)
		if err != nil {
			return moved, conflicts, err
		}
		if srcInfo.IsDir() && dstInfo.IsDir() {
			subMoved, subConflicts, err := mergeMoveDir(srcPath, dstPath, filepath.Join(conflictDir, name))
			moved += subMoved
			conflicts = append(conflicts, subConflicts...)
			if err != nil {
				return moved, conflicts, err
			}
			if err := os.Remove(srcPath); err != nil {
				return moved, conflicts, fmt.Errorf("remove merged dir %s: %w", srcPath, err)
			}
			continue
		}
		if srcInfo.ModTime().After(dstInfo.ModTime()) {
			quarantined, err := quarantine(dstPath, filepath.Join(conflictDir, name))
			if err != nil {
				return moved, conflicts, err
			}
			if err := os.Rename(srcPath, dstPath); err != nil {
				return moved, conflicts, err
			}
			moved++
			conflicts = append(conflicts, quarantined)
		} else {
			quarantined, err := quarantine(srcPath, filepath.Join(conflictDir, name))
			if err != nil {
				return moved, conflicts, err
			}
			conflicts = append(conflicts, quarantined)
		}
	}
	return moved, conflicts, nil
}

// quarantine moves path under the conflicts dir, suffixing the name when a
// previous run already quarantined an entry with the same name. Returns the
// final quarantined path.
func quarantine(path, dest string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return "", err
	}
	candidate := dest
	for i := 1; ; i++ {
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			break
		}
		candidate = fmt.Sprintf("%s.%d", dest, i)
	}
	return candidate, os.Rename(path, candidate)
}

// FormatSharedStateReport renders a one-line human summary.
func FormatSharedStateReport(report SharedStateReport) string {
	line := fmt.Sprintf("Converged %d profile(s), moved %d entr(ies) into the shared store.", report.Profiles, report.Moved)
	if len(report.Conflicts) > 0 {
		line += fmt.Sprintf(" %d conflict(s) quarantined under %s.", len(report.Conflicts), filepath.Join(sharedDirName, sharedConflictsName))
	}
	return line
}
