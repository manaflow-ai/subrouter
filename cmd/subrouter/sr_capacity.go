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

// fetchCodexCapacity reads the per-account Codex capacity report.
func (r srRunner) fetchCodexCapacity(ctx context.Context, server srServerConfig) (proxy.CodexCapacityReport, error) {
	var report proxy.CodexCapacityReport
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serverControlBaseURL(server)+"/_subrouter/codex-capacity", nil)
	if err != nil {
		return report, err
	}
	addServerAdminAuth(req, server)
	client := r.client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return report, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, res.Body)
		return report, fmt.Errorf("codex capacity fetch failed: %s", res.Status)
	}
	if err := json.NewDecoder(res.Body).Decode(&report); err != nil {
		return report, err
	}
	return report, nil
}

// printCodexCapacityStatus appends the accounts that hit "Selected model is
// at capacity" in the last 24h to `sr server status`. Rates are episodes
// (retries collapsed) over episodes plus completed turns.
func (r srRunner) printCodexCapacityStatus(ctx context.Context, server srServerConfig) {
	report, err := r.fetchCodexCapacity(ctx, server)
	if err != nil {
		return
	}
	rows := codexCapacityRowsWithFailures(report)
	if len(rows) == 0 {
		return
	}
	fmt.Fprintln(r.out)
	fmt.Fprintf(r.out, "Codex capacity (24h)  %d accounts hit capacity; retries within %s count once\n", len(rows), report.RetryWindow)
	const limit = 10
	printCodexCapacityRows(r.out, rows[:min(limit, len(rows))])
	if len(rows) > limit {
		fmt.Fprintf(r.out, "  … %d more: %s server capacity %s\n", len(rows)-limit, r.programOrSubrouter(), server.Name)
	}
}

// serverCapacity prints the full capacity table, or the raw report with --json.
func (r srRunner) serverCapacity(ctx context.Context, server srServerConfig, args []string) error {
	asJSON := false
	for _, arg := range args {
		switch arg {
		case "--json":
			asJSON = true
		default:
			return fmt.Errorf("usage: %s server capacity <name> [--json]", r.programOrSubrouter())
		}
	}
	report, err := r.fetchCodexCapacity(ctx, server)
	if err != nil {
		return err
	}
	if asJSON {
		encoder := json.NewEncoder(r.out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	fmt.Fprintf(r.out, "Server: %s (%s)\n", server.Name, server.URL)
	fmt.Fprintf(r.out, "Codex capacity report %s\n", report.GeneratedAt)
	fmt.Fprintf(r.out, "Retry window %s · rotation rule: %s\n", report.RetryWindow, report.PersistentRule)
	fmt.Fprintf(r.out, "Taint rule: %s\n", report.TaintRule)
	rows := codexCapacityRowsWithFailures(report)
	if len(rows) == 0 {
		fmt.Fprintln(r.out, "No Codex account hit capacity in the last 24h.")
		return nil
	}
	fmt.Fprintln(r.out)
	printCodexCapacityRows(r.out, rows)
	return nil
}

func codexCapacityRowsWithFailures(report proxy.CodexCapacityReport) []proxy.CodexCapacityAccountStats {
	rows := make([]proxy.CodexCapacityAccountStats, 0, len(report.Accounts))
	for _, row := range report.Accounts {
		if row.Last24h.Failures > 0 {
			rows = append(rows, row)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].Last24h.Episodes > rows[j].Last24h.Episodes
	})
	return rows
}

func printCodexCapacityRows(out io.Writer, rows []proxy.CodexCapacityAccountStats) {
	fmt.Fprintf(out, "  %-34s %-14s %-14s %-14s %s\n", "account", "15m", "1h", "24h", "state")
	for _, row := range rows {
		name := row.Label
		if name == "" {
			name = accountEmail(row.ID, row.Email)
		}
		if name == "" {
			name = row.ID
		}
		fmt.Fprintf(out, "  %-34s %-14s %-14s %-14s %s\n",
			truncateName(name, 34),
			codexCapacityWindowCell(row.Last15m),
			codexCapacityWindowCell(row.Last1h),
			codexCapacityWindowCell(row.Last24h),
			codexCapacityStateCell(row))
	}
}

// codexCapacityWindowCell renders "episodes/turns rate%"; the turn count is
// episodes plus completed turns, and retries add to neither.
func codexCapacityWindowCell(window proxy.CodexCapacityWindow) string {
	turns := window.Episodes + window.Successes
	if turns == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d %3.0f%%", window.Episodes, turns, 100*window.Rate)
}

func codexCapacityStateCell(row proxy.CodexCapacityAccountStats) string {
	var parts []string
	if until, err := time.Parse(time.RFC3339, row.TaintedUntil); err == nil && until.After(time.Now()) {
		parts = append(parts, "TAINTED out of pool "+time.Until(until).Truncate(time.Second).String())
	}
	if row.Persistent {
		parts = append(parts, "PERSISTENT")
	}
	if row.Streak > 0 {
		streak := fmt.Sprintf("streak %d", row.Streak)
		if row.Streak != row.StreakEpisodes {
			streak = fmt.Sprintf("streak %d (%d retries)", row.Streak, row.Streak-row.StreakEpisodes)
		}
		if since, err := time.Parse(time.RFC3339, row.StreakSince); err == nil {
			streak += " for " + time.Since(since).Truncate(time.Second).String()
		}
		parts = append(parts, streak)
	}
	if until, err := time.Parse(time.RFC3339, row.MarkedUntil); err == nil && until.After(time.Now()) {
		parts = append(parts, "held out "+time.Until(until).Truncate(time.Second).String())
	}
	if len(parts) == 0 {
		return "ok"
	}
	return strings.Join(parts, " · ")
}

func truncateName(name string, width int) string {
	if len(name) <= width {
		return name
	}
	if width <= 1 {
		return name[:width]
	}
	return name[:width-1] + "…"
}
