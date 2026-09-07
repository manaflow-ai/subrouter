package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintVersionIncludesProgramAndFields(t *testing.T) {
	version = "1.2.3"
	commit = "abcdef012345"
	buildDate = "2026-08-07T00:00:00Z"
	t.Cleanup(func() {
		version, commit, buildDate = "", "", ""
	})

	var buf bytes.Buffer
	printVersion(&buf, "sr-gcp")
	got := buf.String()
	for _, want := range []string{"sr-gcp", "1.2.3", "abcdef012345", "2026-08-07T00:00:00Z"} {
		if !strings.Contains(got, want) {
			t.Fatalf("version output %q missing %q", got, want)
		}
	}
}

func TestIsVersionCommand(t *testing.T) {
	for _, arg := range []string{"version", "-v", "--version"} {
		if !isVersionCommand(arg) {
			t.Fatalf("expected %q to be a version command", arg)
		}
	}
	if isVersionCommand("status") {
		t.Fatal("status must not be treated as a version command")
	}
}

func TestRunForProgramVersion(t *testing.T) {
	version = "9.9.9"
	commit = "deadbeefcafe"
	buildDate = "2026-01-01T00:00:00Z"
	t.Cleanup(func() {
		version, commit, buildDate = "", "", ""
	})
	for _, arg := range []string{"version", "--version"} {
		if err := runForProgram("subrouter", []string{arg}); err != nil {
			t.Fatalf("runForProgram(%q): %v", arg, err)
		}
	}
}
