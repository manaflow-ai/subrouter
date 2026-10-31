package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type claudeBootstrapTransport struct {
	usageStatus, fableStatus, primaryStatus  int
	fableBody, primaryUsed, fablePrimaryUsed string
	primaryEmpty                             bool
	models                                   []string
}

func (c *claudeBootstrapTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	h := make(http.Header)
	code, body := c.usageStatus, `{"error":{"type":"permission_error","details":{"error_code":"oauth_scope_insufficient"}}}`
	if strings.HasSuffix(req.URL.Path, "/api/oauth/usage") {
		if code == http.StatusOK {
			body = `{"five_hour":{"utilization":12,"resets_at":"2030-01-01T00:00:00Z"},"seven_day":{"utilization":5,"resets_at":"2030-01-02T00:00:00Z"}}`
		}
		if code == http.StatusTooManyRequests {
			h.Set("Retry-After", "1800")
		}
	} else if strings.HasSuffix(req.URL.Path, "/v1/messages") {
		var payload struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			System    []struct {
				Text string `json:"text"`
			} `json:"system"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			return nil, err
		}
		if payload.MaxTokens != 1 || len(payload.System) != 1 || !strings.Contains(payload.System[0].Text, "You are Claude Code") || req.Header.Get("x-app") != "cli" {
			return nil, fmt.Errorf("probe lacks Claude Code request shape")
		}
		c.models = append(c.models, payload.Model)
		if payload.Model == agentclaude.FableModel {
			code, body = c.fableStatus, c.fableBody
			if c.fablePrimaryUsed != "" {
				for _, window := range []string{"5h", "7d"} {
					h.Set("anthropic-ratelimit-unified-"+window+"-status", "rejected")
					h.Set("anthropic-ratelimit-unified-"+window+"-utilization", c.fablePrimaryUsed)
					h.Set("anthropic-ratelimit-unified-"+window+"-reset", fmt.Sprint(time.Now().Add(time.Hour).Unix()))
				}
			}
		} else if payload.Model == "claude-sonnet-5" {
			code, body = c.primaryStatus, `{"type":"message"}`
			if !c.primaryEmpty {
				for _, window := range []string{"5h", "7d"} {
					h.Set("anthropic-ratelimit-unified-"+window+"-utilization", c.primaryUsed)
					h.Set("anthropic-ratelimit-unified-"+window+"-status", "allowed")
					h.Set("anthropic-ratelimit-unified-"+window+"-reset", fmt.Sprint(time.Now().Add(time.Hour).Unix()))
				}
			}
		} else {
			return nil, fmt.Errorf("unexpected model %q", payload.Model)
		}
	} else {
		return nil, fmt.Errorf("unexpected path %s", req.URL.Path)
	}
	return &http.Response{StatusCode: code, Status: fmt.Sprintf("%d %s", code, http.StatusText(code)), Header: h, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestClaudeSetupTokenFableCreditsBootstrap(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	transport := &claudeBootstrapTransport{usageStatus: http.StatusForbidden, fableStatus: http.StatusTooManyRequests,
		fableBody:     `{"type":"error","error":{"type":"rate_limit_error","details":{"error_code":"credits_required","disabled_reason":"org_level_disabled"}}}`,
		primaryStatus: http.StatusOK, primaryUsed: "0.1"}
	ref := &AccountRef{}
	windows, fresh, err := ref.FetchUsageWindowsCached(context.Background(), &http.Client{Transport: transport}, probeAccount())
	if err != nil || !fresh || !fableProbeHasPrimaryWindows(windows) {
		t.Fatalf("new inference-only account cannot enter routing: windows=%+v fresh=%v err=%v", windows, fresh, err)
	}
	account := probeAccount()
	score := scoreFromUsageWindows(account.Provider, account.ID, windows)
	if !scoreUsableForNewSession(score) {
		t.Fatalf("discovered quota is still excluded from new sessions: %+v", score)
	}
	if score.ClaudeExtraUsageEnabled || score.ClaudeExtraUsageKnown {
		t.Fatal("ordinary quota was misclassified as paid extra usage")
	}
	if got := strings.Join(transport.models, ","); got != agentclaude.FableModel+",claude-sonnet-5" {
		t.Fatalf("probe models=%s", got)
	}
	for _, w := range windows {
		if w.Feature == agentclaude.FableFeature {
			t.Fatalf("ordinary probe fabricated Fable availability: %+v", w)
		}
	}
	if _, _, err := ref.FetchUsageWindowsCached(context.Background(), &http.Client{Transport: transport}, probeAccount()); err != nil {
		t.Fatal(err)
	}
	if len(transport.models) != 2 {
		t.Fatalf("cached usage repeated probes: %v", transport.models)
	}
}

func TestClaudeFableCreditsBootstrapPreservesRejections(t *testing.T) {
	for _, tc := range []struct {
		name          string
		change        func(*claudeBootstrapTransport)
		wantModels    int
		wantError     bool
		wantExhausted bool
	}{
		{"usage endpoint retry deadline", func(c *claudeBootstrapTransport) { c.usageStatus = http.StatusTooManyRequests }, 0, true, false},
		{"ambiguous Fable rate limit", func(c *claudeBootstrapTransport) { c.fableBody = `{"error":{"type":"rate_limit_error"}}` }, 1, true, false},
		{"unrecognized credit error type", func(c *claudeBootstrapTransport) {
			c.fableBody = `{"error":{"type":"overloaded_error","details":{"error_code":"credits_required"}}}`
		}, 1, true, false},
		{"dead Fable credential", func(c *claudeBootstrapTransport) { c.fableStatus = http.StatusUnauthorized }, 1, true, false},
		{"Fable authoritative exhaustion", func(c *claudeBootstrapTransport) { c.fablePrimaryUsed = "1" }, 1, false, true},
		{"ordinary model headerless reply", func(c *claudeBootstrapTransport) { c.primaryEmpty = true }, 2, true, false},
		{"ordinary model rejects credential", func(c *claudeBootstrapTransport) { c.primaryStatus = http.StatusUnauthorized; c.primaryEmpty = true }, 2, true, false},
		{"ordinary model exhausted", func(c *claudeBootstrapTransport) { c.primaryStatus = http.StatusTooManyRequests; c.primaryUsed = "1" }, 2, false, true},
		{"normal OAuth usage already known", func(c *claudeBootstrapTransport) { c.usageStatus = http.StatusOK }, 1, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			transport := &claudeBootstrapTransport{usageStatus: http.StatusForbidden, fableStatus: http.StatusTooManyRequests,
				fableBody:     `{"type":"error","error":{"type":"rate_limit_error","details":{"error_code":"credits_required"}}}`,
				primaryStatus: http.StatusOK, primaryUsed: "0.1"}
			tc.change(transport)
			windows, _, err := (&AccountRef{}).FetchUsageWindowsCached(context.Background(), &http.Client{Transport: transport}, probeAccount())
			if (err != nil) != tc.wantError {
				t.Fatalf("windows=%+v err=%v wantError=%v", windows, err, tc.wantError)
			}
			if len(transport.models) != tc.wantModels {
				t.Fatalf("models=%v want %d probes", transport.models, tc.wantModels)
			}
			if tc.wantExhausted {
				if !fableProbeHasPrimaryWindows(windows) {
					t.Fatal("lost authoritative quota windows")
				}
				for _, w := range windows {
					if w.Feature == "" && w.UsedPercent != 100 {
						t.Fatalf("exhaustion cleared: %+v", w)
					}
				}
			}
		})
	}
}
