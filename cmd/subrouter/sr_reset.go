package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// reset implements `subrouter reset`: redeem a ChatGPT Pro rate-limit reset
// credit so a cooked account becomes usable again without waiting out the 7d
// window. With no arguments it targets the single best candidate (cooked, has a
// credit, longest natural reset remaining). --all sweeps every eligible
// account. <email> targets one account. --dry-run lists candidates without
// consuming a credit.
func (r srRunner) reset(ctx context.Context, args []string) error {
	return r.resetAgainstServer(ctx, args, nil)
}

// resetAgainstServer keeps a command already routed through the serving API
// bound to that exact daemon. Re-resolving the selected server here could send
// a one-time reset credit to a stale remote or the CLI's unrelated disk store.
func (r srRunner) resetAgainstServer(ctx context.Context, args []string, fixedServer *srServerConfig) error {
	flags := flag.NewFlagSet("reset", flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	flags.Usage = func() {
		fmt.Fprintln(r.errOut, "usage: subrouter reset [email] [--all [--yes]] [--gto [-n N]] [--candidates] [--min-wait D] [--dry-run] [--local]")
		fmt.Fprintln(r.errOut, "  Redeem a ChatGPT Pro rate-limit reset credit.")
		fmt.Fprintln(r.errOut, "  No args: reset the best cooked account with a credit available.")
		fmt.Fprintln(r.errOut, "  <email>: reset a specific account.")
		fmt.Fprintln(r.errOut, "  --all: reset every cooked account that has a credit. Shows the list and")
		fmt.Fprintln(r.errOut, "         asks first; --yes skips the question.")
		fmt.Fprintln(r.errOut, "  --candidates: list cooked accounts with a credit, by weekly wait and")
		fmt.Fprintln(r.errOut, "         credit expiry (no redeem).")
		fmt.Fprintln(r.errOut, "  --min-wait D: with no args, --all, or --candidates, skip accounts whose")
		fmt.Fprintln(r.errOut, "         weekly window resets on its own within D (e.g. 1d, 12h).")
		fmt.Fprintln(r.errOut, "  --gto: reset the account(s) routing would most benefit from un-cooking,")
		fmt.Fprintln(r.errOut, "         ranked by post-reset weekly headroom then downtime saved.")
		fmt.Fprintln(r.errOut, "  -n N:  with --gto, redeem the top N ranked accounts (default 1).")
		fmt.Fprintln(r.errOut, "  --list: show every account's available reset credits with expiry (no redeem).")
		fmt.Fprintln(r.errOut, "  --dry-run: list candidates and value verdict without redeeming.")
		fmt.Fprintln(r.errOut, "  --local: use tokens stored on this machine even when a server is configured.")
	}
	all := flags.Bool("all", false, "reset every cooked account that has an available credit")
	gto := flags.Bool("gto", false, "reset the game-theory-optimal account(s) to un-cook, by post-reset headroom then downtime saved")
	count := flags.Int("n", 1, "with --gto, how many top-ranked accounts to redeem")
	list := flags.Bool("list", false, "list every account's available reset credits with expiry (no redeem)")
	dryRun := flags.Bool("dry-run", false, "list eligible accounts without redeeming a credit")
	yes := flags.Bool("yes", false, "with --all, redeem without asking for confirmation")
	listCandidates := flags.Bool("candidates", false, "list reset candidates by weekly wait and credit expiry (no redeem)")
	minWaitRaw := flags.String("min-wait", "", "skip accounts whose weekly window resets on its own within this long, e.g. 1d or 12h")
	local := flags.Bool("local", false, "use tokens stored on this machine even when a server is configured")
	positional, err := parseFlagsAnywhere(flags, args)
	if err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if len(positional) > 1 {
		return fmt.Errorf("reset takes at most one account, got %d: %s", len(positional), strings.Join(positional, " "))
	}
	email := ""
	if len(positional) > 0 {
		email = strings.TrimSpace(positional[0])
	}
	if email != "" && *all {
		return fmt.Errorf("pass either an email or --all, not both")
	}
	if *list && (email != "" || *all || *gto) {
		return fmt.Errorf("--list only reports credits; do not combine it with an email, --all, or --gto")
	}
	if *gto && (email != "" || *all) {
		return fmt.Errorf("--gto selects candidates itself; do not combine it with an email or --all")
	}
	minWait, err := parseWaitDuration(*minWaitRaw)
	if err != nil {
		return fmt.Errorf("--min-wait: %w", err)
	}
	if *listCandidates && (email != "" || *all || *gto || *list) {
		return fmt.Errorf("--candidates lists accounts itself; do not combine it with an email, --all, --gto, or --list")
	}
	if minWait > 0 && (email != "" || *gto || *list) {
		return fmt.Errorf("--min-wait applies to the default pick, --all, and --candidates")
	}
	if *local && fixedServer != nil {
		return fmt.Errorf("--local cannot be used when this command is bound to a running server")
	}
	if !*gto && *count != 1 {
		return fmt.Errorf("-n only applies with --gto")
	}
	if *gto && *count < 1 {
		return fmt.Errorf("-n must be at least 1")
	}

	var server srServerConfig
	ok := false
	if fixedServer != nil {
		server = *fixedServer
		ok = true
	} else if !*local {
		server, ok, err = r.selectedRemoteServer()
		if err != nil {
			return err
		}
	}
	if *list {
		if ok {
			return r.resetListRemote(ctx, server)
		}
		return r.resetListLocal(ctx)
	}
	if *gto {
		if ok {
			return r.resetRemoteGTO(ctx, server, *count, *dryRun)
		}
		return r.resetLocalGTO(ctx, *count, *dryRun)
	}
	if *listCandidates {
		if ok {
			return r.resetRemoteCandidates(ctx, server, minWait)
		}
		return r.resetLocal(ctx, "", true, true, minWait, nil)
	}
	var confirm func(int) error
	if *all && !*dryRun {
		confirm = func(int) error { return nil }
		if !*yes {
			confirm = r.confirmResetAll
		}
	}
	if ok {
		if *all && !*dryRun {
			return r.resetRemoteAllConfirmed(ctx, server, minWait, confirm)
		}
		return r.resetRemote(ctx, server, email, *all, *dryRun, minWait)
	}
	return r.resetLocal(ctx, email, *all, *dryRun, minWait, confirm)
}

// parseWaitDuration parses a --min-wait value. It accepts Go durations plus
// a "d" suffix for days (1d, 1d12h), and returns whole seconds.
func parseWaitDuration(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	var days int64
	if i := strings.Index(raw, "d"); i >= 0 {
		n, err := strconv.ParseInt(raw[:i], 10, 64)
		if err != nil || n < 0 || n > 365 {
			return 0, fmt.Errorf("invalid duration %q", raw)
		}
		days = n
		raw = raw[i+1:]
	}
	var rest time.Duration
	if raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 {
			return 0, fmt.Errorf("invalid duration %q", raw)
		}
		rest = d
	}
	return days*24*60*60 + int64(rest/time.Second), nil
}

