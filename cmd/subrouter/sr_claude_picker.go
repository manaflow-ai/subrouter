package main

import (
	"fmt"
	"io"
	"sort"
	"strings"

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
	provider accounts.Provider
	entries  []claudePickerEntry
	// withUsage is false when the server exposed no usage status; the picker
	// then lists names only and has no recommendation.
	withUsage bool
	// defaultIndex is the entry Enter picks, or -1 when none is healthy.
	defaultIndex int
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
		if usageProvider(row) == accounts.ProviderCodex {
			return "restricted by the provider (account_on_hold)"
		}
		return "restricted by Anthropic (account_on_hold)"
	}
	return "needs re-login (refresh token invalid or expired); re-add with: " + providerReaddCommand(usageProvider(row))
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
	return newAccountPicker(accounts.ProviderClaude, eligible, statuses)
}

// newAccountPicker builds the health-ordered launch picker for one
// provider's accounts. Claude and Codex share it.
func newAccountPicker(provider accounts.Provider, eligible []remoteServerAccount, statuses []remoteServerUsageStatus) claudeAccountPicker {
	picker := claudeAccountPicker{provider: provider, withUsage: len(statuses) > 0, defaultIndex: -1}
	rowsByID := map[string]srUsageRow{}
	for _, row := range usageRowsFromServerUsageStatuses(statuses) {
		if row.provider == provider && row.accountID != "" {
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
			row = srUsageRow{email: name, accountID: account.ID, provider: provider, authMode: account.AuthMode, providerModels: -1}
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
	} else {
		fmt.Fprintln(out, "No account has headroom for a new session right now.")
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
	switch {
	case isNumber:
		accountID = p.entries[index].account.ID
	case p.provider == accounts.ProviderClaude || p.provider == "":
		accountID, err = resolveClaudeProxyAccountSelector(inventory, answer)
		if err != nil {
			return "", false, err
		}
	default:
		accountID, err = p.resolveSelector(answer)
		if err != nil {
			return "", false, err
		}
	}
	if err := p.refuseBroken(accountID); err != nil {
		return "", false, err
	}
	return accountID, true, nil
}

// refuseBroken rejects an account whose credential cannot serve.
func (p claudeAccountPicker) refuseBroken(accountID string) error {
	for _, entry := range p.entries {
		if entry.account.ID == accountID && entry.tier == claudePickerBroken {
			return &claudePickerUnusableError{message: fmt.Sprintf(
				"%s is unusable: %s. Pick another account.", entry.row.displayAccount, claudeRowBrokenReason(entry.row))}
		}
	}
	return nil
}

// resolveSelector matches a typed account against the picker's own entries:
// an exact ID or label, else a unique substring of one.
func (p claudeAccountPicker) resolveSelector(selector string) (string, error) {
	selector = strings.ToLower(strings.TrimSpace(selector))
	if selector == "" {
		return "", fmt.Errorf("account selector cannot be empty")
	}
	var partial []string
	for _, entry := range p.entries {
		id := strings.ToLower(entry.account.ID)
		label := strings.ToLower(strings.TrimSpace(entry.account.Label))
		if id == selector || (label != "" && label == selector) {
			return entry.account.ID, nil
		}
		if strings.Contains(id, selector) || (label != "" && strings.Contains(label, selector)) {
			partial = append(partial, entry.account.ID)
		}
	}
	switch len(partial) {
	case 0:
		return "", fmt.Errorf("%s account %q was not found", p.provider, selector)
	case 1:
		return partial[0], nil
	default:
		return "", fmt.Errorf("%s account %q is ambiguous (%s); use the exact account ID", p.provider, selector, strings.Join(partial, ", "))
	}
}
