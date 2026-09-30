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

func TestCodexGoalResumeFlagDefaultsOnAndStopsAtTerminator(t *testing.T) {
	t.Setenv(subrouterCodexGoalResumeEnv, "")
	if !codexGoalResumeEnabled() {
		t.Fatal("empty goal resume setting should default on")
	}
	t.Setenv(subrouterCodexGoalResumeEnv, "0")
	if codexGoalResumeEnabled() {
		t.Fatal("zero goal resume setting should disable the feature")
	}
	args, enabled := takeCodexGoalResumeFlag([]string{"exec", "--no-goal-resume", "--", "--no-goal-resume"})
	if enabled || strings.Join(args, " ") != "exec -- --no-goal-resume" {
		t.Fatalf("args=%q enabled=%v, want flag removed only before -- and disabled", args, enabled)
	}
	if _, enabled = takeCodexGoalResumeFlag([]string{"exec"}); !enabled {
		t.Fatal("goal resume should default on")
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

func TestCodexGoalResumeConfigFollowsProviderTable(t *testing.T) {
	args := codexArgs([]string{"exec", "hi", "--", "tail"}, "http://127.0.0.1:31415/v1", "", "")
	args = appendCodexConfigBeforeTerminator(args, codexGoalResumeConfigArgs())
	table, goals, retryable, requestRetries, streamRetries, terminator := -1, -1, -1, -1, -1, -1
	for i, arg := range args {
		switch {
		case strings.HasPrefix(arg, "model_providers.subrouter={"):
			table = i
		case arg == "features.goals=true":
			goals = i
		case arg == `model_providers.subrouter.http_headers.X-Subrouter-Capacity-Retryable="1"`:
			retryable = i
		case arg == "model_providers.subrouter.request_max_retries=100":
			requestRetries = i
		case arg == "model_providers.subrouter.stream_max_retries=100":
			streamRetries = i
		case arg == "--":
			terminator = i
		}
	}
	if table < 0 || goals < 0 || retryable < table || requestRetries < table || streamRetries < table || terminator < streamRetries {
		t.Fatalf("args = %q, want retry settings after provider table and before --", args)
	}
}
