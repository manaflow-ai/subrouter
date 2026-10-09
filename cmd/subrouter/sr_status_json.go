package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// srStatusOptions carries the flags `sr status` accepts.
type srStatusOptions struct {
	json bool
}

// Account states in `sr status --json`. Each is derived from the State and
// Use columns of the text table, so the two views never disagree.
const (
	srStatusStateError     = "error"
	srStatusStateCooked    = "cooked"
	srStatusStateTemp      = "temp"
	srStatusStateActive    = "active"
	srStatusStateRec       = "rec"
	srStatusStateProtected = "protected"
	// srStatusStateReady is an account the text table shows with no state
	// marker: usable, not recommended, not protected.
	srStatusStateReady     = "ready"
	srStatusStateThrottled = "throttled"
)

const srStatusJSONSchemaVersion = 1

type srStatusJSON struct {
	SchemaVersion int    `json:"schema_version"`
	GeneratedAt   string `json:"generated_at"`
	// Server is the configured server name that answered, or null when the
	// status came from local state or the hosted service.
	Server    *string                          `json:"server"`
	Providers map[string]*srStatusProviderJSON `json:"providers"`
}

type srStatusProviderJSON struct {
	Accounts []srStatusAccountJSON `json:"accounts"`
	Summary  srStatusSummaryJSON   `json:"summary"`
}

type srStatusAccountJSON struct {
	ID             string   `json:"id"`
	Label          string   `json:"label"`
	Provider       string   `json:"provider"`
	Plan           *string  `json:"plan"`
	State          string   `json:"state"`
	SessionLeftPct *float64 `json:"session_left_pct"`
	SessionResetAt *string  `json:"session_reset_at"`
	WeeklyLeftPct  *float64 `json:"weekly_left_pct"`
	WeeklyResetAt  *string  `json:"weekly_reset_at"`
	// ExtraUsageUSD is the Claude extra-usage money still available: the
	// prepaid credit balance when known, else the monthly limit minus spend.
	ExtraUsageUSD *float64 `json:"extra_usage_usd"`
	// CostTier is the server's Claude cost tier ("plan", "api-credits",
	// "extra-usage"), omitted for other providers and older servers.
	CostTier *string `json:"cost_tier,omitempty"`
	// CreditsExhaustedUntil is when a Claude API key held out for a spent
	// credit balance will be probed again.
	CreditsExhaustedUntil *string `json:"credits_exhausted_until,omitempty"`
}

type srStatusSummaryJSON struct {
	// Usable counts accounts that are not cooked, temp, or error, the same
	// test the text table's State column supports.
	Usable int `json:"usable"`
	Total  int `json:"total"`
	// WeeklyLeftSumPct adds weekly_left_pct over usable accounts; 100 is one
	// full account-week.
	WeeklyLeftSumPct float64 `json:"weekly_left_sum_pct"`
}

