package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// claudePickerTier orders the launch picker: accounts that can start a
// session now first, broken ones last.
type claudePickerTier int

const (
	claudePickerUsable claudePickerTier = iota
	claudePickerProtected
	claudePickerExhausted
	claudePickerUnknown
	claudePickerBroken
)

type claudePickerEntry struct {
	account remoteServerAccount
	row     srUsageRow
	hasRow  bool
	tier    claudePickerTier
}

type claudeAccountPicker struct {
	entries []claudePickerEntry
	// withUsage is false when the server exposed no usage status; the picker
	// then lists names only and has no recommendation.
	withUsage bool
	// defaultIndex is the entry Enter picks, or -1 when none is healthy.
	defaultIndex int
	// defaultReason explains a default other than "most headroom", such as
	// the account that last ran a resumed session.
	defaultReason string
}

// Claude's prompt cache is scoped to the account and expires after about
// five minutes idle, or an hour with the extended TTL. Going back to the same
// account only saves re-billing the conversation while it is still warm.
const (
	claudePromptCacheTTL         = 5 * time.Minute
	claudePromptCacheExtendedTTL = time.Hour
)

// claudePromptCacheHint says whether resuming on the same account still
// helps, given when the session last ran.
func claudePromptCacheHint(lastActive, now time.Time) string {
	idle := now.Sub(lastActive)
	switch {
	case idle < claudePromptCacheTTL:
		return "prompt cache likely warm"
	case idle < claudePromptCacheExtendedTTL:
		return "prompt cache warm only with the 1h TTL"
	default:
		return "prompt cache expired, so the account no longer matters for cost"
	}
}

// applyResumeAffinity makes the account that last ran a resumed session the
// Enter default, if it is still healthy. Otherwise the healthiest account
// stays the default and the reason says why.
func (p *claudeAccountPicker) applyResumeAffinity(span sessionAccountSpan, now time.Time) {
	label := span.Label
	if label == "" {
		label = span.AccountID
	}
	ago := formatAgo(now.Sub(span.To))
	hint := claudePromptCacheHint(span.To, now)
	for i, entry := range p.entries {
		if entry.account.ID != span.AccountID {
			continue
		}
		if entry.tier == claudePickerUsable || !p.withUsage {
			p.defaultIndex = i
			p.defaultReason = fmt.Sprintf("last ran this session, %s; %s", ago, hint)
			return
		}
		p.defaultReason = fmt.Sprintf("%s last ran this session (%s) but cannot take a new session now; recommending the healthiest account instead", label, ago)
		return
	}
	p.defaultReason = fmt.Sprintf("%s last ran this session (%s) but is no longer in the pool; recommending the healthiest account instead", label, ago)
}

// claudePickerUnusableError refuses a broken account; the picker re-prompts.
type claudePickerUnusableError struct{ message string }

func (e *claudePickerUnusableError) Error() string { return e.message }

// claudeRowBroken reports a credential that cannot serve until a human fixes
// it: a dead refresh token (invalid_grant and friends) or Anthropic's
// account_on_hold restriction.
func claudeRowBroken(row srUsageRow) bool {
	return row.err != nil && (authErrorNeedsReadd(row.err) || claudeAccountOnHold(row.err))
}

func claudeAccountOnHold(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "account_on_hold")
}

func claudeRowBrokenReason(row srUsageRow) string {
	if claudeAccountOnHold(row.err) {
		return "restricted by Anthropic (account_on_hold)"
	}
	return "needs re-login (refresh token invalid or expired); re-add with: sr add claude"
}

func claudePickerTierFor(row srUsageRow, hasRow bool) claudePickerTier {
	switch {
	case !hasRow:
		return claudePickerUnknown
	case claudeRowBroken(row):
		return claudePickerBroken
	case row.err != nil:
		return claudePickerUnknown
	case row.cooked || row.tempCooked || exhaustedForNewSession(row.score):
		return claudePickerExhausted
	case len(row.windows) == 0:
		return claudePickerUnknown
	case !usableForNewSession(row.score):
		return claudePickerProtected
	default:
		return claudePickerUsable
	}
}

// claudePickerDisplayName keeps a profile literally named "default" from
// reading as the picker's default choice.
func claudePickerDisplayName(name string) string {
	if strings.EqualFold(strings.TrimSpace(name), "default") {
		return name + " (profile name)"
	}
	return name
}

