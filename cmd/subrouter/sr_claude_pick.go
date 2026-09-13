package main

import (
	"context"
	"fmt"
	"sort"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/agents/claude"
)

// pickClaudeProfile switches the active Claude profile to the one with the
// most quota headroom. Usage comes from the default server's pool view when
// one is configured (the same data `sr` renders), falling back to local
// per-profile usage fetches otherwise.
func (r srRunner) pickClaudeProfile(ctx context.Context) error {
	rows, err := r.claudeUsageRowsForPick(ctx)
	if err != nil {
		return err
	}
	store := claude.DefaultStore()
	localProfiles := map[string]bool{}
	for _, profile := range store.ListProfiles() {
		localProfiles[profile.Name] = true
	}
	candidates := make([]srUsageRow, 0, len(rows))
	for _, row := range rows {
		if localProfiles[row.email] {
			candidates = append(candidates, row)
		}
	}
	if len(candidates) == 0 {
		return fmt.Errorf("no Claude profiles configured. Run 'sr add claude' first")
	}
	target := bestClaudeUsageRow(candidates)
	if target == nil {
		displayUsageRows(r.out, candidates, false)
		return fmt.Errorf("no Claude profile has quota for a new session")
	}
	active := store.ActiveProfile()
	displayUsageRows(r.out, []srUsageRow{*target}, false)
	if target.email == active {
		fmt.Fprintf(r.out, "Already using recommended Claude profile: %s\n", target.email)
		return nil
	}
	if err := store.SetActiveProfile(target.email); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Picked recommended Claude profile: %s\n", target.email)
	return nil
}

func (r srRunner) claudeUsageRowsForPick(ctx context.Context) ([]srUsageRow, error) {
	if server, ok, err := r.defaultRemoteServer(); err != nil {
		return nil, err
	} else if ok {
		usage, available, err := r.fetchServerUsageStatuses(ctx, server)
		if err == nil && available {
			rows := usageRowsFromServerUsageStatuses(usage)
			claudeRows := make([]srUsageRow, 0, len(rows))
			for _, row := range rows {
				if row.provider == accounts.ProviderClaude {
					claudeRows = append(claudeRows, row)
				}
			}
			if len(claudeRows) > 0 {
				return claudeRows, nil
			}
		}
		// Server missing/old/empty: fall through to local usage.
	}
	all, err := r.fetchUsageRows(ctx)
	if err != nil {
		return nil, err
	}
	claudeRows := make([]srUsageRow, 0, len(all))
	for _, row := range all {
		if row.provider == accounts.ProviderClaude {
			claudeRows = append(claudeRows, row)
		}
	}
	return claudeRows, nil
}

// bestClaudeUsageRow returns the healthy row with the most constrained-window
// headroom, or nil when every profile is errored or exhausted. Claude rows
// never get gtoRecommended (that flag is codex-only), so pick sorts directly.
func bestClaudeUsageRow(rows []srUsageRow) *srUsageRow {
	usable := make([]srUsageRow, 0, len(rows))
	for _, row := range rows {
		if row.err != nil || row.cooked || row.tempCooked {
			continue
		}
		if row.score.Headroom <= 0 || row.score.ShortHeadroom <= 0 {
			continue
		}
		usable = append(usable, row)
	}
	if len(usable) == 0 {
		return nil
	}
	sort.SliceStable(usable, func(i, j int) bool {
		left := minFloat(usable[i].score.Headroom, usable[i].score.ShortHeadroom)
		right := minFloat(usable[j].score.Headroom, usable[j].score.ShortHeadroom)
		if left != right {
			return left > right
		}
		return usable[i].email < usable[j].email
	})
	return &usable[0]
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
