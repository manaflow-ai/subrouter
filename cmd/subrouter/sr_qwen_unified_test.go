package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestQwenBackendEnrollmentUsesSelectedServer(t *testing.T) {
	var uploads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer import-key" {
			t.Error("import did not use protected credential")
		}
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"ok":true,"providers":["qwen-token"]}`)
			return
		}
		var body serverAccountImportRequest
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Provider != accounts.ProviderQwenToken || body.Codex == nil || body.Codex.Email != "qwen-token:work" {
			t.Error("wrong backend enrollment")
		}
		uploads++
		io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	var out bytes.Buffer
	runner := srRunner{client: server.Client(), in: strings.NewReader("work\nplan-secret\n"), out: &out, errOut: &out}
	if err := runner.addKeyToServer(t.Context(), srServerConfig{Name: "test", URL: server.URL, AccountImportToken: "import-key"}, []string{"--provider", "qwen", "--backend", "token-plan"}); err != nil {
		t.Fatal(err)
	}
	if uploads != 1 || strings.Contains(out.String(), "plan-secret") {
		t.Fatal("enrollment missing or credential displayed")
	}
}

func TestQwenSharedTableKeepsPlanQuotaColumns(t *testing.T) {
	t.Setenv("COLUMNS", "200")
	reset := time.Now().Add(time.Hour)
	rows := []srUsageRow{{email: "member", provider: accounts.ProviderQwenOAuth, authMode: accounts.AuthModeOAuth, planType: "pro"}, {email: "plan", provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey, planType: "Pro", quotaUsageKnown: true, windows: []accounts.UsageWindow{{Name: "5h", UsedPercent: 25, LimitWindowSeconds: 18000, ResetAt: reset}, {Name: "weekly", UsedPercent: 50, LimitWindowSeconds: 604800, ResetAt: reset}}}}
	var out bytes.Buffer
	displayUsageRows(&out, rows, false)
	for _, value := range []string{"5h", "7d", "75%", "50%"} {
		if !strings.Contains(out.String(), value) {
			t.Fatalf("shared table hid %q:\n%s", value, out.String())
		}
	}
}
