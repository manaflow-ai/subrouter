package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

func TestClaudeAdmissionModelRecoveryUsesMatchingObservedWindows(t *testing.T) {
	for _, matching := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh other model", true: "fresh matching model"}[matching], func(t *testing.T) {
			account := accounts.Account{ID: "recovered", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "token"}
			ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{account}, nil)
			gate := newAdmissionControl()
			key := admissionKeyFor(accounts.ProviderClaude, account.ID, agentclaude.SonnetFeature)
			gate.pauseWithQuotaScope(key, time.Now().Add(time.Hour), false, true)
			observed := time.Now()
			primary := append(healthyClaudeUpgradeWindows(), accounts.UsageWindow{Name: "sonnet-weekly", Feature: agentclaude.SonnetFeature, LimitWindowSeconds: 604800})
			ref.usageWindows = map[string]usageWindowsEntry{account.ID + "\x00claude": {at: observed, orderAt: observed, credentialKey: usageWindowsCredentialKey(account), windows: primary}}
			feature := agentclaude.OpusFeature
			if matching {
				feature = agentclaude.SonnetFeature
			}
			ref.claudeSupplemental = map[string]claudeSupplementalUsage{claudeSupplementalCacheKey(account): {at: observed, requestStarted: observed, windows: []accounts.UsageWindow{{Name: "model-weekly", Feature: feature, LimitWindowSeconds: 604800}}}}
			Server{AccountRef: ref, admission: gate}.reconcileClaudeAdmissionQuota()
			if paused := gate.paused(key, time.Now()); paused == matching {
				t.Fatalf("paused=%v after matching=%v model observation", paused, matching)
			}
		})
	}
}

func TestClaudeAdmissionModelRecoveryDoesNotCacheSyntheticPrimaryEvidence(t *testing.T) {
	for _, feature := range []string{agentclaude.SonnetFeature, agentclaude.OpusFeature} {
		for _, kind := range []string{"omitted", "unknown utilization", "observed zero"} {
			t.Run(feature+"/"+kind, func(t *testing.T) {
				account := accounts.Account{ID: "recovered", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "token"}
				ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{account}, nil)
				gate := newAdmissionControl()
				key := admissionKeyFor(accounts.ProviderClaude, account.ID, feature)
				gate.pauseWithQuotaScope(key, time.Now().Add(time.Hour), false, true)
				zero := 0.0
				usage := agentclaude.UsageResponse{FiveHour: &agentclaude.RateLimit{Utilization: &zero}, SevenDay: &agentclaude.RateLimit{Utilization: &zero}, SevenDayOAuthApps: &agentclaude.RateLimit{Utilization: &zero}}
				var limit *agentclaude.RateLimit
				if kind != "omitted" {
					limit = &agentclaude.RateLimit{}
					if kind == "observed zero" {
						limit.Utilization = &zero
					}
				}
				name := "sonnet-weekly"
				if feature == agentclaude.SonnetFeature {
					usage.SevenDaySonnet = limit
				} else {
					usage.SevenDayOpus = limit
					name = "opus-weekly"
				}
				body, err := json.Marshal(usage)
				if err != nil {
					t.Fatal(err)
				}
				client := &http.Client{Transport: proxyRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), Request: r}, nil
				})}
				if _, _, err := (Server{AccountRef: ref, admission: gate}).fetchAccountUsageWindows(context.Background(), client, account); err != nil {
					t.Fatal(err)
				}
				wantPaused := kind != "observed zero"
				if paused := gate.paused(key, time.Now()); paused != wantPaused {
					t.Fatalf("paused=%v, want %v after %s primary model quota", paused, wantPaused, kind)
				}
				ref.usageWindowsMu.Lock()
				primary := ref.usageWindows[account.ID+"\x00claude"].windows
				ref.usageWindowsMu.Unlock()
				if !usageWindowNamed(primary, name) {
					t.Fatal("routing placeholder disappeared from primary usage")
				}
			})
		}
	}
}
