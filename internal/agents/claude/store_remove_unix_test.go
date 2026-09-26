//go:build !windows

package claude

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveProfileRestoresStagedCredentialWhenRegistryWriteFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory permissions used to force the registry failure")
	}
	store := Store{Dir: filepath.Join(t.TempDir(), "store")}
	instancePath, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ImportProfileCredential("work", CredentialInfo{
		AccessToken:  "access",
		RefreshToken: "refresh",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(store.Dir, 0o500); err != nil {
		t.Fatal(err)
	}
	removed, removeErr := store.RemoveProfile("work")
	if err := os.Chmod(store.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if removeErr == nil {
		t.Fatal("profile removal unexpectedly persisted through an unwritable registry directory")
	}
	if removed {
		t.Fatal("failed profile removal was reported as committed")
	}
	if _, ok := store.FindProfile("work"); !ok {
		t.Fatal("failed profile removal changed the registry")
	}
	credential, err := store.ReadCredential(context.Background(), instancePath)
	if err != nil {
		t.Fatal(err)
	}
	if credential == nil || credential.AccessToken != "access" || credential.RefreshToken != "refresh" {
		t.Fatalf("failed profile removal did not restore the credential: %+v", credential)
	}
}
