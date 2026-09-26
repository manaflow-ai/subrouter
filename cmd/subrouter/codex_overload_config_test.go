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
	t.Setenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER", "")
	config, err = codexOverloadFailoverConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if config == nil || config.Enabled || !config.CapacityRetryPersist || config.CapacityRetryBudget != 90*time.Second {
		t.Fatalf("config = %+v, want persist without the failover", config)
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

// With nothing set the capacity retry is still configured (same account
// only); the account failover stays off.
func TestCodexOverloadConfigDefaultsToSameAccountRetry(t *testing.T) {
	for _, key := range []string{"SUBROUTER_CODEX_OVERLOAD_FAILOVER", "SUBROUTER_CODEX_OVERLOAD_MAX_ACCOUNTS", "SUBROUTER_CODEX_OVERLOAD_MARK_TTL", "SUBROUTER_CODEX_CAPACITY_RETRY", "SUBROUTER_CODEX_CAPACITY_RETRY_BUDGET"} {
		t.Setenv(key, "")
	}
	config, err := codexOverloadFailoverConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if config == nil || config.Enabled || config.CapacityRetryPersist {
		t.Fatalf("config = %+v, want the same-account default with failover off", config)
	}
	t.Setenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER", "1")
	if config, err = codexOverloadFailoverConfigFromEnvironment(); err != nil || !config.Enabled {
		t.Fatalf("config = %+v err = %v, want the failover opted in", config, err)
	}
}
