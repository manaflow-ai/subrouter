package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestQwenUnifiedStatusSeparatesBackendPlans(t *testing.T) {
	t.Setenv("COLUMNS", "200")
	rows := []srUsageRow{{accountID: "qwen-subscription:member", email: "member", provider: accounts.ProviderQwenOAuth, authMode: accounts.AuthModeOAuth, planType: "pro"}, {accountID: "qwen-token:plan", email: "plan", provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey, planType: "Pro"}}
	status := buildSRStatusJSON(rows, "test", time.Now())
	group := status.Providers["qwen"]
	if len(status.Providers) != 1 || group == nil || len(group.Accounts) != 2 {
		t.Fatalf("accounts were not grouped under Qwen: %+v", status.Providers)
	}
	if group.Accounts[0].Backend != "subscription" || group.Accounts[1].Backend != "token-plan" {
		t.Fatalf("backend identity lost: %+v", group.Accounts)
	}
	var out bytes.Buffer
	rankUsageRows(rows)
	displayUsageRows(&out, rows, false)
	if strings.Count(out.String(), "Qwen accounts") != 1 || !strings.Contains(out.String(), "Qwen.ai pro") || !strings.Contains(out.String(), "Token Plan Pro") {
		t.Fatalf("missing unified heading or separate plans:\n%s", out.String())
	}
}

func TestQwenBackendEnrollmentAndNativePins(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want accounts.Provider
	}{{[]string{"--provider", "qwen", "--backend", "token-plan"}, accounts.ProviderQwenToken}, {[]string{"--provider", "qwen", "--backend", "coding-plan"}, accounts.ProviderQwen}, {[]string{"--provider", "qwen-token"}, accounts.ProviderQwenToken}} {
		got, err := parseAddKeyProviderArgs("test", io.Discard, tt.args)
		if err != nil || got != tt.want {
			t.Fatalf("args=%v got=%s err=%v", tt.args, got, err)
		}
	}
	for _, args := range [][]string{{"--provider", "codex", "--backend", "token-plan"}, {"--provider", "qwen", "--backend", "unknown"}} {
		if _, err := parseAddKeyProviderArgs("test", io.Discard, args); err == nil {
			t.Fatalf("accepted invalid backend: %v", args)
		}
	}
	inventory := []remoteServerAccount{{ID: "qwen-subscription:member", Provider: accounts.ProviderQwenOAuth, AuthMode: accounts.AuthModeOAuth}, {ID: "qwen-token:plan", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey}}
	for _, selector := range []string{"member", "plan"} {
		id, err := resolveNativeProxyAccountSelector(qwenNativeProxy, inventory, selector)
		if err != nil || !strings.HasSuffix(id, ":"+selector) {
			t.Fatalf("selector=%s id=%s err=%v", selector, id, err)
		}
	}
}
