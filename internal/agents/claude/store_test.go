package claude

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreCreateSetRemoveProfile(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	instancePath, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"session-env", "todos", "logs", "file-history", "shell-snapshots", "debug", ".anthropic"} {
		if _, err := os.Stat(filepath.Join(instancePath, dir)); err != nil {
			t.Fatalf("missing instance dir %s: %v", dir, err)
		}
	}
	if active := store.ActiveProfile(); active != "work" {
		t.Fatalf("active = %q, want work", active)
	}
	if err := store.SetActiveProfile("work"); err != nil {
		t.Fatal(err)
	}
	removed, err := store.RemoveProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("profile was not removed")
	}
	if profiles := store.ListProfiles(); len(profiles) != 0 {
		t.Fatalf("profiles = %d, want 0", len(profiles))
	}
}

func TestRegisterProfileAllowsEmailName(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	_, dir, err := store.CreateTempInstance()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterProfile("person@example.com", dir); err != nil {
		t.Fatal(err)
	}
	profile, ok := store.FindProfile("person@example.com")
	if !ok {
		t.Fatal("profile not found")
	}
	if profile.Dir != dir {
		t.Fatalf("dir = %q, want %q", profile.Dir, dir)
	}
}

func TestClaudeConfigDirPrefersCodexAccountsAlias(t *testing.T) {
	home := t.TempDir()
	store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(store.Dir, filepath.Join(home, ".codex-accounts")); err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(home, ".codex-accounts", "claude", "work")
	if got := store.ClaudeConfigDir("work"); got != want {
		t.Fatalf("ClaudeConfigDir = %q, want %q", got, want)
	}
}

func TestClaudeConfigDirFallsBackWhenAliasMissing(t *testing.T) {
	home := t.TempDir()
	store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	instancePath, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}

	if got := store.ClaudeConfigDir("work"); got != filepath.Clean(instancePath) {
		t.Fatalf("ClaudeConfigDir = %q, want %q", got, filepath.Clean(instancePath))
	}
}

func TestReadCredentialFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"tok","subscriptionType":"pro"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	credential, ok := readCredentialFile(dir)
	if !ok {
		t.Fatal("credential not read")
	}
	if credential.AccessToken != "tok" {
		t.Fatalf("access token = %q, want tok", credential.AccessToken)
	}
}
