package main

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// gtoResetLowValueSeconds is the natural-recovery threshold below which a reset
// is judged low value: if every cooked account self-heals within this window,
// redeeming a one-time credit only saves a few minutes.
const gtoResetLowValueSeconds int64 = 20 * 60

// gtoResetCandidate is one cooked account that still holds a reset credit,
// scored for how worthwhile un-cooking it is right now.
type gtoResetCandidate struct {
	email string
	// postResetHeadroom is the fraction of the tightest weekly window still
	// free. A reset clears the short (5h) window, so this is what the account
	// has left to spend once it comes back. Higher is a stronger routing pick.
	postResetHeadroom float64
	// downtimeSavedSeconds is how long until the account would self-heal without
	// a reset (the latest reset among its currently-saturated windows). Higher
	// means the credit buys more.
	downtimeSavedSeconds int64
	// weeklyExhausted is true when a weekly window is itself maxed, so a 5h
	// reset may not fully un-cook the account.
	weeklyExhausted  bool
	creditsRemaining int
}

// gtoResetMetrics derives the reset-value signals from an account's windows.
// postResetHeadroom looks only at weekly (long) windows because the reset
// refreshes the short window; downtimeSaved is the latest reset across every
// saturated non-Spark window (when the account naturally becomes usable again).
func gtoResetMetrics(windows []accounts.UsageWindow) (postResetHeadroom float64, downtimeSaved int64, weeklyExhausted bool) {
	postResetHeadroom = 1.0
	for _, w := range windows {
		if isSparkWindow(w) {
			continue
		}
		used := clampUsagePercent(w.UsedPercent)
		if isLongQuotaWindow(w) {
			remaining := 1 - used/100
			if remaining < postResetHeadroom {
				postResetHeadroom = remaining
			}
			if used >= 100 {
				weeklyExhausted = true
			}
		}
		if used >= 100 && w.ResetAfterSeconds > downtimeSaved {
			downtimeSaved = w.ResetAfterSeconds
		}
	}
	return postResetHeadroom, downtimeSaved, weeklyExhausted
}

// complimentaryResetRemaining reports how many reset credits an account holds,
// treating an available-but-uncounted credit as one.
func complimentaryResetRemaining(info *accounts.ComplimentaryResetInfo) int {
	if info == nil || !info.Available {
		return 0
	}
	if info.Remaining != nil {
		return *info.Remaining
	}
	return 1
}

// gtoResetCandidates ranks the cooked, credit-holding Codex accounts by how much
// routing gains from un-cooking each one, and reports how many Codex accounts
// are already usable (so the caller can judge whether any reset is warranted).
func gtoResetCandidates(rows []srUsageRow) (usableNow int, candidates []gtoResetCandidate) {
	for _, row := range rows {
		if row.err != nil || usageProvider(row) != accounts.ProviderCodex || row.authMode != accounts.AuthModeOAuth {
			continue
		}
		if !row.cooked && !row.tempCooked {
			usableNow++
			continue
		}
		credits := complimentaryResetRemaining(row.complimentaryReset)
		if credits <= 0 {
			continue
		}
		headroom, downtime, weeklyExhausted := gtoResetMetrics(row.windows)
		candidates = append(candidates, gtoResetCandidate{
			email:                row.email,
			postResetHeadroom:    headroom,
			downtimeSavedSeconds: downtime,
			weeklyExhausted:      weeklyExhausted,
			creditsRemaining:     credits,
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		// A usable-after-reset account (weekly not maxed) beats one the reset
		// might not fully un-cook.
		if a.weeklyExhausted != b.weeklyExhausted {
			return !a.weeklyExhausted
		}
		if a.postResetHeadroom != b.postResetHeadroom {
			return a.postResetHeadroom > b.postResetHeadroom
		}
		if a.downtimeSavedSeconds != b.downtimeSavedSeconds {
			return a.downtimeSavedSeconds > b.downtimeSavedSeconds
		}
		return a.email < b.email
	})
	return usableNow, candidates
}

// assessResetValue turns the live picture into a one-line verdict plus a boolean
// worthwhile flag. It never blocks the reset (fast unblock is always allowed);
// it just tells the user whether the credit is well spent.
func assessResetValue(usableNow int, candidates []gtoResetCandidate) (verdict string, worthwhile bool) {
	if usableNow > 0 {
		return fmt.Sprintf("LOW VALUE: %d Codex account(s) already usable — no reset needed to unblock.", usableNow), false
	}
	if len(candidates) == 0 {
		return "No cooked Codex account holds a reset credit.", false
	}
	soonest := candidates[0].downtimeSavedSeconds
	for _, c := range candidates {
		if c.downtimeSavedSeconds < soonest {
			soonest = c.downtimeSavedSeconds
		}
	}
	if soonest > 0 && soonest < gtoResetLowValueSeconds {
		return fmt.Sprintf("LOW VALUE: every cooked account self-heals within %s — a reset saves little.", formatDuration(soonest)), false
	}
	if soonest <= 0 {
		return "GOOD VALUE: all Codex accounts cooked with no near-term natural reset.", true
	}
	return fmt.Sprintf("GOOD VALUE: all Codex accounts cooked; soonest natural recovery in %s.", formatDuration(soonest)), true
}

func printGTOCandidates(out io.Writer, candidates []gtoResetCandidate, total int) {
	fmt.Fprintf(out, "Top %d of %d reset candidate(s):\n", len(candidates), total)
	for i, c := range candidates {
		saved := "self-heals now"
		if c.downtimeSavedSeconds > 0 {
			saved = "saves " + formatDuration(c.downtimeSavedSeconds)
		}
		note := ""
		if c.weeklyExhausted {
			note = " (weekly maxed — reset may not fully un-cook)"
		}
		fmt.Fprintf(out, "  %d. %s: %d%% weekly headroom after reset, %s, %d credit(s) left%s\n",
			i+1, c.email, int(c.postResetHeadroom*100+0.5), saved, c.creditsRemaining, note)
	}
}

// resetRemoteGTO selects and redeems the game-theory-optimal reset target(s) via
// the team server. It always prints the value verdict first, then either the
// dry-run candidate list or the redeem results.
func (r srRunner) resetRemoteGTO(ctx context.Context, server srServerConfig, n int, dryRun bool) error {
	statuses, ok, err := r.fetchServerUsageStatuses(ctx, server)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("server %s does not expose usage-status; cannot compute --gto selection", server.Name)
	}
	rows := usageRowsFromServerUsageStatuses(statuses)
	usableNow, candidates := gtoResetCandidates(rows)
	verdict, _ := assessResetValue(usableNow, candidates)
	fmt.Fprintln(r.out, verdict)
	if len(candidates) == 0 {
		if dryRun {
			return nil
		}
		return fmt.Errorf("no cooked Codex account has a rate-limit reset credit available")
	}
	top := candidates
	if len(top) > n {
		top = top[:n]
	}
	if dryRun {
		printGTOCandidates(r.out, top, len(candidates))
		return nil
	}
	results := make([]remoteResetResult, 0, len(top))
	reset := 0
	for _, c := range top {
		payload, err := r.resetRemoteRequest(ctx, server, c.email, false, false)
		if err != nil {
			results = append(results, remoteResetResult{Email: c.email, Error: err.Error()})
			continue
		}
		results = append(results, payload.Results...)
		reset += payload.Reset
	}
	printResetResults(r.out, false, reset, results)
	return nil
}