// confirmResetAll asks before --all spends n credits. Without an interactive
// input it refuses, so a script has to opt in with --yes.
func (r srRunner) confirmResetAll(n int) error {
	if !inputIsInteractive(r.in) {
		return fmt.Errorf("--all would redeem %d reset credit(s); re-run with --yes to confirm", n)
	}
	answer, err := promptLine(r.out, bufio.NewReader(r.in), fmt.Sprintf("Redeem %d reset credit(s)? [y/N]: ", n))
	if err != nil {
		return err
	}
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
		return fmt.Errorf("aborted; no credits redeemed")
	}
	return nil
}

// inputIsInteractive reports whether in can answer a prompt: a terminal,
// or a non-file reader supplied by a caller (tests). A pipe or /dev/null
// cannot, so scripts must pass --yes.
func inputIsInteractive(in io.Reader) bool {
	if in == nil {
		return false
	}
	if f, ok := in.(*os.File); ok {
		return term.IsTerminal(int(f.Fd()))
	}
	return true
}

// resetRemoteAllConfirmed previews --all as a dry run, confirms, then redeems
// exactly the previewed accounts one at a time. Redeeming by email rather
// than re-sending all=true means an older server that ignores min_wait can
// never spend more than the user was shown.
func (r srRunner) resetRemoteAllConfirmed(ctx context.Context, server srServerConfig, minWait int64, confirm func(int) error) error {
	preview, err := r.resetRemoteRequest(ctx, server, "", true, true, minWait)
	if err != nil {
		return err
	}
	if err := requireReportedWaits(server, minWait, preview.Results); err != nil {
		return err
	}
	var targets []string
	for _, res := range preview.Results {
		if !res.Eligible || res.Error != "" {
			continue
		}
		targets = append(targets, res.Email)
	}
	printResetCandidates(r.out, time.Now(), preview.Results)
	if len(targets) == 0 {
		return nil
	}
	if err := confirm(len(targets)); err != nil {
		return err
	}
	results := make([]remoteResetResult, 0, len(targets))
	reset := 0
	for _, email := range targets {
		payload, err := r.resetRemoteRequest(ctx, server, email, false, false, 0)
		if err != nil {
			results = append(results, remoteResetResult{Email: email, Error: err.Error()})
			continue
		}
		reset += payload.Reset
		results = append(results, payload.Results...)
	}
	printResetResults(r.out, false, reset, results)
	return nil
}

