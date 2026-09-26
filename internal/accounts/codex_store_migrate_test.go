package accounts

import (
	"os"
	"path/filepath"
	"testing"
)

// A credential handed to the team vault must stop being refreshed here: the
// provider rotates refresh tokens on use, so two refreshers invalidate each
// other. Moving the file out of the scanned store is what disowns it, while
// keeping it readable for rollback.
func TestMigrateStoredAwayDisownsButKeepsRecord(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}
	account := StoredCodexAccount{
		Email: "lc+8@cmux.com",
		Auth: CodexAuthFile{Tokens: &CodexTokens{
			AccessToken:  "access",
			RefreshToken: "refresh",
			IDToken:      "id",
		}},
	}
	if err := store.SaveStored(account); err != nil {
		t.Fatal(err)
	}
	if stored, err := store.ListStored(); err != nil || len(stored) != 1 {
		t.Fatalf("precondition: stored=%d err=%v", len(stored), err)
	}

	path, ok, err := store.MigrateStoredAway("lc+8@cmux.com")
	if err != nil || !ok {
		t.Fatalf("migrate: ok=%v err=%v", ok, err)
	}

	stored, err := store.ListStored()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Fatalf("account still in the active store after handover: %+v", stored)
	}
	if filepath.Dir(path) != filepath.Join(store.Dir, MigratedDirName) {
		t.Fatalf("record landed at %s, want it under %s", path, MigratedDirName)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("rollback record unreadable: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("rollback record is empty")
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("record directory mode %o, want 700", perm)
	}
}

func TestMigrateStoredAwayReportsMissingAccount(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}
	if _, ok, err := store.MigrateStoredAway("nobody@example.com"); ok || err != nil {
		t.Fatalf("ok=%v err=%v, want false and no error", ok, err)
	}
}