// resetLocalGTO is the no-server path: it scores locally-stored accounts against
// live usage and redeems the top N directly through the wham API.
func (r srRunner) resetLocalGTO(ctx context.Context, n int, dryRun bool) error {
	storedAccounts, err := r.store.ListStored()
	if err != nil {
		return err
	}
	rows := make([]srUsageRow, 0, len(storedAccounts))
	accountByEmail := make(map[string]accounts.Account, len(storedAccounts))
	for _, stored := range storedAccounts {
		if stored.IsAPIKey() {
			continue
		}
		account, ok := stored.Account(stored.SourcePath(r.store))
		if !ok || account.Token == "" {
			continue
		}
		details, err := accounts.FetchCodexUsageDetails(ctx, r.client, account)
		if err != nil {
			continue
		}
		row := srUsageRow{
			email:              stored.Email,
			authMode:           accounts.AuthModeOAuth,
			provider:           accounts.ProviderCodex,
			windows:            details.Windows,
			complimentaryReset: details.ComplimentaryReset,
			score:              scoreFromWindows(stored.Email, details.Windows),
		}
		row.cooked, row.cookedReason = cookedFromWindows(details.Windows)
		row.tempCooked, row.tempCookedReason = tempCookedFromWindows(details.Windows)
		rows = append(rows, row)
		accountByEmail[stored.Email] = account
	}
	usableNow, candidates := gtoResetCandidates(rows)
	verdict, _ := assessResetValue(usableNow, candidates)
	fmt.Fprintln(r.out, verdict)
	if len(candidates) == 0 {
		if dryRun {
			return nil
		}
		return fmt.Errorf("no cooked Codex account has a rate-limit reset credit available")
	}
	top := candidates
	if len(top) > n {
		top = top[:n]
	}
	if dryRun {
		printGTOCandidates(r.out, top, len(candidates))
		return nil
	}
	results := make([]remoteResetResult, 0, len(top))
	reset := 0
	for _, c := range top {
		account := accountByEmail[c.email]
		res := remoteResetResult{Email: c.email, Eligible: true}
		if before, err := accounts.FetchCodexUsageDetails(ctx, r.client, account); err == nil {
			res.WindowsBefore = before.Windows
		}
		credit, err := accounts.RedeemRateLimitReset(ctx, r.client, account)
		if err != nil {
			res.Error = err.Error()
			results = append(results, res)
			continue
		}
		res.Credit = &credit
		res.Reset = true
		if after, err := accounts.FetchCodexUsageDetails(ctx, r.client, account); err == nil {
			res.WindowsAfter = after.Windows
		}
		results = append(results, res)
		reset++
	}
	printResetResults(r.out, false, reset, results)
	return nil
}
