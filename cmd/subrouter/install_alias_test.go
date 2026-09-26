package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeAliasTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// An `sr` or `cx` that belongs to some other tool must survive an install:
// before, installBinaryAlias removed whatever was at the alias path and, if
// the symlink then failed, left nothing behind.
func TestInstallBinaryAliasRefusesForeignRegularFile(t *testing.T) {
	dir := t.TempDir()
	subrouter := filepath.Join(dir, "subrouter")
	writeAliasTestFile(t, subrouter, "subrouter")
	shim := filepath.Join(dir, "sr")
	writeAliasTestFile(t, shim, "#!/bin/sh\necho someone else's sr\n")

	if err := installBinaryAlias(subrouter, shim); err == nil {
		t.Fatal("installBinaryAlias replaced a foreign file")
	}
	body, err := os.ReadFile(shim)
	if err != nil || string(body) != "#!/bin/sh\necho someone else's sr\n" {
		t.Fatalf("foreign file changed: %q, %v", body, err)
	}
}

func TestInstallBinaryAliasRefusesForeignSymlink(t *testing.T) {
	dir := t.TempDir()
	subrouter := filepath.Join(dir, "subrouter")
	writeAliasTestFile(t, subrouter, "subrouter")
	other := filepath.Join(dir, "search-replace")
	writeAliasTestFile(t, other, "other tool")
	shim := filepath.Join(dir, "sr")
	if err := os.Symlink(other, shim); err != nil {
		t.Fatal(err)
	}

	if err := installBinaryAlias(subrouter, shim); err == nil {
		t.Fatal("installBinaryAlias replaced a symlink to another tool")
	}
	if target, err := os.Readlink(shim); err != nil || target != other {
		t.Fatalf("foreign symlink changed: %q, %v", target, err)
	}
}

func TestInstallBinaryAliasReplacesOwnStaleSymlink(t *testing.T) {
	dir := t.TempDir()
	subrouter := filepath.Join(dir, "bin", "subrouter")
	if err := os.MkdirAll(filepath.Dir(subrouter), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAliasTestFile(t, subrouter, "subrouter")
	shim := filepath.Join(dir, "bin", "sr")
	// A dangling link left by an older install path is still ours.
	if err := os.Symlink(filepath.Join(dir, "old", "subrouter"), shim); err != nil {
		t.Fatal(err)
	}
	if err := installBinaryAlias(subrouter, shim); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(shim); err != nil || target != subrouter {
		t.Fatalf("alias target = %q, %v; want %q", target, err, subrouter)
	}
}
