package main

import (
	"debug/buildinfo"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestInstallBinaryAliasForceReplacesForeignFile(t *testing.T) {
	dir := t.TempDir()
	subrouter := filepath.Join(dir, "subrouter")
	writeAliasTestFile(t, subrouter, "subrouter")
	shim := filepath.Join(dir, "cx")
	writeAliasTestFile(t, shim, "foreign")
	if err := installBinaryAlias(subrouter, shim); !errors.Is(err, errForeignAlias) || !strings.Contains(err.Error(), "--force-shims") {
		t.Fatalf("error = %v, want a foreign-alias refusal naming --force-shims", err)
	}
	if err := installBinaryAliasWith(subrouter, shim, true); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(shim); err != nil || target != subrouter {
		t.Fatalf("alias target = %q, %v; want %q", target, err, subrouter)
	}
}

// The test binary is built from this package, so its build info pins that
// subrouterMainPackage matches the real import path.
func TestSubrouterMainPackageMatchesThisPackage(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	info, err := buildinfo.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSuffix(info.Path, ".test") != subrouterMainPackage {
		t.Fatalf("build info path = %q, want %q", info.Path, subrouterMainPackage)
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
