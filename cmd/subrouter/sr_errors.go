package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/proxy"
)

type srErrorsOptions struct {
	provider string
	account  string
	since    string
	limit    int
	json     bool
}

func parseErrorsArgs(args []string) (srErrorsOptions, error) {
	opts := srErrorsOptions{since: "1h", limit: 30}
	for i := 0; i < len(args); i++ {
		value := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", args[i])
			}
			i++
			return args[i], nil
		}
		var err error
		switch args[i] {
		case "--json":
			opts.json = true
		case "--provider":
			opts.provider, err = value()
		case "--account":
			opts.account, err = value()
		case "--since":
			opts.since, err = value()
		case "--limit":
			var raw string
			if raw, err = value(); err == nil {
				opts.limit, err = strconv.Atoi(raw)
				if err != nil || opts.limit < 1 || opts.limit > 2000 {
					err = fmt.Errorf("--limit must be 1..2000")
				}
			}
		default:
			if strings.HasPrefix(args[i], "-") {
				return opts, fmt.Errorf("usage: sr errors [--provider claude|codex] [--account ID] [--since 1h] [--limit N] [--json]")
			}
			opts.provider = args[i]
		}
		if err != nil {
			return opts, err
		}
	}
	return opts, nil
}

// upstreamErrors prints recent upstream failures from the selected server, and the
// account pool as it stood at the newest failure of each provider.
func (r srRunner) upstreamErrors(ctx context.Context, args []string) error {
	opts, err := parseErrorsArgs(args)
	if err != nil {
		return err
	}
	server, err := r.errorsServer(ctx)
	if err != nil {
		return err
	}
	baseURL, err := serverControlBaseURL(server)
	if err != nil {
		return err
	}
	query := url.Values{"since": {opts.since}, "limit": {strconv.Itoa(opts.limit)}}
	if opts.provider != "" {
		query.Set("provider", opts.provider)
	}
	if opts.account != "" {
		query.Set("account", opts.account)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+proxy.ErrorEventsPath+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	addServerAdminAuth(req, server)
	secured, err := r.securedRequestClientForServer(server, baseURL, 15*time.Second)
	if err != nil {
		return err
	}
	res, err := secured.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return err
	}
	if res.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s does not record upstream errors yet; it needs a worker with %s", r.serverHeading(server), proxy.ErrorEventsPath)
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", res.Status, strings.TrimSpace(string(body)))
	}
	if opts.json {
		_, err = r.out.Write(body)
		return err
	}
	var report proxy.ErrorEventReport
	if err := json.Unmarshal(body, &report); err != nil {
		return err
	}
	printErrorReport(r.out, report, opts.since, time.Now())
	return nil
}

// errorsServer picks the server the way sr status does, and never starts a
// local daemon only to read an empty error log.
func (r srRunner) errorsServer(ctx context.Context) (srServerConfig, error) {
	config, err := cloudModeConfig()
	if err != nil {
		return srServerConfig{}, err
	}
	unavailable := fmt.Errorf("sr errors reads a subrouter server; this machine routes through %s", config.EffectiveCredentialSource())
	switch config.EffectiveCredentialSource() {
	case broker.CredentialSourceLegacy:
		if explicitLocalStateAuthority() {
			return srServerConfig{}, unavailable
		}
		if server, ok, err := r.defaultRemoteServer(); err != nil || ok {
			return server, err
		}
		if r.useServingAPI {
			return r.readyLocalServingServer(ctx, defaultDaemonStarter())
		}
	case broker.CredentialSourceLocal:
		if !explicitLocalStateAuthority() && r.useServingAPI {
			return r.readyLocalServingServer(ctx, defaultDaemonStarter())
		}
	}
	return srServerConfig{}, unavailable
}

func printErrorReport(out io.Writer, report proxy.ErrorEventReport, since string, now time.Time) {
	fmt.Fprintf(out, "Upstream errors in the last %s: %d\n", since, report.Summary.Total)
	if report.Summary.Total == 0 {
		return
	}
	fmt.Fprintf(out, "  by status:  %s\n", topCounts(report.Summary.ByStatus, 8))
	fmt.Fprintf(out, "  by type:    %s\n", topCounts(report.Summary.ByType, 8))
	fmt.Fprintf(out, "  by account: %s\n", topCounts(report.Summary.ByAccount, 8))
	fmt.Fprintln(out)
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "AGO\tPROVIDER\tACCOUNT\tSTATUS\tTYPE\tPOOL NEW/USABLE/ALL\tMESSAGE")
	for _, event := range report.Events {
		status := event.ErrorClass
		if event.Status != 0 {
			status = strconv.Itoa(event.Status)
		}
		errorType := event.ErrorType
		if errorType == "" {
			errorType = event.ErrorClass
		}
		pool := "-"
		if snapshot, ok := report.Snapshots[strconv.FormatUint(event.SnapshotID, 10)]; ok {
			pool = fmt.Sprintf("%d/%d/%d", snapshot.UsableNew, snapshot.Usable, snapshot.Total)
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", shortAgo(now.Sub(event.At)), event.Provider,
			event.Account, status, errorType, pool, truncateRunes(event.Message, 70))
	}
	_ = table.Flush()

	seen := map[string]bool{}
	for _, event := range report.Events {
		if seen[string(event.Provider)] {
			continue
		}
		seen[string(event.Provider)] = true
		snapshot, ok := report.Snapshots[strconv.FormatUint(event.SnapshotID, 10)]
		if !ok {
			continue
		}
		fmt.Fprintf(out, "\n%s pool at the newest error (%s ago): %d with room for a new session, %d usable, %d total\n",
			snapshot.Provider, shortAgo(now.Sub(snapshot.At)), snapshot.UsableNew, snapshot.Usable, snapshot.Total)
		table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(table, "ACCOUNT\tTIER\tLEFT\t5H\tWEEK\tSTATE\tIN FLIGHT")
		for _, state := range snapshot.Accounts {
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%d\n", state.Account, state.Tier,
				headroomCell(state.Headroom, state.Known), headroomCell(state.ShortHeadroom, state.Known),
				headroomCell(state.WeeklyHeadroom, state.Known), accountStateCell(state, now), state.Inflight)
		}
		_ = table.Flush()
	}
}

func headroomCell(value float64, known bool) string {
	if !known {
		return "?"
	}
	return fmt.Sprintf("%d%%", int(value*100+0.5))
}

func accountStateCell(state proxy.AccountState, now time.Time) string {
	switch {
	case state.Blocked && state.BlockedUntil != nil:
		return "blocked " + shortAgo(state.BlockedUntil.Sub(now))
	case state.Blocked:
		return "blocked"
	case state.Exhausted && state.ResetAt != nil:
		return "exhausted, resets in " + shortAgo(state.ResetAt.Sub(now))
	case state.Exhausted:
		return "exhausted"
	case state.UsableNew:
		return "ok"
	default:
		return "low"
	}
}

func topCounts(counts map[string]int, limit int) string {
	type entry struct {
		key   string
		count int
	}
	entries := make([]entry, 0, len(counts))
	for key, count := range counts {
		entries = append(entries, entry{key, count})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count != entries[j].count {
			return entries[i].count > entries[j].count
		}
		return entries[i].key < entries[j].key
	})
	parts := make([]string, 0, limit)
	for i, entry := range entries {
		if i == limit {
			parts = append(parts, fmt.Sprintf("+%d more", len(entries)-limit))
			break
		}
		parts = append(parts, fmt.Sprintf("%s ×%d", entry.key, entry.count))
	}
	return strings.Join(parts, ", ")
}

func shortAgo(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit-1]) + "…"
}