// requireReportedWaits refuses to act on a --min-wait sweep whose server
// did not report weekly waits: such a server ignored min_wait_seconds.
func requireReportedWaits(server srServerConfig, minWait int64, results []remoteResetResult) error {
	if minWait <= 0 {
		return nil
	}
	for _, res := range results {
		if res.Eligible && res.Error == "" && res.WeeklyWaitSeconds == 0 {
			return fmt.Errorf("server %s does not report weekly waits, so --min-wait cannot be applied; upgrade it or drop --min-wait", server.Name)
		}
	}
	return nil
}

// resetRemoteCandidates lists what --all would redeem, without redeeming.
func (r srRunner) resetRemoteCandidates(ctx context.Context, server srServerConfig, minWait int64) error {
	payload, err := r.resetRemoteRequest(ctx, server, "", true, true, minWait)
	if err != nil {
		return err
	}
	if err := requireReportedWaits(server, minWait, payload.Results); err != nil {
		return err
	}
	printResetCandidates(r.out, time.Now(), payload.Results)
	return nil
}

// printResetCandidates prints sweep candidates longest wait first with their
// soonest credit expiry, followed by any accounts the sweep could not read.
func printResetCandidates(out io.Writer, now time.Time, results []remoteResetResult) {
	var eligible, failed []remoteResetResult
	for _, res := range results {
		if res.Eligible && res.Error == "" {
			eligible = append(eligible, res)
		} else if res.Error != "" {
			failed = append(failed, res)
		}
	}
	if len(eligible) == 0 {
		fmt.Fprintln(out, "No accounts are eligible for a rate-limit reset.")
	} else {
		fmt.Fprintf(out, "%d account(s) eligible for a reset:\n", len(eligible))
		for _, res := range eligible {
			wait := "wait unknown"
			if res.WeeklyWaitSeconds > 0 {
				wait = "waits " + formatDuration(res.WeeklyWaitSeconds)
			}
			expiry := "no credit expiry reported"
			if t, err := time.Parse(time.RFC3339, res.CreditExpiresAt); err == nil {
				expiry = "credit " + formatExpiryFromNow(t, now)
			}
			fmt.Fprintf(out, "  %-32s %-14s %s\n", res.Email, wait, expiry)
		}
	}
	for _, res := range failed {
		fmt.Fprintf(out, "  %-32s error: %s\n", res.Email, res.Error)
	}
}

// resetRemote talks to the team server, which holds the OAuth tokens and runs
// the actual consume call against the wham API.
func (r srRunner) resetRemote(ctx context.Context, server srServerConfig, email string, all, dryRun bool, minWait int64) error {
	// No explicit target: pick the single smartest candidate from live usage so
	// the user does not have to know which account has the worst 7d window.
	if !all && email == "" {
		// Let the server pick: it applies its own eligibility rule, so the
		// account it chooses is never one it would then refuse.
		if minWait > 0 {
			return r.resetRemoteBestWithMinWait(ctx, server, dryRun, minWait)
		}
		payload, err := r.resetRemoteRequest(ctx, server, "", false, dryRun, minWait)
		if err == nil {
			if len(payload.Results) == 0 && !dryRun {
				return fmt.Errorf("no cooked account has a rate-limit reset credit available")
			}
			printResetResults(r.out, payload.DryRun, payload.Reset, payload.Results)
			return nil
		}
		if !serverLacksBestReset(err) {
			return err
		}
		// Older servers accept only email or all=true; pick client-side.
		candidate, err := r.pickSmartResetCandidateRemote(ctx, server, 0)
		if err != nil {
			return err
		}
		if candidate == "" {
			if dryRun {
				printResetResults(r.out, true, 0, nil)
				return nil
			}
			return fmt.Errorf("no cooked account has a rate-limit reset credit available")
		}
		email = candidate
	}

	target := email
	if all {
		target = ""
	}
	return r.resetRemoteSweep(ctx, server, target, all, dryRun, minWait)
}

