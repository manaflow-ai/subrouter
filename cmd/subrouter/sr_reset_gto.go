package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// resetCreditsAccount mirrors the server's /_subrouter/reset-credits entry.
type resetCreditsAccount struct {
	Email   string                          `json:"email"`
	Count   int                             `json:"count"`
	Credits []accounts.RateLimitResetCredit `json:"credits,omitempty"`
	Error   string                          `json:"error,omitempty"`
}

// soonestCreditExpiry returns the earliest expiry across a set of credits.
func soonestCreditExpiry(credits []accounts.RateLimitResetCredit) (time.Time, bool) {
	var soonest time.Time
	found := false
	for _, c := range credits {
		if c.ExpiresAt == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, c.ExpiresAt)
		if err != nil {
			continue
		}
		if !found || t.Before(soonest) {
			soonest = t
			found = true
		}
	}
	return soonest, found
}

// formatExpiryFromNow renders how long until expiry relative to now.
func formatExpiryFromNow(expiry, now time.Time) string {
	d := expiry.Sub(now)
	if d <= 0 {
		return "expired"
	}
	return "expires in " + formatDuration(int64(d/time.Second))
}

func printResetCredits(out io.Writer, now time.Time, entries []resetCreditsAccount) {
	total := 0
	withCredits := 0
	for _, e := range entries {
		total += e.Count
		line := fmt.Sprintf("  %-28s %d credit(s)", e.Email, e.Count)
		if e.Error != "" {
			line = fmt.Sprintf("  %-28s error: %s", e.Email, e.Error)
		} else if e.Count == 0 {
			continue
		} else {
			withCredits++
			if expiry, ok := soonestCreditExpiry(e.Credits); ok {
				line += ", soonest " + formatExpiryFromNow(expiry, now)
			} else {
				line += ", no expiry reported"
			}
		}
		fmt.Fprintln(out, line)
	}
	fmt.Fprintf(out, "%d reset credit(s) across %d account(s).\n", total, withCredits)
}

// resetListRemote fetches per-account reset-credit detail (count + expiry) from
// the team server, which alone holds live tokens to query the upstream.
func (r srRunner) resetListRemote(ctx context.Context, server srServerConfig) error {
	baseURL, err := serverControlBaseURL(server)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/_subrouter/reset-credits", nil)
	if err != nil {
		return redactServerRequestError(err, server)
	}
	addServerAdminAuth(req, server)
	secured, err := r.securedRequestClientForServer(server, baseURL, 60*time.Second)
	if err != nil {
		return err
	}
	res, err := secured.Do(req)
	if err != nil {
		return redactServerRequestError(err, server)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return fmt.Errorf("server %s does not expose /_subrouter/reset-credits yet; redeploy it to use --list", server.Name)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("reset-credits failed: %s", res.Status)
	}
	var payload struct {
		Accounts []resetCreditsAccount `json:"accounts"`
	}
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		return fmt.Errorf("decode reset-credits response: %w", err)
	}
	sort.Slice(payload.Accounts, func(i, j int) bool { return payload.Accounts[i].Email < payload.Accounts[j].Email })
	printResetCredits(r.out, time.Now(), payload.Accounts)
	return nil
}

// resetListLocal lists reset credits using locally stored tokens (no server).
func (r srRunner) resetListLocal(ctx context.Context) error {
	storedAccounts, err := r.store.ListStored()
	if err != nil {
		return err
	}
	entries := make([]resetCreditsAccount, 0, len(storedAccounts))
	for _, stored := range storedAccounts {
		if stored.IsAPIKey() {
			continue
		}
		account, ok := stored.Account(stored.SourcePath(r.store))
		if !ok || account.Token == "" {
			continue
		}
		entry := resetCreditsAccount{Email: stored.Email}
		credits, err := accounts.ListRateLimitResetCredits(ctx, r.client, account)
		if err != nil {
			entry.Error = err.Error()
		} else {
			for _, c := range credits {
				if c.Status == "" || c.Status == "available" {
					entry.Credits = append(entry.Credits, c)
				}
			}
			entry.Count = len(entry.Credits)
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Email < entries[j].Email })
	printResetCredits(r.out, time.Now(), entries)
	return nil
}

// A redeemed credit restarts the account's weekly window, so every later
// reset arrives sooner by the wait it skips: a credit is worth
// weeklyWait/7d of a window. It does nothing for a short (5h) window, and
// spending it on an account whose weekly window is not full throws away
// what is left of that window.
const (
	gtoWeekSeconds int64 = 7 * 24 * 60 * 60
	// gtoResetLowValueWait: below it a credit buys under 4% of a window.
	gtoResetLowValueWait int64 = 6 * 60 * 60
	// gtoResetGoodWait: the account cooked early in its window, the most a
	// credit is usually worth.
	gtoResetGoodWait int64 = 4 * 24 * 60 * 60
)

// gtoResetCandidate is one weekly-cooked account that still holds a credit.
type gtoResetCandidate struct {
	email             string
	weeklyWaitSeconds int64
	creditsRemaining  int
}

