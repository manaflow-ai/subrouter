package claude

import (
	"context"
	"path/filepath"
	"testing"
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
