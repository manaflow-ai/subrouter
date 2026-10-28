package proxy

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

func TestClaudeQuotaRecoveryPreservesModelRejectionInMixedResponse(t *testing.T) {
	for _, pool := range []struct{ feature, prefix string }{
		{agentclaude.SonnetFeature, "7d_sonnet"},
		{agentclaude.OpusFeature, "7d_opus"},
		{agentclaude.FableFeature, "7d_oi"},
	} {
		t.Run(pool.feature, func(t *testing.T) {
			for _, tc := range []struct {
				name                      string
				accountReset, modelReset  time.Duration
				modelRecovered, unrelated bool
				modelStatus               string
			}{
				{name: "model reset later", accountReset: time.Hour, modelReset: 2 * time.Hour},
				{name: "account reset later", accountReset: 2 * time.Hour, modelReset: time.Hour},
				{name: "new matching model quota also recovered", accountReset: time.Hour, modelReset: 2 * time.Hour, modelRecovered: true},
				{name: "unrelated model rejection is ignored", accountReset: time.Hour, modelReset: 2 * time.Hour, unrelated: true},
				{name: "model status with whitespace", accountReset: time.Hour, modelReset: 2 * time.Hour, modelStatus: " rejected "},
			} {
				t.Run(tc.name, func(t *testing.T) {
					account := accounts.Account{ID: "recovered", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "token"}
					ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{account}, nil)
					gate := newAdmissionControl()
					key := admissionKeyFor(accounts.ProviderClaude, account.ID, pool.feature)
					now := time.Now()
					accountUntil := time.Unix(now.Add(tc.accountReset).Unix(), 0)
					modelUntil := time.Unix(now.Add(tc.modelReset).Unix(), 0)
					header := http.Header{
						"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"},
						"Anthropic-Ratelimit-Unified-5h-Reset":  {strconv.FormatInt(accountUntil.Unix(), 10)},
					}
					prefix := pool.prefix
					if tc.unrelated {
						prefix = "7d_opus"
						if pool.feature == agentclaude.OpusFeature {
							prefix = "7d_sonnet"
						}
					}
					status := tc.modelStatus
					if status == "" {
						status = "rejected"
					}
					header.Set("Anthropic-Ratelimit-Unified-"+prefix+"-Status", status)
					header.Set("Anthropic-Ratelimit-Unified-"+prefix+"-Reset", strconv.FormatInt(modelUntil.Unix(), 10))
					gate.observe(key, http.StatusTooManyRequests, header, now)
					observed := time.Now()
					// A primary usage refresh proves base quota recovered after
					// the earlier supplemental model observation expired.
					ref.usageWindows = map[string]usageWindowsEntry{account.ID + "\x00claude": {
						at: observed, orderAt: observed, credentialKey: usageWindowsCredentialKey(account), windows: healthyClaudeUpgradeWindows(),
					}}
					if tc.modelRecovered {
						ref.claudeSupplemental = map[string]claudeSupplementalUsage{claudeSupplementalCacheKey(account): {
							at: observed, requestStarted: observed, windows: []accounts.UsageWindow{{Name: "model-weekly", Feature: pool.feature, LimitWindowSeconds: 604800}},
						}}
					}
					Server{AccountRef: ref, admission: gate}.reconcileClaudeAdmissionQuota()
					gate.mu.Lock()
					until, throttled := gate.buckets[key].pausedTil, gate.buckets[key].throttled
					gate.mu.Unlock()
					want := modelUntil
					if tc.modelRecovered || tc.unrelated {
						want = time.Time{}
					}
					if !until.Equal(want) {
						t.Fatalf("remaining pause = %s, want %s", until, want)
					}
					if throttled != 1 {
						t.Fatalf("one 429 counted as %d throttles", throttled)
					}
				})
			}
		})
	}
}
