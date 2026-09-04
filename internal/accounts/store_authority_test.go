package accounts

import (
	"crypto/hmac"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreAuthorityIDNormalizesTheSamePath(t *testing.T) {
	root := t.TempDir()
	left, err := StoreAuthorityID(filepath.Join(root, "codex", "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	right, err := StoreAuthorityID(filepath.Join(root, "codex") + string(filepath.Separator) + "." + string(filepath.Separator) + "accounts")
	if err != nil {
		t.Fatal(err)
	}
	if left == "" || left != right {
		t.Fatalf("authority IDs differ: %q != %q", left, right)
	}
	other, err := StoreAuthorityID(filepath.Join(root, "other", "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	if other == left {
		t.Fatal("different account stores have the same authority ID")
	}
}

func TestStoreAuthorityProofIsSharedOnlyByTheSameStore(t *testing.T) {
	challenge := hex.EncodeToString(make([]byte, 32))
	store := filepath.Join(t.TempDir(), "codex", "accounts")
	first, err := StoreAuthorityProof(store, challenge)
	if err != nil {
		t.Fatal(err)
	}
	second, err := StoreAuthorityProof(filepath.Dir(store)+string(filepath.Separator)+"."+string(filepath.Separator)+"accounts", challenge)
	if err != nil {
		t.Fatal(err)
	}
	if !hmac.Equal([]byte(first), []byte(second)) {
		t.Fatal("the same account store produced different proofs")
	}
	other, err := StoreAuthorityProof(filepath.Join(t.TempDir(), "codex", "accounts"), challenge)
	if err != nil {
		t.Fatal(err)
	}
	if hmac.Equal([]byte(first), []byte(other)) {
		t.Fatal("different account stores produced the same proof")
	}
}

func TestExistingStoreAuthorityProofDoesNotCreateAuthorityMaterial(t *testing.T) {
	store := filepath.Join(t.TempDir(), "codex", "accounts")
	challenge := hex.EncodeToString(make([]byte, 32))
	if _, err := ExistingStoreAuthorityProof(store, challenge); err == nil {
		t.Fatal("missing existing authority key unexpectedly produced a proof")
	}
	if _, err := os.Stat(filepath.Dir(store)); !os.IsNotExist(err) {
		t.Fatalf("client-side proof created authority material: %v", err)
	}
}

func TestStoreAuthorityUsesResolvedSymlinkedStateRoot(t *testing.T) {
	root := t.TempDir()
	realState := filepath.Join(root, "real-state")
	if err := os.MkdirAll(filepath.Join(realState, "codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "state-alias")
	if err := os.Symlink(realState, alias); err != nil {
		t.Skipf("symlinked state-root test unavailable: %v", err)
	}
	realStore := filepath.Join(realState, "codex", "accounts")
	aliasStore := filepath.Join(alias, "codex", "accounts")

	realID, err := StoreAuthorityID(realStore)
	if err != nil {
		t.Fatal(err)
	}
	aliasID, err := StoreAuthorityID(aliasStore)
	if err != nil {
		t.Fatal(err)
	}
	if realID != aliasID {
		t.Fatalf("symlinked store authority IDs differ: %q != %q", realID, aliasID)
	}
	challenge := hex.EncodeToString(make([]byte, 32))
	realProof, err := StoreAuthorityProof(realStore, challenge)
	if err != nil {
		t.Fatal(err)
	}
	aliasProof, err := StoreAuthorityProof(aliasStore, challenge)
	if err != nil {
		t.Fatal(err)
	}
	if !hmac.Equal([]byte(realProof), []byte(aliasProof)) {
		t.Fatal("symlinked state roots produced different store proofs")
	}
}

func TestStoreAuthorityUsesResolvedSymlinkedStoreDirectory(t *testing.T) {
	root := t.TempDir()
	realStore := filepath.Join(root, "real", "accounts")
	if err := os.MkdirAll(realStore, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasStore := filepath.Join(root, "accounts-alias")
	if err := os.Symlink(realStore, aliasStore); err != nil {
		t.Skipf("symlinked account-store test unavailable: %v", err)
	}

	realID, err := StoreAuthorityID(realStore)
	if err != nil {
		t.Fatal(err)
	}
	aliasID, err := StoreAuthorityID(aliasStore)
	if err != nil {
		t.Fatal(err)
	}
	if realID != aliasID {
		t.Fatalf("symlinked store authority IDs differ: %q != %q", realID, aliasID)
	}
	challenge := hex.EncodeToString(make([]byte, 32))
	realProof, err := StoreAuthorityProof(realStore, challenge)
	if err != nil {
		t.Fatal(err)
	}
	aliasProof, err := StoreAuthorityProof(aliasStore, challenge)
	if err != nil {
		t.Fatal(err)
	}
	if !hmac.Equal([]byte(realProof), []byte(aliasProof)) {
		t.Fatal("symlinked account-store directories produced different proofs")
	}
}

func TestStoreAuthorityProofRejectsMalformedChallenge(t *testing.T) {
	for _, challenge := range []string{"short", hex.EncodeToString(make([]byte, 31))} {
		if _, err := StoreAuthorityProof(filepath.Join(t.TempDir(), "accounts"), challenge); err == nil {
			t.Fatalf("malformed account-store challenge %q unexpectedly succeeded", challenge)
		}
	}
}

func TestStoreAuthorityIDRejectsEmptyPath(t *testing.T) {
	if _, err := StoreAuthorityID(""); err == nil {
		t.Fatal("empty account store unexpectedly received an authority ID")
	}
}