func (r srRunner) parseStatusArgs(args []string) (srStatusOptions, error) {
	usage := r.programOrSubrouter() + " status [--json]"
	flags := flag.NewFlagSet(r.programOrSubrouter()+" status", flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	jsonOutput := flags.Bool("json", false, "print machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return srStatusOptions{}, err
	}
	if flags.NArg() != 0 {
		return srStatusOptions{}, errors.New("usage: " + usage)
	}
	return srStatusOptions{json: *jsonOutput}, nil
}

// srStatusAccountState maps a row to one JSON state. The text table can show
// several markers at once ("active, cooked"); the most limiting one wins, so
// error > cooked > temp > active > rec > protected > ready.
func srStatusAccountState(row srUsageRow) string {
	markers := map[string]bool{}
	for _, part := range strings.Split(usageGridState(row), ",") {
		for _, word := range strings.Fields(part) {
			markers[word] = true
		}
	}
	switch {
	case markers["error"] || markers["revoked"] || srStatusProviderHealthFailed(row):
		return srStatusStateError
	case markers["cooked"] || markers["exhausted"]:
		return srStatusStateCooked
	case markers["temp"]:
		return srStatusStateTemp
	case markers["active"]:
		return srStatusStateActive
	case markers["rec"]:
		return srStatusStateRec
	case markers["throttled"]:
		return srStatusStateThrottled
	case strings.Contains(compactPickReason(row), "protected <"):
		return srStatusStateProtected
	default:
		return srStatusStateReady
	}
}

// srStatusProviderHealthFailed reports a key validation failure that the
// State column prints as the raw health text ("bad key", "auth error") and
// colors red.
func srStatusProviderHealthFailed(row srUsageRow) bool {
	switch row.providerHealth {
	case "", "auth ok", "ok", "not checked", "stored":
		return false
	default:
		return true
	}
}

func srStatusUsableState(state string) bool {
	switch state {
	case srStatusStateCooked, srStatusStateTemp, srStatusStateError:
		return false
	default:
		return true
	}
}

// srStatusQuotaWindows picks the windows behind the Session/5h and
// Weekly/7d columns of the text table.
func srStatusQuotaWindows(row srUsageRow) (session, weekly *accounts.UsageWindow) {
	sessionMatch := accountWideWindow(isShortQuotaWindow)
	weeklyMatch := accountWideWindow(isLongQuotaWindow)
	if usageProvider(row) == accounts.ProviderClaude {
		sessionMatch = isClaudeSessionWindow
		weeklyMatch = isClaudeWeeklyWindow
	}
	for i := range row.windows {
		if session == nil && sessionMatch(row.windows[i]) {
			session = &row.windows[i]
		}
		if weekly == nil && weeklyMatch(row.windows[i]) {
			weekly = &row.windows[i]
		}
	}
	return session, weekly
}

func srStatusLeftPct(window *accounts.UsageWindow) *float64 {
	if window == nil {
		return nil
	}
	left := roundHundredths(100 - clampUsagePercent(window.UsedPercent))
	return &left
}

// srStatusResetAt returns the window's reset as RFC3339 UTC. Rows are
// re-anchored to now when fetched, so a window without an absolute reset
// time resets ResetAfterSeconds after now.
func srStatusResetAt(window *accounts.UsageWindow, now time.Time) *string {
	if window == nil {
		return nil
	}
	reset := window.ResetTime(now)
	if reset.IsZero() {
		return nil
	}
	text := reset.UTC().Truncate(time.Second).Format(time.RFC3339)
	return &text
}

func srStatusExtraUsageUSD(row srUsageRow) *float64 {
	if usageProvider(row) != accounts.ProviderClaude {
		return nil
	}
	extra := claudeExtraUsageForRow(row)
	if extra == nil {
		return nil
	}
	if extra.CreditsBalance != nil {
		usd := roundHundredths(*extra.CreditsBalance / 100)
		return &usd
	}
	if !extra.IsEnabled {
		return nil
	}
	remaining, ok := extra.Remaining()
	if !ok {
		return nil
	}
	usd := roundHundredths(remaining / 100)
	return &usd
}

func srStatusPlan(row srUsageRow) *string {
	plan := strings.TrimSpace(usageGridPlan(row))
	if plan == "" || strings.EqualFold(plan, "unknown") {
		return nil
	}
	return &plan
}

func roundHundredths(value float64) float64 {
	return math.Round(value*100) / 100
}

func srStatusAccountFromRow(row srUsageRow, now time.Time) srStatusAccountJSON {
	id := strings.TrimSpace(row.accountID)
	if id == "" {
		id = row.email
	}
	session, weekly := srStatusQuotaWindows(row)
	var costTier, creditsExhaustedUntil *string
	if row.costTier != "" {
		tier := row.costTier
		costTier = &tier
	}
	if !row.creditsExhaustedUntil.IsZero() {
		until := row.creditsExhaustedUntil.UTC().Format(time.RFC3339)
		creditsExhaustedUntil = &until
	}
	return srStatusAccountJSON{
		ID:             id,
		Label:          displayUsageAccountName(row),
		Provider:       string(usageProvider(row)),
		Plan:           srStatusPlan(row),
		State:          srStatusAccountState(row),
		SessionLeftPct: srStatusLeftPct(session),
		SessionResetAt: srStatusResetAt(session, now),
		WeeklyLeftPct:  srStatusLeftPct(weekly),
		WeeklyResetAt:  srStatusResetAt(weekly, now),
		ExtraUsageUSD:  srStatusExtraUsageUSD(row),

		CostTier:              costTier,
		CreditsExhaustedUntil: creditsExhaustedUntil,
	}
}

// buildSRStatusJSON groups rows by provider in their display order.
func buildSRStatusJSON(rows []srUsageRow, server string, now time.Time) srStatusJSON {
	result := srStatusJSON{
		SchemaVersion: srStatusJSONSchemaVersion,
		GeneratedAt:   now.UTC().Truncate(time.Second).Format(time.RFC3339),
		Providers:     map[string]*srStatusProviderJSON{},
	}
	if server != "" {
		result.Server = &server
	}
	for _, row := range rows {
		account := srStatusAccountFromRow(row, now)
		provider := result.Providers[account.Provider]
		if provider == nil {
			provider = &srStatusProviderJSON{Accounts: []srStatusAccountJSON{}}
			result.Providers[account.Provider] = provider
		}
		provider.Accounts = append(provider.Accounts, account)
		provider.Summary.Total++
		if !srStatusUsableState(account.State) {
			continue
		}
		provider.Summary.Usable++
		if account.WeeklyLeftPct != nil {
			provider.Summary.WeeklyLeftSumPct = roundHundredths(provider.Summary.WeeklyLeftSumPct + *account.WeeklyLeftPct)
		}
	}
	return result
}

func writeSRStatusJSON(out io.Writer, rows []srUsageRow, server string, now time.Time) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(buildSRStatusJSON(rows, server, now))
}