func newClaudeAccountPicker(eligible []remoteServerAccount, statuses []remoteServerUsageStatus) claudeAccountPicker {
	picker := claudeAccountPicker{withUsage: len(statuses) > 0, defaultIndex: -1}
	rowsByID := map[string]srUsageRow{}
	for _, row := range usageRowsFromServerUsageStatuses(statuses) {
		if row.provider == accounts.ProviderClaude && row.accountID != "" {
			rowsByID[row.accountID] = row
		}
	}
	for _, account := range eligible {
		row, ok := rowsByID[account.ID]
		if !ok {
			name := strings.TrimSpace(account.Label)
			if name == "" {
				name = account.ID
			}
			row = srUsageRow{email: name, accountID: account.ID, provider: accounts.ProviderClaude, authMode: account.AuthMode, providerModels: -1}
		}
		label := strings.TrimSpace(account.Label)
		if label == "" {
			label = displayUsageAccountName(row)
		}
		row.displayAccount = claudePickerDisplayName(label)
		picker.entries = append(picker.entries, claudePickerEntry{
			account: account,
			row:     row,
			hasRow:  ok,
			tier:    claudePickerTierFor(row, ok),
		})
	}
	sort.SliceStable(picker.entries, func(i, j int) bool {
		a, b := picker.entries[i], picker.entries[j]
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		if a.tier == claudePickerUsable || a.tier == claudePickerProtected {
			left := minFloat(a.row.score.Headroom, a.row.score.ShortHeadroom)
			right := minFloat(b.row.score.Headroom, b.row.score.ShortHeadroom)
			if left != right {
				return left > right
			}
		}
		return strings.ToLower(a.account.ID) < strings.ToLower(b.account.ID)
	})
	if picker.withUsage && len(picker.entries) > 0 && picker.entries[0].tier == claudePickerUsable {
		picker.defaultIndex = 0
	}
	return picker
}

func (p claudeAccountPicker) display(out io.Writer, pinned bool) {
	if !pinned {
		fmt.Fprintln(out, "  0) Automatic current recommendation (the server picks and fails over)")
	}
	if !p.withUsage {
		for i, entry := range p.entries {
			fmt.Fprintf(out, "  %d) %s\n", i+1, entry.row.displayAccount)
		}
		if p.defaultIndex >= 0 && p.defaultReason != "" {
			fmt.Fprintf(out, "Recommended: %d) %s\n  (%s)\n", p.defaultIndex+1, p.entries[p.defaultIndex].row.displayAccount, p.defaultReason)
		}
		return
	}
	rows := make([]srUsageRow, len(p.entries))
	for i, entry := range p.entries {
		rows[i] = entry.row
	}
	displayUsageRows(out, rows, true)
	// The table already prints each error with its fix; name the rows that
	// the picker will refuse.
	var broken []string
	for i, entry := range p.entries {
		if entry.tier == claudePickerBroken {
			broken = append(broken, fmt.Sprintf("%d", i+1))
		}
	}
	if len(broken) > 0 {
		fmt.Fprintf(out, "Unusable, cannot be picked: %s\n", strings.Join(broken, ", "))
	}
	if p.defaultIndex >= 0 {
		fmt.Fprintf(out, "Recommended: %d) %s\n", p.defaultIndex+1, p.entries[p.defaultIndex].row.displayAccount)
	} else if p.withUsage {
		fmt.Fprintln(out, "No account has headroom for a new session right now.")
	}
	if p.defaultReason != "" {
		fmt.Fprintf(out, "  (%s)\n", p.defaultReason)
	}
}

func (p claudeAccountPicker) prompt(pinned bool) string {
	enter := "cancel"
	if !pinned {
		enter = "automatic"
	}
	if p.defaultIndex >= 0 {
		enter = fmt.Sprintf("%d", p.defaultIndex+1)
	}
	return fmt.Sprintf("Launch account (# or exact profile, Enter = %s): ", enter)
}

// choose resolves one picker answer. chosen=false means the user cancelled a
// pinned launch; an empty ID with chosen=true is the automatic pooled pick.
func (p claudeAccountPicker) choose(answer string, pinned bool, inventory []remoteServerAccount) (string, bool, error) {
	if answer == "" {
		if p.defaultIndex >= 0 {
			return p.entries[p.defaultIndex].account.ID, true, nil
		}
		return "", !pinned, nil
	}
	if !pinned && answer == "0" {
		return "", true, nil
	}
	index, isNumber, err := parsePickerNumber(answer, len(p.entries))
	if err != nil {
		return "", false, err
	}
	var accountID string
	if isNumber {
		accountID = p.entries[index].account.ID
	} else {
		accountID, err = resolveClaudeProxyAccountSelector(inventory, answer)
		if err != nil {
			return "", false, err
		}
	}
	for _, entry := range p.entries {
		if entry.account.ID == accountID && entry.tier == claudePickerBroken {
			return "", false, &claudePickerUnusableError{message: fmt.Sprintf(
				"%s is unusable: %s. Pick another account.", entry.row.displayAccount, claudeRowBrokenReason(entry.row))}
		}
	}
	return accountID, true, nil
}
