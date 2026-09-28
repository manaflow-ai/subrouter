package main

import (
	"testing"
	"time"
)

func TestOverloadMaxWaitEnvironment(t *testing.T) {
	t.Setenv("SUBROUTER_CLAUDE_OVERLOAD_MAX_WAIT", "")
	t.Setenv("SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT", "")
	claude, err := claudeOverloadRetryConfigFromEnvironment()
	if err != nil || claude.MaxWait != 0 || claude.Unbounded {
		t.Fatalf("claude = %+v err = %v, want the default", claude, err)
	}
	codex, err := codexOverloadFailoverConfigFromEnvironment()
	if err != nil || codex.StayMaxWait != 0 || codex.StayUnbounded {
		t.Fatalf("codex = %+v err = %v, want the default", codex, err)
	}

	t.Setenv("SUBROUTER_CLAUDE_OVERLOAD_MAX_WAIT", "20m")
	t.Setenv("SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT", "90s")
	if claude, err = claudeOverloadRetryConfigFromEnvironment(); err != nil || claude.MaxWait != 20*time.Minute || claude.Unbounded {
		t.Fatalf("claude = %+v err = %v, want 20m", claude, err)
	}
	if codex, err = codexOverloadFailoverConfigFromEnvironment(); err != nil || codex.StayMaxWait != 90*time.Second || codex.StayUnbounded {
		t.Fatalf("codex = %+v err = %v, want 90s", codex, err)
	}

	for _, zero := range []string{"0", "0s"} {
		t.Setenv("SUBROUTER_CLAUDE_OVERLOAD_MAX_WAIT", zero)
		t.Setenv("SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT", zero)
		if claude, err = claudeOverloadRetryConfigFromEnvironment(); err != nil || !claude.Unbounded {
			t.Fatalf("claude %q = %+v err = %v, want no cap", zero, claude, err)
		}
		if codex, err = codexOverloadFailoverConfigFromEnvironment(); err != nil || !codex.StayUnbounded {
			t.Fatalf("codex %q = %+v err = %v, want no cap", zero, codex, err)
		}
	}

	for _, bad := range []string{"soon", "-1m"} {
		t.Setenv("SUBROUTER_CLAUDE_OVERLOAD_MAX_WAIT", bad)
		if _, err := claudeOverloadRetryConfigFromEnvironment(); err == nil {
			t.Fatalf("SUBROUTER_CLAUDE_OVERLOAD_MAX_WAIT=%q accepted", bad)
		}
		t.Setenv("SUBROUTER_CLAUDE_OVERLOAD_MAX_WAIT", "")
		t.Setenv("SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT", bad)
		if _, err := codexOverloadFailoverConfigFromEnvironment(); err == nil {
			t.Fatalf("SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT=%q accepted", bad)
		}
		t.Setenv("SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT", "")
	}
}