// serverStatusJSONFor is the --json form of serverStatusFor. It skips the
// text-only trailing sections and refuses servers that predate usage-status,
// whose fallback is an opaque text body.
func (r srRunner) serverStatusJSONFor(ctx context.Context, server srServerConfig) error {
	usage, available, err := r.fetchServerUsageStatuses(ctx, server)
	if err != nil {
		return err
	}
	if !available {
		return fmt.Errorf("server %s does not serve /_subrouter/usage-status, so %s status --json has no data; upgrade the server", server.Name, r.programOrSubrouter())
	}
	rows := usageRowsFromServerUsageStatuses(usage)
	fresh := enrichClaudeRowsWithWebBalancesFresh(ctx, rows)
	if err := writeSRStatusJSON(r.out, rows, server.Name, time.Now()); err != nil {
		return err
	}
	r.pushClaudeWebBalances(ctx, server, fresh)
	return nil
}

// cloudStatusJSON is the --json form of cloudStatus for hosted credential
// storage. Team storage without a hosted tenant exposes only an account
// list, with no usage to report.
func (r srRunner) cloudStatusJSON(ctx context.Context) error {
	config, _, client, err := loadCloudClient(true)
	if err != nil {
		return err
	}
	if !config.TeamModeReady() && !config.HostedReady() {
		return fmt.Errorf("credential storage is %s; run 'sr login' to use hosted cmux", config.EffectiveCredentialSource())
	}
	if !config.HostedTenantReady() {
		return fmt.Errorf("%s status --json needs usage data, which this credential storage does not provide; use %s status", r.programOrSubrouter(), r.programOrSubrouter())
	}
	statuses, err := client.UsageStatuses(ctx)
	if err != nil {
		return err
	}
	return writeSRStatusJSON(r.out, usageRowsFromHostedStatuses(statuses), "", time.Now())
}

// localStatusJSON is the --json form of the local-store status. Anything
// the auto-import prints goes to stderr so stdout stays one JSON document.
func (r srRunner) localStatusJSON(ctx context.Context) error {
	quiet := r
	quiet.out = r.errOut
	if quiet.out == nil {
		quiet.out = io.Discard
	}
	if err := quiet.autoImportIfEmpty(ctx); err != nil {
		return err
	}
	rows, err := r.fetchUsageRows(ctx)
	if err != nil {
		return err
	}
	return writeSRStatusJSON(r.out, rows, "", time.Now())
}
