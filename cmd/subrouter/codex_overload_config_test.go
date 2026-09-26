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
