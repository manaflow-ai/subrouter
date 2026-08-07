package accounts

import (
	"context"
	"testing"
	"time"
)

func TestFirstServeStampsUnclaimedOwner(t *testing.T) {
	t.Setenv("SUBROUTER_HOST_ID", "first-serve-host")
	ResetLocalHostIDForTest()
	defer ResetLocalHostIDForTest()

	store := CodexStore{Dir: t.TempDir()}
	// Write a legacy file with no owner claim and a still-fresh access token.
	writeStoredCodexAccountFile(t, store, StoredCodexAccount{
		Email: "legacy@example.com",
		Auth: CodexAuthFile{Tokens: &CodexTokens{
			AccessToken:  testJWT("legacy@example.com", time.Now().Add(time.Hour)),
			RefreshToken: "unused-refresh",
			IDToken:      testJWT("legacy@example.com", time.Now().Add(time.Hour)),
		}},
	})

	account, ok, err := store.FindStored("legacy@example.com")
	if err != nil || !ok {
		t.Fatal("seed missing")
	}
	got, refreshed, err := store.RefreshStoredIfExpired(context.Background(), nil, account)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed {
		t.Fatal("fresh token must not hit the provider")
	}
	if got.Owner == nil || got.Owner.Host != "first-serve-host" || got.Owner.Epoch != 1 {
		t.Fatalf("first serve did not stamp owner: %+v", got.Owner)
	}
	stored, ok, err := store.FindStored("legacy@example.com")
	if err != nil || !ok || stored.Owner == nil || stored.Owner.Host != "first-serve-host" {
		t.Fatalf("claim was not persisted: ok=%v owner=%+v err=%v", ok, stored.Owner, err)
	}
}
