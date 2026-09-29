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

// codexOverloadFailoverConfigFromEnvironment reads the Codex capacity retry
// settings. Capacity failures (before any output) are always retried on the
// session's own account, keeping its prompt cache, for up to
// SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT (default 4m; 0 = until the client
// disconnects; ~10s when an egress or Azure fallback is configured), with
// steady gaps of SUBROUTER_CODEX_CAPACITY_RETRY_INTERVAL (default ~9s, at
// least 500ms) after the ramp.
// Account failover is opt-in with SUBROUTER_CODEX_OVERLOAD_FAILOVER=1. The
// default keeps a live session on its account and waits through capacity instead
// (~10s ladder), with optional SUBROUTER_CODEX_OVERLOAD_MAX_ACCOUNTS and
// SUBROUTER_CODEX_OVERLOAD_MARK_TTL (Go duration). A conversation of more
// than SUBROUTER_CODEX_OVERLOAD_FAILOVER_MAX_INPUT estimated input tokens
// (default 32000; 0 = no limit) keeps its account and its prompt cache: it
// takes the same-account wait even with the failover on.
// Persistent retry is enabled by default for up to
// SUBROUTER_CODEX_CAPACITY_RETRY_BUDGET (default 10m): on the same
// account, or across accounts with the failover on. The per-request
// X-Subrouter-Capacity-Retry and X-Subrouter-Retry headers are honored only
// with the failover on or SUBROUTER_CODEX_CAPACITY_RETRY_HEADER=1;
// X-Subrouter-Retry shapes only the same-account wait (failover off), so
// with the failover on it is accepted but has no effect.
func codexOverloadFailoverConfigFromEnvironment() (*proxy.CodexOverloadFailoverConfig, error) {
	// Capacity failover is opt-in. Keeping it off by default prevents a capacity
	// response from becoming a websocket 1012 reconnect storm that can end a
	// live Codex turn; same-account retry remains enabled.
	config := &proxy.CodexOverloadFailoverConfig{
		Enabled:              envTrue("SUBROUTER_CODEX_OVERLOAD_FAILOVER"),
		CapacityRetryPersist: true,
		CapacityRetryBudget:  10 * time.Minute,
		CapacityRetryHeader:  envTrue("SUBROUTER_CODEX_CAPACITY_RETRY_HEADER"),
	}
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
	if raw := strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER_MAX_INPUT")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("SUBROUTER_CODEX_OVERLOAD_FAILOVER_MAX_INPUT=%q: want a number of input tokens, or 0 for no limit", raw)
		}
		config.FailoverMaxInput, config.FailoverMaxInputUnlimited = n, n == 0
	}
	maxWait, unbounded, _, err := overloadMaxWaitFromEnvironment("SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT")
	if err != nil {
		return nil, err
	}
	config.StayMaxWait, config.StayUnbounded = maxWait, unbounded
	if config.StayInterval, err = overloadIntervalFromEnvironment("SUBROUTER_CODEX_CAPACITY_RETRY_INTERVAL"); err != nil {
		return nil, err
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
