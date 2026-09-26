package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// codexEgressConfigFromEnvironment reads SUBROUTER_CODEX_EGRESS_PROXIES, a
// comma-separated list of HTTP CONNECT proxy URLs in other regions
// (for example http://cmux-egress-fra:3128,http://cmux-egress-tyo:3128).
// Unset or empty means the feature is off and the Codex path is unchanged.
func codexEgressConfigFromEnvironment(sessionPath string) (*proxy.CodexEgressConfig, error) {
	raw := strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_EGRESS_PROXIES"))
	if raw == "" {
		return nil, nil
	}
	proxies, err := proxy.ParseCodexEgressProxies(raw)
	if err != nil {
		return nil, err
	}
	if len(proxies) == 0 {
		return nil, nil
	}
	return &proxy.CodexEgressConfig{
		Proxies:      proxies,
		PinStorePath: filepath.Join(filepath.Dir(sessionPath), "codex-egress-pins.json"),
	}, nil
}

// codexOverloadFailoverConfigFromEnvironment reads
// SUBROUTER_CODEX_OVERLOAD_FAILOVER=1 (off unless set), with optional
// SUBROUTER_CODEX_OVERLOAD_MAX_ACCOUNTS and SUBROUTER_CODEX_OVERLOAD_MARK_TTL
// (Go duration). SUBROUTER_CODEX_CAPACITY_RETRY=persist keeps retrying
// capacity failures (before any output) until
// SUBROUTER_CODEX_CAPACITY_RETRY_BUDGET (default 2m) instead of the default
// ~10s of quick retries.
func codexOverloadFailoverConfigFromEnvironment() (*proxy.CodexOverloadFailoverConfig, error) {
	if !envTrue("SUBROUTER_CODEX_OVERLOAD_FAILOVER") {
		return nil, nil
	}
	config := &proxy.CodexOverloadFailoverConfig{Enabled: true}
	if raw := strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_OVERLOAD_MAX_ACCOUNTS")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("SUBROUTER_CODEX_OVERLOAD_MAX_ACCOUNTS=%q: want a positive integer", raw)
		}
		config.MaxAccounts = n
	}
	if raw := strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_OVERLOAD_MARK_TTL")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("SUBROUTER_CODEX_OVERLOAD_MARK_TTL=%q: want a positive duration", raw)
		}
		config.MarkTTL = d
	}
	if raw := os.Getenv("SUBROUTER_CODEX_CAPACITY_RETRY"); strings.TrimSpace(raw) != "" {
		persist, ok := proxy.ParseCodexCapacityRetryMode(raw)
		if !ok {
			return nil, fmt.Errorf("SUBROUTER_CODEX_CAPACITY_RETRY=%q: want persist or default", raw)
		}
		config.CapacityRetryPersist = persist
	}
	if raw := strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_CAPACITY_RETRY_BUDGET")); raw != "" {
		budget := proxy.ParseCodexCapacityRetryBudget(raw)
		if budget <= 0 {
			return nil, fmt.Errorf("SUBROUTER_CODEX_CAPACITY_RETRY_BUDGET=%q: want a positive duration such as 2m (capped at 10m)", raw)
		}
		config.CapacityRetryBudget = budget
	}
	return config, nil
}
