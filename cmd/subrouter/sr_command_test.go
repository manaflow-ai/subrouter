package main

import (
	"runtime"
	"slices"
	"testing"
)

func TestMergeCommandEnvReplacesExistingValues(t *testing.T) {
	got := mergeCommandEnv(
		[]string{"PATH=/system", "HOME=/home", "KEEP=base"},
		[]string{"PATH=/chrome-shim:/system", "NEW=value"},
	)
	want := []string{"HOME=/home", "KEEP=base", "PATH=/chrome-shim:/system", "NEW=value"}
	if !slices.Equal(got, want) {
		t.Fatalf("mergeCommandEnv() = %#v, want %#v", got, want)
	}
}

func TestCommandEnvKeyMatchesPlatformRules(t *testing.T) {
	got := commandEnvKey("Path")
	if runtime.GOOS == "windows" {
		if got != "PATH" {
			t.Fatalf("commandEnvKey(Path) = %q, want PATH", got)
		}
	} else if got != "Path" {
		t.Fatalf("commandEnvKey(Path) = %q, want Path", got)
	}
}
