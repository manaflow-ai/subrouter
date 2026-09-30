//go:build !windows

package claude

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func shareUserConfigFixture(t *testing.T) (Store, string, string) {
	t.Helper()
	root := t.TempDir()
	user := filepath.Join(root, "user")
	proxy := filepath.Join(root, "proxy")
	for _, dir := range []string{filepath.Join(user, "skills", "ci-wait"), filepath.Join(user, "agents"), proxy} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		"settings.json": `{"apiKeyHelper":"leak","hooks":{}}`,
		"CLAUDE.md":     "user memory\n",
	} {
		if err := os.WriteFile(filepath.Join(user, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return Store{Dir: filepath.Join(root, "store"), SharedStateDir: user}, user, proxy
}

func assertLinked(t *testing.T, link, target string) {
	t.Helper()
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("%s is not a symlink: %v", link, err)
	}
	if got != target {
		t.Fatalf("%s -> %s, want %s", link, got, target)
	}
}

func TestShareUserConfigDirLinksSkillsButNotSettingsOrCredentials(t *testing.T) {
	store, user, proxy := shareUserConfigFixture(t)
	if err := os.WriteFile(filepath.Join(proxy, ".credentials.json"), []byte("proxy-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.ShareUserConfigDir(proxy); err != nil {
		t.Fatal(err)
	}
	assertLinked(t, filepath.Join(proxy, "skills"), filepath.Join(user, "skills"))
	assertLinked(t, filepath.Join(proxy, "agents"), filepath.Join(user, "agents"))
	assertLinked(t, filepath.Join(proxy, "CLAUDE.md"), filepath.Join(user, "CLAUDE.md"))
	// settings.json reaches proxy launches only through the filtered overlay.
	if _, err := os.Lstat(filepath.Join(proxy, "settings.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("settings.json linked or created: %v", err)
	}
	for _, name := range claudeUserConfigEntries {
		assertLinked(t, filepath.Join(proxy, name), filepath.Join(user, name))
	}
	if body, _ := os.ReadFile(filepath.Join(proxy, ".credentials.json")); string(body) != "proxy-secret" {
		t.Fatalf("credential changed: %q", body)
	}
	if _, err := os.Stat(filepath.Join(user, ".credentials.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential leaked into the user home: %v", err)
	}
}

func TestShareUserConfigDirMigratesDirectoriesAndFileConflicts(t *testing.T) {
	store, user, proxy := shareUserConfigFixture(t)
	if err := os.MkdirAll(filepath.Join(proxy, "skills"), 0o700); err != nil { // empty first-run dir
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(proxy, "agents", "own"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proxy, "CLAUDE.md"), []byte("proxy memory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.ShareUserConfigDir(proxy); err != nil {
		t.Fatal(err)
	}
	assertLinked(t, filepath.Join(proxy, "skills"), filepath.Join(user, "skills"))
	if matches, _ := filepath.Glob(filepath.Join(proxy, "skills.subrouter-backup-*")); len(matches) != 1 {
		t.Fatalf("empty skills backup count = %d, want 1", len(matches))
	}
	assertLinked(t, filepath.Join(proxy, "agents"), filepath.Join(user, "agents"))
	if matches, _ := filepath.Glob(filepath.Join(proxy, "agents.subrouter-backup-*")); len(matches) != 1 {
		t.Fatalf("populated agents backup count = %d, want 1", len(matches))
	}
	assertLinked(t, filepath.Join(proxy, "CLAUDE.md"), filepath.Join(user, "CLAUDE.md"))
	if matches, _ := filepath.Glob(filepath.Join(proxy, "CLAUDE.md.subrouter-backup-*")); len(matches) != 1 {
		t.Fatalf("CLAUDE.md backup count = %d, want 1", len(matches))
	}
}

func TestShareUserConfigDirIsIdempotent(t *testing.T) {
	store, user, proxy := shareUserConfigFixture(t)
	for i := 0; i < 2; i++ {
		if err := store.ShareUserConfigDir(proxy); err != nil {
			t.Fatal(err)
		}
	}
	assertLinked(t, filepath.Join(proxy, "agents"), filepath.Join(user, "agents"))
	assertLinked(t, filepath.Join(proxy, "skills"), filepath.Join(user, "skills"))
}

func TestShareUserConfigDirMigratesARealPluginsDirectoryWithBackup(t *testing.T) {
	store, user, proxy := shareUserConfigFixture(t)
	if err := os.MkdirAll(filepath.Join(user, "plugins", "market"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(user, "plugins", "market", "shared.txt"), []byte("shared"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(proxy, "plugins", "local"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proxy, "plugins", "local", "account.txt"), []byte("account"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.ShareUserConfigDir(proxy); err != nil {
		t.Fatal(err)
	}
	assertLinked(t, filepath.Join(proxy, "plugins"), filepath.Join(user, "plugins"))
	if _, err := os.Stat(filepath.Join(user, "plugins", "market", "shared.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(user, "plugins", "local", "account.txt")); err != nil {
		t.Fatal(err)
	}
	if matches, _ := filepath.Glob(filepath.Join(proxy, "plugins.subrouter-backup-*")); len(matches) != 1 {
		t.Fatalf("plugins backup count = %d, want 1", len(matches))
	}
}

func TestShareUserConfigDirWithoutSharedStateIsNoop(t *testing.T) {
	proxy := t.TempDir()
	if err := (Store{Dir: t.TempDir()}).ShareUserConfigDir(proxy); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(proxy)
	if len(entries) != 0 {
		t.Fatalf("created %d entries without a shared state dir", len(entries))
	}
}

func TestShareUserConfigDirFreshAndMissingSharedEntries(t *testing.T) {
	root := t.TempDir()
	store := Store{SharedStateDir: filepath.Join(root, "user")}
	proxy := filepath.Join(root, "proxy")
	if err := store.ShareUserConfigDir(proxy); err != nil {
		t.Fatal(err)
	}
	for _, name := range claudeUserConfigEntries {
		assertLinked(t, filepath.Join(proxy, name), filepath.Join(store.SharedStateDir, name))
	}
	if err := os.WriteFile(filepath.Join(proxy, "CLAUDE.md"), []byte("shared now"), 0o600); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(store.SharedStateDir, "CLAUDE.md")); err != nil || string(body) != "shared now" {
		t.Fatalf("file creation = %q, %v", body, err)
	}
}

func TestShareUserConfigDirPreservesPluginCheckoutsAndIsIdempotent(t *testing.T) {
	store, user, proxy := shareUserConfigFixture(t)
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(user, "plugins", "marketplaces", "official", ".git", "HEAD"), "shared-head")
	write(filepath.Join(proxy, "plugins", "marketplaces", "official", ".git", "HEAD"), "local-head")
	write(filepath.Join(proxy, "plugins", "marketplaces", "official", "unique"), "account-only")
	write(filepath.Join(proxy, "hooks", "exec.sh"), "#!/bin/sh\n")
	write(filepath.Join(proxy, "CLAUDE.md"), "account instructions")
	write(filepath.Join(proxy, ".credentials.json"), "private")
	for i := 0; i < 2; i++ {
		if err := store.ShareUserConfigDir(proxy); err != nil {
			t.Fatal(err)
		}
	}
	for path, want := range map[string]string{
		filepath.Join(user, "plugins", "marketplaces", "official", ".git", "HEAD"):              "shared-head",
		filepath.Join(user, "plugins", "marketplaces", "official.subrouter-legacy-1", "unique"): "account-only",
		filepath.Join(user, "CLAUDE.md.subrouter-legacy-1"):                                     "account instructions",
		filepath.Join(proxy, ".credentials.json"):                                               "private",
	} {
		body, err := os.ReadFile(path)
		if err != nil || string(body) != want {
			t.Fatalf("%s = %q, %v", path, body, err)
		}
	}
	if _, err := os.Stat(filepath.Join(user, "plugins", "marketplaces", "official", "unique")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("mixed marketplace checkouts")
	}
	backups, err := filepath.Glob(filepath.Join(proxy, "plugins.subrouter-backup-*"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups: %v, %v", backups, err)
	}
	if body, err := os.ReadFile(filepath.Join(backups[0], "marketplaces", "official", "unique")); err != nil || string(body) != "account-only" {
		t.Fatalf("backup changed: %q, %v", body, err)
	}
	if info, err := os.Stat(filepath.Join(user, "hooks", "exec.sh")); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatal("lost executable hook mode")
	}
}

func TestShareUserConfigDirReplacesWrongLinksAndKeepsReferents(t *testing.T) {
	store, user, proxy := shareUserConfigFixture(t)
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "keep"), []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(proxy, "agents")); err != nil {
		t.Fatal(err)
	}
	if err := store.ShareUserConfigDir(proxy); err != nil {
		t.Fatal(err)
	}
	assertLinked(t, filepath.Join(proxy, "agents"), filepath.Join(user, "agents"))
	backups, _ := filepath.Glob(filepath.Join(proxy, "agents.subrouter-backup-*"))
	if len(backups) != 1 {
		t.Fatal(backups)
	}
	assertLinked(t, backups[0], other)
	if body, err := os.ReadFile(filepath.Join(other, "keep")); err != nil || string(body) != "untouched" {
		t.Fatal("changed old link referent")
	}
}

func TestShareUserConfigDirDoesNotFollowNestedDestinationSymlink(t *testing.T) {
	store, user, proxy := shareUserConfigFixture(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(user, "agents", "nested")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(proxy, "agents", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proxy, "agents", "nested", "keep"), []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.ShareUserConfigDir(proxy); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("wrote through destination symlink")
	}
	if _, err := os.Stat(filepath.Join(user, "agents", "nested.subrouter-legacy-1", "keep")); err != nil {
		t.Fatal(err)
	}
}