// resetRemoteBestWithMinWait resolves the default pick as a dry run first
// and redeems the chosen account by email only after checking its reported
// wait itself. A server that ignores min_wait_seconds, or predates best=true,
// can then never spend a credit on an account --min-wait excludes.
func (r srRunner) resetRemoteBestWithMinWait(ctx context.Context, server srServerConfig, dryRun bool, minWait int64) error {
	var pick string
	preview, err := r.resetRemoteRequest(ctx, server, "", false, true, minWait)
	switch {
	case err == nil:
		for _, res := range preview.Results {
			if !res.Eligible || res.Error != "" {
				continue
			}
			if res.WeeklyWaitSeconds == 0 {
				return fmt.Errorf("server %s does not report weekly waits, so --min-wait cannot be applied; upgrade it or drop --min-wait", server.Name)
			}
			if res.WeeklyWaitSeconds >= minWait {
				pick = res.Email
			}
			break
		}
	case serverLacksBestReset(err):
		if pick, err = r.pickSmartResetCandidateRemote(ctx, server, minWait); err != nil {
			return err
		}
	default:
		return err
	}
	if pick == "" {
		if dryRun {
			printResetResults(r.out, true, 0, nil)
			return nil
		}
		return fmt.Errorf("no cooked account with a reset credit waits at least %s", formatDuration(minWait))
	}
	return r.resetRemoteSweep(ctx, server, pick, false, dryRun, 0)
}

// serverLacksBestReset reports the error an older server returns for a
// reset request that names neither an email nor all=true.
func serverLacksBestReset(err error) bool {
	return err != nil && strings.Contains(err.Error(), "email or all=true is required")
}

// remoteResetPayload mirrors the server's /_subrouter/rate-limit-reset JSON.
type remoteResetPayload struct {
	Reset   int                 `json:"reset"`
	DryRun  bool                `json:"dry_run"`
	Results []remoteResetResult `json:"results"`
}

// resetRemoteRequest performs one reset call against the server and returns the
// decoded payload without printing, so callers (sweep, GTO) can aggregate.
func (r srRunner) resetRemoteRequest(ctx context.Context, server srServerConfig, email string, all, dryRun bool, minWait int64) (remoteResetPayload, error) {
	baseURL, err := serverControlBaseURL(server)
	if err != nil {
		return remoteResetPayload{}, err
	}
	u := baseURL + "/_subrouter/rate-limit-reset?"
	q := url.Values{}
	if all {
		q.Set("all", "true")
	}
	if email != "" {
		q.Set("email", email)
	}
	if email == "" && !all {
		q.Set("best", "true")
	}
	if dryRun {
		q.Set("dry_run", "true")
	}
	if minWait > 0 {
		q.Set("min_wait_seconds", strconv.FormatInt(minWait, 10))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u+q.Encode(), nil)
	if err != nil {
		return remoteResetPayload{}, redactServerRequestError(err, server)
	}
	addServerAdminAuth(req, server)
	secured, err := r.securedRequestClientForServer(server, baseURL, 60*time.Second)
	if err != nil {
		return remoteResetPayload{}, err
	}
	res, err := secured.Do(req)
	if err != nil {
		return remoteResetPayload{}, redactServerRequestError(err, server)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		if len(body) == 0 {
			return remoteResetPayload{}, fmt.Errorf("rate-limit reset failed: %s", res.Status)
		}
		return remoteResetPayload{}, fmt.Errorf("rate-limit reset failed: %s\n%s", res.Status, bytes.TrimSpace(body))
	}
	var payload remoteResetPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return remoteResetPayload{}, fmt.Errorf("decode reset response: %w", err)
	}
	return payload, nil
}

func (r srRunner) resetRemoteSweep(ctx context.Context, server srServerConfig, email string, all, dryRun bool, minWait int64) error {
	payload, err := r.resetRemoteRequest(ctx, server, email, all, dryRun, minWait)
	if err != nil {
		return err
	}
	if err := requireReportedWaits(server, minWait, payload.Results); err != nil {
		return err
	}
	printResetResults(r.out, payload.DryRun, payload.Reset, payload.Results)
	return resetFailuresError(payload.Results)
}

