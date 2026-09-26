package main

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShadowHealthKeyLoadsValid32ByteValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shadow-health-key")
	want := []byte("0123456789abcdef0123456789abcdef")
	if err := os.WriteFile(path, []byte(hex.EncodeToString(want)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_SHADOW_HEALTH_KEY_FILE", path)
	got, err := shadowHealthKeyFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("key mismatch")
	}
}

func TestShadowHealthKeyRejectsMalformedValueWithoutEchoingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shadow-health-key")
	secret := "invalid-secret-value"
	if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_SHADOW_HEALTH_KEY_FILE", path)
	_, err := shadowHealthKeyFromEnvironment()
	if err == nil {
		t.Fatal("malformed key was accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error exposed key material: %v", err)
	}
}
