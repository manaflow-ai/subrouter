package tenant

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateResolveRevoke(t *testing.T) {
	registry := NewRegistry(t.TempDir())
	created, key, err := registry.Create("acme")
	if err != nil {
		t.Fatal(err)
	}
	if !ValidKeyFormat(key) {
		t.Fatalf("key %q is not srt_<32 hex>", key)
	}
	if strings.Contains(mustRead(t, registry.Path()), key) {
		t.Fatal("plaintext key persisted to tenants.json")
	}
	if _, err := os.Stat(filepath.Join(registry.Dir(created.ID), "codex", "accounts")); err != nil {
		t.Fatalf("tenant dir not provisioned: %v", err)
	}

	resolved, ok, err := registry.Resolve(key)
	if err != nil || !ok {
		t.Fatalf("resolve = %v, %v", ok, err)
	}
	if resolved.ID != created.ID {
		t.Fatalf("resolved tenant %q, want %q", resolved.ID, created.ID)
	}

	if _, ok, _ := registry.Resolve("srt_00000000000000000000000000000000"); ok {
		t.Fatal("unknown key resolved")
	}

	revoked, err := registry.RevokeKey(created.ID, created.Keys[0].Prefix)
	if err != nil || revoked != 1 {
		t.Fatalf("revoke = %d, %v", revoked, err)
	}
	if _, ok, _ := registry.Resolve(key); ok {
		t.Fatal("revoked key still resolves")
	}
}

func TestCreateKeyAddsSecondKey(t *testing.T) {
	registry := NewRegistry(t.TempDir())
	created, first, err := registry.Create("acme")
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := registry.CreateKey(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("duplicate key generated")
	}
	for _, key := range []string{first, second} {
		if _, ok, _ := registry.Resolve(key); !ok {
			t.Fatalf("key %q does not resolve", key[:10])
		}
	}
}

func TestDuplicateTenantNameRejected(t *testing.T) {
	registry := NewRegistry(t.TempDir())
	if _, _, err := registry.Create("acme"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.Create("ACME"); err == nil {
		t.Fatal("duplicate name accepted")
	}
}

func TestValidKeyFormat(t *testing.T) {
	valid := "srt_0123456789abcdef0123456789abcdef"
	if !ValidKeyFormat(valid) {
		t.Fatal("valid key rejected")
	}
	for _, bad := range []string{
		"",
		"srt_",
		"srt_0123456789ABCDEF0123456789ABCDEF",
		"srt_0123456789abcdef0123456789abcde",
		"srt_0123456789abcdef0123456789abcdef0",
		"sk-0123456789abcdef0123456789abcdef",
		"subrouter",
	} {
		if ValidKeyFormat(bad) {
			t.Fatalf("invalid key %q accepted", bad)
		}
	}
}

func TestEnsureExternalIsStableAndUpdatesName(t *testing.T) {
	registry := NewRegistry(t.TempDir())
	secret := []byte("0123456789abcdef0123456789abcdef")
	key, err := DeriveKey(secret, "stack-project", "team-123")
	if err != nil {
		t.Fatal(err)
	}
	first, err := registry.EnsureExternal("team-123", "Old name", key)
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.EnsureExternal("team-123", "New name", key)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "team-123" || second.Name != "New name" {
		t.Fatalf("tenants = %#v %#v", first, second)
	}
	if len(second.Keys) != 1 {
		t.Fatalf("repeated login accumulated %d keys", len(second.Keys))
	}
	if resolved, ok, err := registry.Resolve(key); err != nil || !ok || resolved.ID != "team-123" {
		t.Fatalf("resolve = %#v, %v, %v", resolved, ok, err)
	}
}

func TestExternalTenantRejectsTraversalAndWeakSecret(t *testing.T) {
	if _, err := DeriveKey([]byte("short"), "stack", "team"); err == nil {
		t.Fatal("weak secret accepted")
	}
	for _, id := range []string{"", ".", "..", "../team", "team/other", "team other", "Team-A"} {
		if ValidExternalID(id) {
			t.Fatalf("invalid external ID %q accepted", id)
		}
	}
	first, err := DeriveKey([]byte("0123456789abcdef0123456789abcdef"), "stack-a", "team-a")
	if err != nil {
		t.Fatal(err)
	}
	again, err := DeriveKey([]byte("0123456789abcdef0123456789abcdef"), "stack-a", "team-a")
	if err != nil {
		t.Fatal(err)
	}
	otherNamespace, err := DeriveKey([]byte("0123456789abcdef0123456789abcdef"), "stack-b", "team-a")
	if err != nil {
		t.Fatal(err)
	}
	if first != again || first == otherNamespace {
		t.Fatalf("derived keys = %q, %q, %q", first, again, otherNamespace)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
