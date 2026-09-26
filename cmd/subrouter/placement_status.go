package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// placementStatusLines renders the daemon's placement counters as a compact
// table: one summary line per pool (last-hour placements and the busiest
// account's share, the herding signal) and one row per account with any
// activity. Nothing is printed before the daemon has placed or routed work.
func placementStatusLines(snapshot selectacct.PlacementStatsSnapshot, now time.Time) []string {
	active := make([]selectacct.AccountPlacementStats, 0, len(snapshot.Accounts))
	for _, acct := range snapshot.Accounts {
		if acct.Placements+acct.Routed+acct.Evictions+acct.CapacityMarks+acct.FailoverTotal() > 0 || acct.Sessions > 0 {
			active = append(active, acct)
		}
	}
	if len(active) == 0 {
		return nil
	}
	var lines []string
	header := "Placement"
	if !snapshot.Since.IsZero() {
		header += fmt.Sprintf(" since %s (%s ago)", snapshot.Since.Local().Format("Jan 2 15:04"), now.Sub(snapshot.Since).Round(time.Minute))
	}
	lines = append(lines, header)
	for _, pool := range snapshot.Pools {
		if pool.Placements == 0 {
			continue
		}
		name := string(pool.Provider)
		if pool.Pool != "" {
			name += "/" + pool.Pool
		}
		lines = append(lines, fmt.Sprintf("  %s last hour: %d placements across %d accounts, busiest %s %.0f%%",
			name, pool.Placements, pool.Accounts, displayAccountName(pool.BusiestAccountID), pool.BusiestShare*100))
	}
	var table strings.Builder
	w := tabwriter.NewWriter(&table, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  PROVIDER\tACCOUNT\tPLACED\tROUTED\tEVICTED\tFAILOVER usage/auth/cap\tCAP MARKS\tSESSIONS")
	for _, acct := range active {
		failovers := fmt.Sprintf("%d/%d/%d",
			acct.Failovers[selectacct.FailoverUsageLimit],
			acct.Failovers[selectacct.FailoverAuth],
			acct.Failovers[selectacct.FailoverCapacity])
		fmt.Fprintf(w, "  %s\t%s\t%d\t%d\t%d\t%s\t%d\t%d\n", acct.Provider, displayAccountName(acct.AccountID),
			acct.Placements, acct.Routed, acct.Evictions, failovers, acct.CapacityMarks, acct.Sessions)
	}
	_ = w.Flush()
	lines = append(lines, strings.Split(strings.TrimRight(table.String(), "\n"), "\n")...)
	return lines
}

// printPlacementStatus appends the daemon's placement distribution to sr
// status. Best effort: an older daemon without the endpoint, or any fetch
// failure, prints nothing.
func (r srRunner) printPlacementStatus(ctx context.Context, server srServerConfig) {
	baseURL, err := serverControlBaseURL(server)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+proxy.PlacementStatsPath, nil)
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
		_, _ = io.Copy(io.Discard, res.Body)
		return
	}
	var snapshot selectacct.PlacementStatsSnapshot
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&snapshot); err != nil {
		return
	}
	lines := placementStatusLines(snapshot, time.Now())
	if len(lines) == 0 {
		return
	}
	fmt.Fprintln(r.out)
	for _, line := range lines {
		fmt.Fprintln(r.out, line)
	}
}