// resetFailuresError turns per-account redemption failures into a non-zero
// exit. The results are already printed; this only reports how many failed.
func resetFailuresError(results []remoteResetResult) error {
	failed := 0
	for _, res := range results {
		if res.Error != "" {
			failed++
		}
	}
	if failed == 0 {
		return nil
	}
	return fmt.Errorf("%d of %d rate-limit reset(s) failed", failed, len(results))
}

// resetFetchFailures collects usage-fetch errors from a local sweep so they
// are printed instead of silently dropped, and fail the command when no
// account could be checked at all.
type resetFetchFailures struct {
	attempted int
	failed    int
}

func (f *resetFetchFailures) record(errOut io.Writer, email string, err error) {
	f.attempted++
	if err == nil {
		return
	}
	f.failed++
	fmt.Fprintf(errOut, "warning: %s: usage fetch failed: %v\n", email, err)
}

func (f resetFetchFailures) err() error {
	if f.attempted > 0 && f.failed == f.attempted {
		return fmt.Errorf("usage fetch failed for all %d account(s); nothing could be checked for a reset", f.attempted)
	}
	return nil
}

// pickSmartResetCandidateRemote fetches live usage from the server and returns
// the email of the single best reset candidate: cooked on the 7d window, with a
// credit available, and the longest natural reset remaining (biggest downtime
// win from redeeming now). Returns "" when nothing is eligible.
func (r srRunner) pickSmartResetCandidateRemote(ctx context.Context, server srServerConfig, minWait int64) (string, error) {
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
			if !isModelScopedWindow(w) && isLongQuotaWindow(w) && w.ResetAfterSeconds > resetAfter {
				resetAfter = w.ResetAfterSeconds
			}
		}
		if resetAfter < minWait {
			continue
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
// stored OAuth token (no server). minWait drops accounts that recover on their
// own sooner; confirm, when set, is asked before redeeming.
func (r srRunner) resetLocal(ctx context.Context, email string, all, dryRun bool, minWait int64, confirm func(int) error) error {
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
	var fetches resetFetchFailures
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
		if err != nil && email != "" {
			return fmt.Errorf("%s: %w", stored.Email, err)
		}
		fetches.record(r.errOut, stored.Email, err)
		if err != nil {
			continue
		}
		if !localRateLimitCooked(details) || !localRateLimitHasCredit(details) {
			if email != "" {
				return fmt.Errorf("%s is not eligible for a reset (cooked=%v, credit=%v)", stored.Email, localRateLimitCooked(details), localRateLimitHasCredit(details))
			}
			continue
		}
		if email == "" && longResetAfter(details.Windows) < minWait {
			continue
		}
		candidates = append(candidates, cand{account: account, before: details.Windows})
	}
	if email != "" && len(candidates) == 0 {
		return fmt.Errorf("account %s not found or not eligible", email)
	}
	if err := fetches.err(); err != nil {
		return err
	}
	if !all && email == "" && len(candidates) > 0 {
		// Single best candidate by longest 7d reset remaining.
		sort.Slice(candidates, func(i, j int) bool {
			return longResetAfter(candidates[i].before) > longResetAfter(candidates[j].before)
		})
		candidates = candidates[:1]
	}

	if !dryRun && confirm != nil && len(candidates) > 0 {
		preview := make([]remoteResetResult, 0, len(candidates))
		for _, c := range candidates {
			preview = append(preview, remoteResetResult{Email: c.account.Email, Eligible: true, WeeklyWaitSeconds: longResetAfter(c.before)})
		}
		printResetCandidates(r.out, time.Now(), preview)
		if err := confirm(len(candidates)); err != nil {
			return err
		}
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
	return resetFailuresError(results)
}

func longResetAfter(windows []accounts.UsageWindow) int64 {
	var max int64
	for _, w := range windows {
		if !isModelScopedWindow(w) && isLongQuotaWindow(w) && w.ResetAfterSeconds > max {
			max = w.ResetAfterSeconds
		}
	}
	return max
}

func localRateLimitCooked(details accounts.CodexUsageDetails) bool {
	return accounts.WeeklyLimitCooked(details)
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
	// WeeklyWaitSeconds and CreditExpiresAt come from newer servers only.
	WeeklyWaitSeconds int64  `json:"weekly_wait_seconds,omitempty"`
	CreditExpiresAt   string `json:"credit_expires_at,omitempty"`
	Error             string `json:"error,omitempty"`
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
