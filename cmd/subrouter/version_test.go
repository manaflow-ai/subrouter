package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/buildversion"
)

func TestVersionCommandPrintsBuildIdentity(t *testing.T) {
	info := buildversion.Get()
	for _, program := range []string{"subrouter", "sr"} {
		for _, arg := range []string{"version", "--version", "-version"} {
			var out bytes.Buffer
			previous := versionOut
			versionOut = &out
			err := runForProgram(program, []string{arg})
			versionOut = previous
			if err != nil {
				t.Fatalf("%s %s: %v", program, arg, err)
			}
			got := out.String()
			want := program + " " + info.Version + " (commit "
			if !strings.HasPrefix(got, want) || !strings.HasSuffix(got, ")\n") {
				t.Fatalf("%s %s printed %q, want prefix %q", program, arg, got, want)
			}
			if !strings.Contains(got, info.GoVersion) {
				t.Fatalf("%s %s printed %q without Go version %q", program, arg, got, info.GoVersion)
			}
		}
	}
}

func TestIsVersionCommand(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-version"} {
		if !isVersionCommand(arg) {
			t.Fatalf("expected %q to be a version command", arg)
		}
	}
	for _, arg := range []string{"status", "-v", "versions"} {
		if isVersionCommand(arg) {
			t.Fatalf("%q must not be treated as a version command", arg)
		}
	}
}
