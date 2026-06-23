package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// reset implements `subrouter reset`: redeem a ChatGPT Pro rate-limit reset
// credit so a cooked account becomes usable again without waiting out the 7d
// window. With no arguments it targets the single best candidate (cooked, has a
// credit, longest natural reset remaining). --all sweeps every eligible
// account. <email> targets one account. --dry-run lists candidates without
// consuming a credit.
func (r srRunner) reset(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("reset", flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	flags.Usage = func() {
		fmt.Fprintln(r.errOut, "usage: subrouter reset [email] [--all] [--dry-run]")
		fmt.Fprintln(r.errOut, "  Redeem a ChatGPT Pro rate-limit reset credit.")
		fmt.Fprintln(r.errOut, "  No args: reset the best cooked account with a credit available.")
		fmt.Fprintln(r.errOut, "  <email>: reset a specific account.")
		fmt.Fprintln(r.errOut, "  --all: reset every cooked account that has a credit.")
		fmt.Fprintln(r.errOut, "  --dry-run: list eligible accounts without redeeming.")
	}
	all := flags.Bool("all", false, "reset every cooked account that has an available credit")
	dryRun := flags.Bool("dry-run", false, "list eligible accounts without redeeming a credit")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	positional := flags.Args()
	email := ""
	if len(positional) > 0 {
		email = strings.TrimSpace(positional[0])
	}
	if email != "" && *all {
		return fmt.Errorf("pass either an email or --all, not both")
	}

	if server, ok, err := r.selectedRemoteServer(); err != nil {
		return err
	} else if ok {
		return r.resetRemote(ctx, server, email, *all, *dryRun)
	}
	return r.resetLocal(ctx, email, *all, *dryRun)
}

// resetRemote talks to the team server, which holds the OAuth tokens and runs
// the actual consume call against the wham API.
func (r srRunner) resetRemote(ctx context.Context, server srServerConfig, email string, all, dryRun bool) error {
	// No explicit target: pick the single smartest candidate from live usage so
	// the user does not have to know which account has the worst 7d window.
	if !all && email == "" {
		candidate, err := r.pickSmartResetCandidateRemote(ctx, server)
		if err != nil {
			return err
		}
		if candidate == "" {
			if dryRun {
				return r.resetRemoteSweep(ctx, server, "", false, true)
			}
			return fmt.Errorf("no cooked account has a rate-limit reset credit available")
		}
		email = candidate
	}

	target := email
	if all {
		target = ""
	}
	return r.resetRemoteSweep(ctx, server, target, all, dryRun)
}

func (r srRunner) resetRemoteSweep(ctx context.Context, server srServerConfig, email string, all, dryRun bool) error {
	u := server.URL + "/_subrouter/rate-limit-reset?"
	q := url.Values{}
	if all {
		q.Set("all", "true")
	}
	if email != "" {
		q.Set("email", email)
	}
	if dryRun {
		q.Set("dry_run", "true")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u+q.Encode(), nil)
	if err != nil {
		return err
	}
	addServerAdminAuth(req, server)
	client := r.client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		if len(body) == 0 {
			return fmt.Errorf("rate-limit reset failed: %s", res.Status)
		}
		return fmt.Errorf("rate-limit reset failed: %s\n%s", res.Status, bytes.TrimSpace(body))
	}
	var payload struct {
		Reset   int                 `json:"reset"`
		DryRun  bool                `json:"dry_run"`
		Results []remoteResetResult `json:"results"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("decode reset response: %w", err)
	}
	printResetResults(r.out, payload.DryRun, payload.Reset, payload.Results)
	return nil
}

// pickSmartResetCandidateRemote fetches live usage from the server and returns
// the email of the single best reset candidate: cooked on the 7d window, with a
// credit available, and the longest natural reset remaining (biggest downtime
// win from redeeming now). Returns "" when nothing is eligible.
func (r srRunner) pickSmartResetCandidateRemote(ctx context.Context, server srServerConfig) (string, error) {
	statuses, _, err := r.fetchServerUsageStatuses(ctx, server)
	if err != nil {
		return "", err
	}
	rows := usageRowsFromServerUsageStatuses(statuses)
	type cand struct {
		email      string
		resetAfter int64
	}
	candidates := make([]cand, 0, len(rows))
	for _, row := range rows {
		if row.authMode != accounts.AuthModeOAuth || row.provider != accounts.ProviderCodex {
			continue
		}
		if !row.cooked {
			continue
		}
		if row.complimentaryReset == nil || !row.complimentaryReset.Available {
			continue
		}
		var resetAfter int64
		for _, w := range row.windows {
			if isLongQuotaWindow(w) && w.ResetAfterSeconds > resetAfter {
				resetAfter = w.ResetAfterSeconds
			}
		}
		candidates = append(candidates, cand{email: row.email, resetAfter: resetAfter})
	}
	if len(candidates) == 0 {
		return "", nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].resetAfter > candidates[j].resetAfter
	})
	return candidates[0].email, nil
}

// resetLocal runs the redeem directly against the wham API using the locally
// stored OAuth token (no server).
func (r srRunner) resetLocal(ctx context.Context, email string, all, dryRun bool) error {
	storedAccounts, err := r.store.ListStored()
	if err != nil {
		return err
	}
	// Build candidates with a live usage fetch so local mode shares the same
	// eligibility rules as the server path.
	type cand struct {
		account accounts.Account
		before  []accounts.UsageWindow
	}
	candidates := make([]cand, 0, len(storedAccounts))
	for _, stored := range storedAccounts {
		if stored.IsAPIKey() {
			continue
		}
		if email != "" && stored.Email != email {
			continue
		}
		account, ok := stored.Account(stored.SourcePath(r.store))
		if !ok || account.Token == "" {
			continue
		}
		details, err := accounts.FetchCodexUsageDetails(ctx, r.client, account)
		if err != nil {
			if email != "" {
				return fmt.Errorf("%s: %w", stored.Email, err)
			}
			continue
		}
		if !localRateLimitCooked(details) || !localRateLimitHasCredit(details) {
			if email != "" {
				return fmt.Errorf("%s is not eligible for a reset (cooked=%v, credit=%v)", stored.Email, localRateLimitCooked(details), localRateLimitHasCredit(details))
			}
			continue
		}
		candidates = append(candidates, cand{account: account, before: details.Windows})
	}
	if email != "" && len(candidates) == 0 {
		return fmt.Errorf("account %s not found or not eligible", email)
	}
	if !all && email == "" && len(candidates) > 0 {
		// Single best candidate by longest 7d reset remaining.
		sort.Slice(candidates, func(i, j int) bool {
			return longResetAfter(candidates[i].before) > longResetAfter(candidates[j].before)
		})
		candidates = candidates[:1]
	}

	results := make([]remoteResetResult, 0, len(candidates))
	reset := 0
	for _, c := range candidates {
		res := remoteResetResult{Email: c.account.Email, Eligible: true, WindowsBefore: c.before, DryRun: dryRun}
		if dryRun {
			results = append(results, res)
			continue
		}
		credit, err := accounts.RedeemRateLimitReset(ctx, r.client, c.account)
		if err != nil {
			res.Error = err.Error()
			results = append(results, res)
			continue
		}
		res.Credit = &credit
		res.Reset = true
		if after, err := accounts.FetchCodexUsageDetails(ctx, r.client, c.account); err == nil {
			res.WindowsAfter = after.Windows
		}
		results = append(results, res)
		if res.Reset {
			reset++
		}
	}
	printResetResults(r.out, dryRun, reset, results)
	return nil
}

func longResetAfter(windows []accounts.UsageWindow) int64 {
	var max int64
	for _, w := range windows {
		if isLongQuotaWindow(w) && w.ResetAfterSeconds > max {
			max = w.ResetAfterSeconds
		}
	}
	return max
}

func localRateLimitCooked(details accounts.CodexUsageDetails) bool {
	if details.RawRateLimit.LimitReached {
		return true
	}
	if sw := details.RawRateLimit.SecondaryWindow; sw != nil && sw.UsedPercent >= 100 {
		return true
	}
	return false
}

func localRateLimitHasCredit(details accounts.CodexUsageDetails) bool {
	return details.ComplimentaryReset != nil && details.ComplimentaryReset.Available
}

// remoteResetResult mirrors the server's RateLimitResetResult JSON.
type remoteResetResult struct {
	Email         string                         `json:"email"`
	Eligible      bool                           `json:"eligible"`
	Reset         bool                           `json:"reset"`
	DryRun        bool                           `json:"dry_run,omitempty"`
	Credit        *accounts.RateLimitResetCredit `json:"credit,omitempty"`
	WindowsBefore []accounts.UsageWindow         `json:"windows_before,omitempty"`
	WindowsAfter  []accounts.UsageWindow         `json:"windows_after,omitempty"`
	Error         string                         `json:"error,omitempty"`
}

func printResetResults(out io.Writer, dryRun bool, resetCount int, results []remoteResetResult) {
	if len(results) == 0 {
		if dryRun {
			fmt.Fprintln(out, "No accounts are eligible for a rate-limit reset.")
		} else {
			fmt.Fprintln(out, "No rate-limit resets performed (no eligible accounts).")
		}
		return
	}
	verb := "Reset"
	if dryRun {
		verb = "Would reset"
	}
	fmt.Fprintf(out, "%s %d/%d accounts:\n", verb, resetCount, len(results))
	for _, res := range results {
		fmt.Fprintln(out, resetResultLine(res))
	}
}

func resetResultLine(res remoteResetResult) string {
	var b strings.Builder
	b.WriteString("  ")
	b.WriteString(res.Email)
	switch {
	case res.Error != "":
		b.WriteString(": ")
		b.WriteString(res.Error)
	case res.Reset:
		before := longUsageSummary(res.WindowsBefore)
		after := longUsageSummary(res.WindowsAfter)
		if before != "" || after != "" {
			fmt.Fprintf(&b, ": 7d %s -> %s", before, after)
		} else {
			b.WriteString(": reset")
		}
		if res.Credit != nil && res.Credit.Status != "" {
			fmt.Fprintf(&b, " (credit %s)", res.Credit.Status)
		}
	case res.DryRun:
		b.WriteString(": eligible ")
		b.WriteString(longUsageSummary(res.WindowsBefore))
	default:
		b.WriteString(": no action")
	}
	return b.String()
}

// longUsageSummary renders the 7d (secondary) window as "used%/resets-in".
func longUsageSummary(windows []accounts.UsageWindow) string {
	for _, w := range windows {
		if !isLongQuotaWindow(w) {
			continue
		}
		if w.ResetAfterSeconds > 0 {
			return fmt.Sprintf("%g%%/%s", w.UsedPercent, formatDuration(w.ResetAfterSeconds))
		}
		return fmt.Sprintf("%g%%", w.UsedPercent)
	}
	return "?"
}
