package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// tokenUsageStatusSummary is one provider's token usage over the status
// window, summed from /_subrouter/token-usage rows.
type tokenUsageStatusSummary struct {
	provider          string
	requests          int64
	inputTokens       int64
	cachedInputTokens int64
	switches          int64
	switchInputTokens int64
	switchesInRequest int64
	ttfbBuckets       []int64
	ttfbMaxMs         int64
	ttfbCount         int64
	upstreamErrors    int64
}

// tokenUsageStatusLines renders one sr status line per provider: turns,
// input tokens and the cached share, account switches with the input tokens
// they sent without a warm cache, the estimated p95 time to first byte, and
// upstream errors. A daemon too old to report switches and latency prints
// the token part only.
func tokenUsageStatusLines(rows []proxy.TokenUsageRow, window string) []string {
	byProvider := map[string]*tokenUsageStatusSummary{}
	for _, row := range rows {
		summary := byProvider[row.Provider]
		if summary == nil {
			summary = &tokenUsageStatusSummary{provider: row.Provider}
			byProvider[row.Provider] = summary
		}
		summary.requests += row.Requests
		summary.inputTokens += row.InputTokens
		summary.cachedInputTokens += row.CachedInputTokens
		summary.switches += row.AccountSwitches
		summary.switchInputTokens += row.AccountSwitchInputTokens
		summary.switchesInRequest += row.AccountSwitchesInRequest
		summary.ttfbCount += row.TTFBCount
		if row.TTFBMsMax > summary.ttfbMaxMs {
			summary.ttfbMaxMs = row.TTFBMsMax
		}
		for index, count := range row.TTFBMsBuckets {
			for len(summary.ttfbBuckets) <= index {
				summary.ttfbBuckets = append(summary.ttfbBuckets, 0)
			}
			summary.ttfbBuckets[index] += count
		}
		for _, count := range row.UpstreamErrors {
			summary.upstreamErrors += count
		}
	}
	providers := make([]string, 0, len(byProvider))
	for provider := range byProvider {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	var lines []string
	for _, provider := range providers {
		summary := byProvider[provider]
		if summary.requests == 0 && summary.upstreamErrors == 0 {
			continue
		}
		parts := []string{fmt.Sprintf("%s %d turns", provider, summary.requests)}
		if summary.inputTokens > 0 {
			parts = append(parts, fmt.Sprintf("%s in, %.0f%% cached", fmtTokens(summary.inputTokens),
				100*float64(summary.cachedInputTokens)/float64(summary.inputTokens)))
		}
		// Latency counts mean the daemon also reports switches, so zero
		// switches is a measurement rather than an old daemon.
		if summary.switches > 0 || summary.ttfbCount > 0 {
			text := fmt.Sprintf("%d account switches", summary.switches)
			if summary.switches > 0 {
				text += fmt.Sprintf(" (%d mid-request), %s input tokens sent cold", summary.switchesInRequest, fmtTokens(summary.switchInputTokens))
			}
			parts = append(parts, text)
		}
		if p95, ok := proxy.TokenUsageLatencyQuantileMs(summary.ttfbBuckets, summary.ttfbMaxMs, 0.95); ok {
			parts = append(parts, "TTFB p95 "+formatTokenUsageLatency(p95))
		}
		if summary.upstreamErrors > 0 {
			parts = append(parts, fmt.Sprintf("%d upstream errors", summary.upstreamErrors))
		}
		label := fmt.Sprintf("Token usage (%s)", window)
		if len(lines) > 0 {
			label = ""
		}
		lines = append(lines, fmt.Sprintf("%-22s%s", label, strings.Join(parts, " · ")))
	}
	return lines
}

// formatTokenUsageLatency shows a bucket bound compactly: 500ms, 4s, 2.1m.
func formatTokenUsageLatency(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%gs", float64(ms/100)/10)
	default:
		return fmt.Sprintf("%.1fm", float64(ms)/60_000)
	}
}

// printTokenUsageStatus appends the server's last 24h of token usage to sr
// status. Best effort: an older daemon, a missing endpoint, or no traffic
// prints nothing.
func (r srRunner) printTokenUsageStatus(ctx context.Context, server srServerConfig) {
	baseURL, err := serverControlBaseURL(server)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/_subrouter/token-usage?since=24h", nil)
	if err != nil {
		return
	}
	addServerAdminAuth(req, server)
	secured, err := r.securedRequestClientForServer(server, baseURL, 10*time.Second)
	if err != nil {
		return
	}
	res, err := secured.Do(req)
	if err != nil {
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return
	}
	var usage struct {
		Rows []proxy.TokenUsageRow `json:"rows"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(&usage); err != nil {
		return
	}
	lines := tokenUsageStatusLines(usage.Rows, "24h")
	if len(lines) == 0 {
		return
	}
	fmt.Fprintln(r.out)
	for _, line := range lines {
		fmt.Fprintln(r.out, line)
	}
}