// windowValue is the fraction of a weekly window the credit buys.
func (c gtoResetCandidate) windowValue() float64 {
	if c.weeklyWaitSeconds >= gtoWeekSeconds {
		return 1
	}
	return float64(c.weeklyWaitSeconds) / float64(gtoWeekSeconds)
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

// gtoResetCandidates ranks weekly-cooked, credit-holding Codex accounts by
// what a credit is worth on each (the longest weekly wait first) and reports
// how many Codex accounts are usable now. Accounts blocked only by the 5h
// window are neither: a credit would restart their weekly window and waste
// what is left of it.
func gtoResetCandidates(rows []srUsageRow) (usableNow int, candidates []gtoResetCandidate) {
	for _, row := range rows {
		if row.err != nil || usageProvider(row) != accounts.ProviderCodex || row.authMode != accounts.AuthModeOAuth {
			continue
		}
		if !row.cooked {
			if !row.tempCooked {
				usableNow++
			}
			continue
		}
		credits := complimentaryResetRemaining(row.complimentaryReset)
		if credits <= 0 {
			continue
		}
		candidates = append(candidates, gtoResetCandidate{
			email:             row.email,
			weeklyWaitSeconds: accounts.WeeklyResetWait(row.windows),
			creditsRemaining:  credits,
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.weeklyWaitSeconds != b.weeklyWaitSeconds {
			return a.weeklyWaitSeconds > b.weeklyWaitSeconds
		}
		return a.email < b.email
	})
	return usableNow, candidates
}

// assessResetValue turns the best candidate's weekly wait into a one-line
// verdict plus a worthwhile flag. It never blocks the reset.
func assessResetValue(usableNow int, candidates []gtoResetCandidate) (verdict string, worthwhile bool) {
	if len(candidates) == 0 {
		return "No weekly-cooked Codex account holds a reset credit.", false
	}
	top := candidates[0]
	usable := ""
	if usableNow > 0 {
		usable = fmt.Sprintf(" %d Codex account(s) are still usable.", usableNow)
	}
	percent := int(top.windowValue()*100 + 0.5)
	switch {
	case top.weeklyWaitSeconds <= 0:
		return "UNKNOWN VALUE: the best candidate reports no weekly reset time." + usable, true
	case top.weeklyWaitSeconds < gtoResetLowValueWait:
		return fmt.Sprintf("LOW VALUE: the best candidate's weekly window resets on its own in %s; a credit buys %d%% of a window.%s",
			formatDuration(top.weeklyWaitSeconds), percent, usable), false
	case top.weeklyWaitSeconds < gtoResetGoodWait:
		return fmt.Sprintf("FAIR VALUE: a credit buys %d%% of a window (weekly resets in %s).%s",
			percent, formatDuration(top.weeklyWaitSeconds), usable), true
	default:
		return fmt.Sprintf("GOOD VALUE: a credit buys %d%% of a window (weekly resets in %s).%s",
			percent, formatDuration(top.weeklyWaitSeconds), usable), true
	}
}

func printGTOCandidates(out io.Writer, candidates []gtoResetCandidate, total int) {
	fmt.Fprintf(out, "Top %d of %d reset candidate(s):\n", len(candidates), total)
	for i, c := range candidates {
		wait := "weekly reset time unknown"
		if c.weeklyWaitSeconds > 0 {
			wait = "weekly resets in " + formatDuration(c.weeklyWaitSeconds)
		}
		fmt.Fprintf(out, "  %d. %s: %s, credit buys %d%% of a window, %d credit(s) left\n",
			i+1, c.email, wait, int(c.windowValue()*100+0.5), c.creditsRemaining)
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
		payload, err := r.resetRemoteRequest(ctx, server, c.email, false, false, 0)
		if err != nil {
			results = append(results, remoteResetResult{Email: c.email, Error: err.Error()})
			continue
		}
		results = append(results, payload.Results...)
		reset += payload.Reset
	}
	printResetResults(r.out, false, reset, results)
	return resetFailuresError(results)
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
	var fetches resetFetchFailures
	for _, stored := range storedAccounts {
		if stored.IsAPIKey() {
			continue
		}
		account, ok := stored.Account(stored.SourcePath(r.store))
		if !ok || account.Token == "" {
			continue
		}
		details, err := accounts.FetchCodexUsageDetails(ctx, r.client, account)
		fetches.record(r.errOut, stored.Email, err)
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
	if err := fetches.err(); err != nil {
		return err
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
		// Re-check live usage right before spending: the ranking came from a
		// fetch that may be minutes old, and redeeming an account whose
		// weekly window is no longer full wastes what is left of it.
		before, err := accounts.FetchCodexUsageDetails(ctx, r.client, account)
		if err != nil {
			res.Eligible = false
			res.Error = "usage fetch failed: " + err.Error()
			results = append(results, res)
			continue
		}
		res.WindowsBefore = before.Windows
		if !accounts.WeeklyLimitCooked(before) {
			res.Eligible = false
			res.Error = "account is no longer weekly-cooked; skipping"
			results = append(results, res)
			continue
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
	return resetFailuresError(results)
}
