package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUpsertCredentialProfileUsesFileBackedServerState(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	credential := CredentialInfo{
		AccessToken: "access", RefreshToken: "refresh",
		SubscriptionType: "max", ExpiresAt: 4_102_444_800_000,
	}
	profile, err := store.UpsertCredentialProfile("user@example.com", credential)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Name != "user@example.com" {
		t.Fatalf("profile = %#v", profile)
	}
	read, err := store.ReadCredential(context.Background(), filepath.Join(store.InstancesDir(), profile.Dir))
	if err != nil {
		t.Fatal(err)
	}
	if read == nil || read.RefreshToken != "refresh" {
		t.Fatalf("credential = %#v", read)
	}
}

func TestUpsertCredentialProfileKeepsSanitizedNameCollisionsIsolated(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	firstName := "first.last@example.com"
	secondName := "first+last@example.com"
	if sanitizeName(firstName) != sanitizeName(secondName) {
		t.Fatal("test inputs no longer reproduce the legacy directory collision")
	}
	for name, access := range map[string]string{
		firstName:  "first-access",
		secondName: "second-access",
	} {
		if _, err := store.UpsertCredentialProfile(name, CredentialInfo{
			AccessToken: access, RefreshToken: "refresh-" + access,
		}); err != nil {
			t.Fatal(err)
		}
	}

	first, firstOK := store.FindProfile(firstName)
	second, secondOK := store.FindProfile(secondName)
	if !firstOK || !secondOK {
		t.Fatalf("profiles missing: first=%v second=%v", firstOK, secondOK)
	}
	if first.Dir == second.Dir {
		t.Fatalf("distinct labels share credential directory %q", first.Dir)
	}
	if removed, err := store.RemoveProfile(firstName); err != nil || !removed {
		t.Fatalf("remove first profile: removed=%v err=%v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(store.InstancesDir(), first.Dir)); !os.IsNotExist(err) {
		t.Fatalf("removed profile directory still exists: %v", err)
	}
	credential, err := store.ReadCredential(t.Context(), filepath.Join(store.InstancesDir(), second.Dir))
	if err != nil {
		t.Fatal(err)
	}
	if credential == nil || credential.AccessToken != "second-access" {
		t.Fatalf("removing colliding label affected survivor: %#v", credential)
	}
}

func TestUpsertCredentialProfilePreservesExistingDirectoryWithoutOrphan(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	const name = "existing.profile@example.com"
	const existingDir = "stable-existing-directory"
	if err := store.writeProfiles(profilesFile{Profiles: map[string]Profile{
		name: {Name: name, CreatedAt: "2026-08-28T00:00:00Z", Dir: existingDir},
	}}); err != nil {
		t.Fatal(err)
	}
	profile, err := store.UpsertCredentialProfile(name, CredentialInfo{
		AccessToken: "new-access", RefreshToken: "new-refresh",
	})
	if err != nil {
		t.Fatal(err)
	}
	if profile.Dir != existingDir {
		t.Fatalf("profile directory = %q, want %q", profile.Dir, existingDir)
	}
	entries, err := os.ReadDir(store.InstancesDir())
	if err != nil {
		t.Fatal(err)
	}
	directories := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			directories = append(directories, entry.Name())
		}
	}
	if len(directories) != 1 || directories[0] != existingDir {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("profile directories = %v, want only existing directory", names)
	}
	if orphan := importedProfileDir(name); orphan == existingDir {
		t.Fatal("test existing directory unexpectedly equals derived directory")
	} else if _, err := os.Stat(filepath.Join(store.InstancesDir(), orphan)); !os.IsNotExist(err) {
		t.Fatalf("derived orphan directory exists: %v", err)
	}
}

func TestUpsertCredentialProfileWaitsForCredentialWriter(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	profile, err := store.UpsertCredentialProfile("work@example.com", CredentialInfo{
		AccessToken: "old-access", RefreshToken: "old-refresh",
	})
	if err != nil {
		t.Fatal(err)
	}
	instancePath := filepath.Join(store.InstancesDir(), profile.Dir)
	writerLock, err := lockProfileCredential(context.Background(), instancePath)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := store.UpsertCredentialProfile("work@example.com", CredentialInfo{
			AccessToken: "uploaded-access", RefreshToken: "uploaded-refresh",
		})
		done <- err
	}()
	select {
	case err := <-done:
		_ = writerLock.Close()
		t.Fatalf("upsert bypassed active credential writer lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := writerLock.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upsert remained blocked after credential writer released its lock")
	}
	credential, err := store.ReadCredential(t.Context(), instancePath)
	if err != nil {
		t.Fatal(err)
	}
	if credential == nil || !strings.HasPrefix(credential.AccessToken, "uploaded-") {
		t.Fatalf("credential = %#v, want uploaded credential", credential)
	}
}
