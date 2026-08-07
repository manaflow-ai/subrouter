package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GNU coreutils treats `stat -f` as "filesystem status" and exits 0, so a
// BSD-first ownership probe never falls back to `stat -c` and poisons
// `install -o` with a multi-line filesystem report (see #126).
func TestNoBSDFirstStatOwnerProbeInCmdSources(t *testing.T) {
	root := "."
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	var walk func(string)
	walk = func(dir string) {
		items, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			path := filepath.Join(dir, item.Name())
			if item.IsDir() {
				walk(path)
				continue
			}
			if strings.HasSuffix(path, ".go") {
				files = append(files, path)
			}
		}
	}
	_ = entries
	walk(root)

	forbidden := []string{
		"stat -f '%Su'",
		`stat -f "%Su"`,
		"stat -f '%Sg'",
		`stat -f "%Sg"`,
	}
	for _, path := range files {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		for _, needle := range forbidden {
			if !strings.Contains(text, needle) {
				continue
			}
			// Allow the pattern only if GNU-first ordering is clearly present
			// on the same line / nearby. Prefer failing closed.
			idx := strings.Index(text, needle)
			windowStart := idx - 80
			if windowStart < 0 {
				windowStart = 0
			}
			windowEnd := idx + len(needle) + 80
			if windowEnd > len(text) {
				windowEnd = len(text)
			}
			window := text[windowStart:windowEnd]
			if strings.Contains(window, "stat -c") && strings.Index(window, "stat -c") < strings.Index(window, "stat -f") {
				continue
			}
			t.Fatalf("%s contains BSD-first ownership probe %q; use GNU-first: stat -c then stat -f (#126)", path, needle)
		}
	}
}
