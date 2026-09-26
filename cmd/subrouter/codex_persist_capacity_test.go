package main

import (
	"strings"
	"testing"
)

func TestCodexPersistCapacityFlagIsTakenBeforeTerminatorOnly(t *testing.T) {
	args, found := takeCodexPersistCapacityFlag([]string{"--persist-capacity", "exec", "--", "--persist-capacity"})
	if !found {
		t.Fatal("--persist-capacity not recognized")
	}
	if got := strings.Join(args, " "); got != "exec -- --persist-capacity" {
		t.Fatalf("args = %q, want the flag removed only before --", got)
	}
	if _, found := takeCodexPersistCapacityFlag([]string{"exec", "--", "--persist-capacity"}); found {
		t.Fatal("prompt text after -- was taken as the flag")
	}
}

// The persist leaves must come after the launcher's whole-table provider
// override, or Codex's in-order -c merge would drop them.
func TestCodexPersistCapacityConfigFollowsProviderTable(t *testing.T) {
	args := codexArgs([]string{"exec", "hi", "--", "tail"}, "http://127.0.0.1:31415/v1", "", "")
	args = appendCodexConfigBeforeTerminator(args, codexPersistCapacityConfigArgs())
	table, header, retries, terminator := -1, -1, -1, -1
	for i, arg := range args {
		switch {
		case strings.HasPrefix(arg, "model_providers.subrouter={"):
			table = i
		case arg == `model_providers.subrouter.http_headers.X-Subrouter-Capacity-Retry="persist"`:
			header = i
		case strings.HasPrefix(arg, "model_providers.subrouter.stream_max_retries="):
			retries = i
		case arg == "--":
			terminator = i
		}
	}
	if table < 0 || header < table || retries < table || terminator < retries || terminator < header {
		t.Fatalf("args = %q, want the persist overrides after the provider table and before --", args)
	}
}
