package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/proxy"
)

const codexAttestLegacyCommand = "sr codex attest-legacy"

func isCodexAttestLegacyCommand(args []string) bool {
	return len(args) > 1 && args[0] == "codex" && args[1] == "attest-legacy"
}

// attestLegacyCodexCredentials repairs stored Codex OAuth records that predate
// provenance tracking. Each one is refreshed once so the provider rotates its
// refresh token, then recorded as server-attested, which the serving isolation
// gate accepts. No interactive login is required. Records whose refresh token
// is also the interactive login on this machine are reported and skipped.
func (r srRunner) attestLegacyCodexCredentials(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet(codexAttestLegacyCommand, flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	stateDir := flags.String("state-dir", "", "attest the store under this service state root instead of the local store")
	dryRun := flags.Bool("dry-run", false, "list the records that would be attested without refreshing anything")
	var only stringList
	flags.Var(&only, "only", "attest one stored Codex OAuth identity (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("usage: %s [--state-dir PATH] [--dry-run] [--only ACCOUNT]...", codexAttestLegacyCommand)
	}

	store := r.store
	if strings.TrimSpace(*stateDir) != "" {
		root, err := normalizeStateRoot(*stateDir)
		if err != nil {
			return err
		}
		store = rawCodexStoreForStateRoot(root)
	}

	targets, err := codexIsolationTargets(store)
	if err != nil {
		return err
	}
	targets, err = selectCodexAttestTargets(targets, only)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		fmt.Fprintln(r.out, "All stored Codex OAuth accounts are isolated. Nothing to attest.")
		return nil
	}
	if *dryRun {
		fmt.Fprintf(r.out, "%d stored Codex OAuth account(s) would be attested:\n", len(targets))
		for _, target := range targets {
			fmt.Fprintf(r.out, "  %s\n", target.Email)
		}
		return nil
	}

	fmt.Fprintf(r.out, "Attesting %d stored Codex OAuth account(s) in %s.\n", len(targets), store.Dir)
	attested, skipped, failed := 0, 0, 0
	for _, target := range targets {
		err := proxy.PublishAccountDiskMutation(ctx, store.StoreDir(), func() (bool, error) {
			_, attestErr := store.AttestStoredLegacyOAuth(ctx, r.client, target.Email)
			return attestErr == nil, attestErr
		})
		switch {
		case err == nil:
			attested++
			fmt.Fprintf(r.out, "  attested %s\n", target.Email)
		case errors.Is(err, accounts.ErrCodexCredentialAlreadyIsolated):
			skipped++
			fmt.Fprintf(r.out, "  skipped  %s: already isolated\n", target.Email)
		case errors.Is(err, accounts.ErrCodexCredentialSharesActiveAuth):
			skipped++
			fmt.Fprintf(r.out, "  skipped  %s: shares the interactive Codex login; run '%s'\n", target.Email, codexIsolationRemediation)
		default:
			failed++
			fmt.Fprintf(r.out, "  failed   %s: %v\n", target.Email, err)
		}
	}
	fmt.Fprintf(r.out, "\nAttested %d, skipped %d, failed %d.\n", attested, skipped, failed)
	if failed != 0 {
		return fmt.Errorf("%d stored Codex OAuth account(s) could not be attested", failed)
	}
	return nil
}

func selectCodexAttestTargets(targets []accounts.StoredCodexAccount, only stringList) ([]accounts.StoredCodexAccount, error) {
	if len(only) == 0 {
		return targets, nil
	}
	byEmail := make(map[string]accounts.StoredCodexAccount, len(targets))
	for _, target := range targets {
		byEmail[strings.ToLower(strings.TrimSpace(target.Email))] = target
	}
	selected := make([]accounts.StoredCodexAccount, 0, len(only))
	seen := make(map[string]struct{}, len(only))
	for _, raw := range only {
		selector := strings.ToLower(strings.TrimSpace(raw))
		if selector == "" {
			return nil, errors.New("--only account selector must not be empty")
		}
		if _, dup := seen[selector]; dup {
			return nil, fmt.Errorf("duplicate --only account selector %q", strings.TrimSpace(raw))
		}
		seen[selector] = struct{}{}
		target, ok := byEmail[selector]
		if !ok {
			return nil, fmt.Errorf("--only account %q is not a stored Codex OAuth identity that needs attestation", strings.TrimSpace(raw))
		}
		selected = append(selected, target)
	}
	return selected, nil
}
