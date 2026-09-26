package main

import (
	"fmt"
	"io"
	"os"

	"github.com/manaflow-ai/subrouter/internal/buildversion"
)

// versionOut is where `<program> version` prints; tests replace it.
var versionOut io.Writer = os.Stdout

func isVersionCommand(arg string) bool {
	switch arg {
	case "version", "--version", "-version":
		return true
	default:
		return false
	}
}

func printVersion(w io.Writer, program string) {
	fmt.Fprintf(w, "%s %s\n", program, buildversion.Get())
}
