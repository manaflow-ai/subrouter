package main

import (
	"testing"
	"time"
)

func TestCodexOverloadConfigReadsCapacityRetryEnvironment(t *testing.T) {
	t.Setenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER", "1")
	t.Setenv("SUBROUTER_CODEX_CAPACITY_RETRY", "persist")
	t.Setenv("SUBROUTER_CODEX_CAPACITY_RETRY_BUDGET", "90s")
	config, err := codexOverloadFailoverConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if config == nil || !config.CapacityRetryPersist || config.CapacityRetryBudget != 90*time.Second {
		t.Fatalf("config = %+v, want persist with a 90s budget", config)
	}

	// Persist mode does not need the account failover: it then stays on the
	// session's account.
	t.Setenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER", "0")
	config, err = codexOverloadFailoverConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if config == nil || config.Enabled || !config.CapacityRetryPersist || config.CapacityRetryBudget != 90*time.Second {
		t.Fatalf("config = %+v, want persist with failover explicitly disabled", config)
	}

	t.Setenv("SUBROUTER_CODEX_CAPACITY_RETRY", "hammer")
	if _, err := codexOverloadFailoverConfigFromEnvironment(); err == nil {
		t.Fatal("unknown capacity retry mode accepted")
	}
	t.Setenv("SUBROUTER_CODEX_CAPACITY_RETRY", "default")
	t.Setenv("SUBROUTER_CODEX_CAPACITY_RETRY_BUDGET", "soon")
	if _, err := codexOverloadFailoverConfigFromEnvironment(); err == nil {
		t.Fatal("invalid capacity retry budget accepted")
	}
}

func TestCodexOverloadConfigReadsFailoverMaxInput(t *testing.T) {
	t.Setenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER", "1")
	t.Setenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER_MAX_INPUT", "")
	config, err := codexOverloadFailoverConfigFromEnvironment()
	if err != nil || config.FailoverMaxInput != 0 || config.FailoverMaxInputUnlimited {
		t.Fatalf("unset: config = %+v err = %v, want the built-in default", config, err)
	}
	t.Setenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER_MAX_INPUT", "200000")
	if config, err = codexOverloadFailoverConfigFromEnvironment(); err != nil || config.FailoverMaxInput != 200000 || config.FailoverMaxInputUnlimited {
		t.Fatalf("200000: config = %+v err = %v", config, err)
	}
	t.Setenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER_MAX_INPUT", "0")
	if config, err = codexOverloadFailoverConfigFromEnvironment(); err != nil || !config.FailoverMaxInputUnlimited {
		t.Fatalf("0: config = %+v err = %v, want no limit", config, err)
	}
	for _, bad := range []string{"-1", "32k", "lots"} {
		t.Setenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER_MAX_INPUT", bad)
		if _, err := codexOverloadFailoverConfigFromEnvironment(); err == nil {
			t.Fatalf("SUBROUTER_CODEX_OVERLOAD_FAILOVER_MAX_INPUT=%q accepted", bad)
		}
	}
}

// With nothing set capacity retry stays on the session account; failover is opt-in.
func TestCodexOverloadConfigDefaultsToSameAccountRetry(t *testing.T) {
	for _, key := range []string{"SUBROUTER_CODEX_OVERLOAD_FAILOVER", "SUBROUTER_CODEX_OVERLOAD_MAX_ACCOUNTS", "SUBROUTER_CODEX_OVERLOAD_MARK_TTL", "SUBROUTER_CODEX_CAPACITY_RETRY", "SUBROUTER_CODEX_CAPACITY_RETRY_BUDGET", "SUBROUTER_CODEX_CAPACITY_RETRY_HEADER"} {
		t.Setenv(key, "")
	}
	config, err := codexOverloadFailoverConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if config == nil || config.Enabled || !config.CapacityRetryPersist || config.FailoverMaxInputUnlimited || config.CapacityRetryBudget != 10*time.Minute || config.CapacityRetryHeader {
		t.Fatalf("config = %+v, want persistent same-account retry with client headers off", config)
	}
	t.Setenv("SUBROUTER_CODEX_CAPACITY_RETRY_HEADER", "1")
	if config, err = codexOverloadFailoverConfigFromEnvironment(); err != nil || !config.CapacityRetryHeader || config.Enabled {
		t.Fatalf("config = %+v err = %v, want client capacity retry headers without implicit failover", config, err)
	}
	t.Setenv("SUBROUTER_CODEX_CAPACITY_RETRY_HEADER", "")
	t.Setenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER", "1")
	if config, err = codexOverloadFailoverConfigFromEnvironment(); err != nil || !config.Enabled || !config.CapacityRetryPersist {
		t.Fatalf("config = %+v err = %v, want persistence independent of explicit failover=1", config, err)
	}
}
