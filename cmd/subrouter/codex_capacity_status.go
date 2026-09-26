package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// codexCapacitySheddingView mirrors proxy.CodexSheddingState as the daemon
// reports it in /_subrouter/health.
type codexCapacitySheddingView struct {
	Model        string    `json:"model"`
	Tier         string    `json:"tier"`
	FailureRatio float64   `json:"failure_ratio"`
	Samples      int       `json:"samples"`
	Failures     int       `json:"failures"`
	Shedding     bool      `json:"shedding"`
	Since        time.Time `json:"since"`
	RetryBudget  string    `json:"retry_budget"`
}

// codexCapacityStatusLines renders one sr status line per (model, tier)
// pool that is currently shedding ("Selected model is at capacity" on most
// recent requests). Pools with a few scattered failures print nothing.
func codexCapacityStatusLines(states []codexCapacitySheddingView, now time.Time) []string {
	var lines []string
	for _, state := range states {
		if !state.Shedding {
			continue
		}
		since := ""
		if !state.Since.IsZero() {
			since = fmt.Sprintf(" for %s", now.Sub(state.Since).Round(time.Second))
		}
		budget := state.RetryBudget
		if budget == "" {
			budget = "3s"
		}
		lines = append(lines, fmt.Sprintf("Codex capacity        %s (%s tier) shedding%s · %.0f%% of %d recent attempts at capacity · quick retries capped at %s",
			state.Model, state.Tier, since, state.FailureRatio*100, state.Samples, budget))
	}
	return lines
}

// printCodexCapacityStatus appends the daemon's capacity shedding state to
// sr status. Best effort: an older daemon or an unreachable health endpoint
// prints nothing.
func (r srRunner) printCodexCapacityStatus(ctx context.Context, server srServerConfig) {
	baseURL, err := serverControlBaseURL(server)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/_subrouter/health", nil)
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
	var health struct {
		Shedding []codexCapacitySheddingView `json:"codex_capacity_shedding"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 256<<10)).Decode(&health); err != nil {
		return
	}
	lines := codexCapacityStatusLines(health.Shedding, time.Now())
	if len(lines) == 0 {
		return
	}
	fmt.Fprintln(r.out)
	for _, line := range lines {
		fmt.Fprintln(r.out, line)
	}
}
