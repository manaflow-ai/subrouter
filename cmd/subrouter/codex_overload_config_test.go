package main

import (
	"testing"
	"time"
)

// Overload failover is on unless the operator turns it off explicitly.
func TestCodexOverloadFailoverDefaultsOn(t *testing.T) {
	t.Setenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER", "")
	config, err := codexOverloadFailoverConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if config == nil || !config.Enabled {
		t.Fatalf("config with the variable unset = %+v, want failover enabled", config)
	}
	for _, off := range []string{"0", "false", "FALSE", "no", "off"} {
		t.Setenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER", off)
		config, err := codexOverloadFailoverConfigFromEnvironment()
		if err != nil {
			t.Fatal(err)
		}
		if config != nil {
			t.Fatalf("SUBROUTER_CODEX_OVERLOAD_FAILOVER=%s left failover configured: %+v", off, config)
		}
	}
	t.Setenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER", "1")
	if config, err := codexOverloadFailoverConfigFromEnvironment(); err != nil || config == nil || !config.Enabled {
		t.Fatalf("explicit 1 = %+v (%v), want enabled", config, err)
	}
}

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
