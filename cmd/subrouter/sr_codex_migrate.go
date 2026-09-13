package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/internal/storepath"
)

const codexIsolationRemediation = "sr codex migrate-isolation"
const codexIsolatedEnrollmentCommand = "sr codex enroll-isolated"
const codexIsolationComparisonRemediation = "verify candidate and retiring credential stores before activation"
const codexAccountUsage = "sr codex isolation-check [--json] [--retiring-state-dir PATH], sr codex migrate-isolation [--device-auth], or sr codex enroll-isolated --retiring-state-dir PATH [--device-auth] [--only ACCOUNT]..."

var errCodexIsolationCheckFailed = errors.New("codex credential isolation preflight failed")

func isCodexAccountCommand(args []string) bool {
	return len(args) > 1 && args[0] == "codex" &&
		(args[1] == "isolation-check" || args[1] == "migrate-isolation" || args[1] == "enroll-isolated")
}

func isCodexIsolationCheckCommand(args []string) bool {
	return len(args) > 1 && args[0] == "codex" && args[1] == "isolation-check"
}

func isCodexIsolatedEnrollmentCommand(args []string) bool {
	return len(args) > 1 && args[0] == "codex" && args[1] == "enroll-isolated"
}

type codexIsolationCheckResult struct {
	SchemaVersion              int                             `json:"schema_version"`
	OK                         bool                            `json:"ok"`
	AccountsRequiringMigration int                             `json:"accounts_requiring_migration"`
	Remediation                string                          `json:"remediation,omitempty"`
	Comparison                 *codexIsolationComparisonResult `json:"comparison,omitempty"`
}

type codexIsolationComparisonResult struct {
	OK                              bool `json:"ok"`
	RootsDistinct                   bool `json:"roots_distinct"`
	CandidateStoreAnchored          bool `json:"candidate_store_anchored"`
	RetiringStoreAnchored           bool `json:"retiring_store_anchored"`
	EffectiveStoresDistinct         bool `json:"effective_stores_distinct"`
	CandidateAccountCount           int  `json:"candidate_account_count"`
	RetiringAccountCount            int  `json:"retiring_account_count"`
	CandidateDuplicateSelectorCount int  `json:"candidate_duplicate_normalized_selector_count"`
	RetiringDuplicateSelectorCount  int  `json:"retiring_duplicate_normalized_selector_count"`
	NormalizedSelectorsUnique       bool `json:"normalized_account_selectors_unique"`
	NonzeroAccountInventory         bool `json:"nonzero_account_inventory"`
	AccountInventoryMatch           bool `json:"account_inventory_match"`
	CandidateMissingIdentityCount   int  `json:"candidate_missing_immutable_identity_count"`
	RetiringMissingIdentityCount    int  `json:"retiring_missing_immutable_identity_count"`
	ImmutableIdentitiesComplete     bool `json:"immutable_account_identities_complete"`
	CandidateOAuthAccountCount      int  `json:"candidate_oauth_account_count"`
	CandidateUntrustedOAuthCount    int  `json:"candidate_untrusted_oauth_count"`
	CandidateOAuthOriginsTrusted    bool `json:"candidate_oauth_origins_trusted"`
	CandidateMissingRefreshCount    int  `json:"candidate_missing_oauth_refresh_token_count"`
	CandidateDuplicateChainCount    int  `json:"candidate_duplicate_oauth_refresh_chain_count"`
	CandidateOAuthChainsValid       bool `json:"candidate_oauth_refresh_chains_valid"`
	SharedOAuthRefreshTokenCount    int  `json:"shared_oauth_refresh_token_count"`
	OAuthRefreshTokenChainsUnique   bool `json:"oauth_refresh_token_chains_unique"`
}

type activeCodexAuthSnapshot struct {
	exists bool
	body   []byte
}

func snapshotActiveCodexAuth() (activeCodexAuthSnapshot, error) {
	body, err := os.ReadFile(accounts.DefaultCodexAuthPath())
	if errors.Is(err, os.ErrNotExist) {
		return activeCodexAuthSnapshot{}, nil
	}
	if err != nil {
		return activeCodexAuthSnapshot{}, err
	}
	return activeCodexAuthSnapshot{exists: true, body: body}, nil
}

func (snapshot activeCodexAuthSnapshot) unchanged() (bool, error) {
	current, err := snapshotActiveCodexAuth()
	if err != nil {
		return false, err
	}
	return snapshot.exists == current.exists && bytes.Equal(snapshot.body, current.body), nil
}

func codexIsolationTargets(store accounts.CodexStore) ([]accounts.StoredCodexAccount, error) {
	return codexIsolationTargetsFromList(store, store.ListStored)
}

func codexIsolationTargetsReadOnly(store accounts.CodexStore) ([]accounts.StoredCodexAccount, error) {
	return codexIsolationTargetsFromList(store, store.ListStoredReadOnly)
}

func codexIsolationTargetsFromList(
	store accounts.CodexStore,
	list func() ([]accounts.StoredCodexAccount, error),
) ([]accounts.StoredCodexAccount, error) {
	stored, err := list()
	if err != nil {
		return nil, err
	}
	active, activeOK, activeErr := accounts.ReadActiveCodexAuth()
	if activeErr != nil {
		// Explicit provenance remains authoritative when interactive auth is
		// unreadable. Missing provenance still requires migration.
		activeOK = false
	}
	targets := make([]accounts.StoredCodexAccount, 0)
	for _, account := range stored {
		if account.IsAPIKey() || account.ProviderOrDefault() != accounts.ProviderCodex || account.Auth.Tokens == nil {
			continue
		}
		trusted := account.OAuthCredentialOrigin == accounts.CodexOAuthOriginIsolatedServerLogin ||
			account.OAuthCredentialOrigin == accounts.CodexOAuthOriginServerAttested
		sharesActive := activeOK && active.Tokens != nil && active.Tokens.RefreshToken != "" &&
			active.Tokens.RefreshToken == account.Auth.Tokens.RefreshToken
		if !trusted || sharesActive {
			targets = append(targets, account)
		}
	}
	return targets, nil
}

func compareCodexIsolationStores(
	candidateRoot, retiringRoot string,
	candidateStore, retiringStore accounts.CodexStore,
) (codexIsolationComparisonResult, error) {
	normalizedCandidate, err := normalizeStateRoot(candidateRoot)
	if err != nil {
		return codexIsolationComparisonResult{}, fmt.Errorf("normalize candidate state root: %w", err)
	}
	normalizedRetiring, err := normalizeStateRoot(retiringRoot)
	if err != nil {
		return codexIsolationComparisonResult{}, fmt.Errorf("normalize retiring state root: %w", err)
	}
	rootsDistinct, err := distinctStateRoots(normalizedCandidate, normalizedRetiring)
	if err != nil {
		return codexIsolationComparisonResult{}, err
	}
	normalizedCandidateStore, err := normalizeStateRoot(candidateStore.Dir)
	if err != nil {
		return codexIsolationComparisonResult{}, fmt.Errorf("normalize candidate credential store: %w", err)
	}
	normalizedRetiringStore, err := normalizeStateRoot(retiringStore.Dir)
	if err != nil {
		return codexIsolationComparisonResult{}, fmt.Errorf("normalize retiring credential store: %w", err)
	}
	expectedCandidateStore, err := normalizeStateRoot(filepath.Join(normalizedCandidate, "codex", "accounts"))
	if err != nil {
		return codexIsolationComparisonResult{}, fmt.Errorf("normalize expected candidate credential store: %w", err)
	}
	candidateStoreAnchored := normalizedCandidateStore == expectedCandidateStore
	expectedRetiringStore, err := normalizeStateRoot(filepath.Join(normalizedRetiring, "codex", "accounts"))
	if err != nil {
		return codexIsolationComparisonResult{}, fmt.Errorf("normalize expected retiring credential store: %w", err)
	}
	retiringStoreAnchored := normalizedRetiringStore == expectedRetiringStore
	effectiveStoresDistinct, err := distinctStateRoots(normalizedCandidateStore, normalizedRetiringStore)
	if err != nil {
		return codexIsolationComparisonResult{}, fmt.Errorf("compare effective credential stores: %w", err)
	}
	candidate, err := candidateStore.ListStoredReadOnly()
	if err != nil {
		return codexIsolationComparisonResult{}, fmt.Errorf("inspect candidate credential store: %w", err)
	}
	retiring, err := retiringStore.ListStoredReadOnly()
	if err != nil {
		return codexIsolationComparisonResult{}, fmt.Errorf("inspect retiring credential store: %w", err)
	}

	candidateIDs, candidateMissingIdentity, candidateDuplicateSelectors := codexStoredAccountInventory(candidate)
	retiringIDs, retiringMissingIdentity, retiringDuplicateSelectors := codexStoredAccountInventory(retiring)
	nonzeroInventory := len(candidate) > 0 && len(retiring) > 0
	selectorsUnique := candidateDuplicateSelectors == 0 && retiringDuplicateSelectors == 0
	identitiesComplete := candidateMissingIdentity == 0 && retiringMissingIdentity == 0
	inventoryMatch := nonzeroInventory && selectorsUnique && identitiesComplete && equalStoredAccountInventories(candidateIDs, retiringIDs)

	candidateRefresh := make(map[[sha256.Size]byte]int)
	retiringRefresh := make(map[[sha256.Size]byte]struct{})
	candidateOAuthCount := 0
	candidateUntrusted := 0
	candidateMissingRefresh := 0
	for _, account := range candidate {
		if !storedCodexOAuthAccount(account) {
			continue
		}
		candidateOAuthCount++
		if !trustedCodexOAuthOrigin(account.OAuthCredentialOrigin) {
			candidateUntrusted++
		}
		if account.Auth.Tokens == nil || strings.TrimSpace(account.Auth.Tokens.RefreshToken) == "" {
			candidateMissingRefresh++
			continue
		}
		token := strings.TrimSpace(account.Auth.Tokens.RefreshToken)
		candidateRefresh[sha256.Sum256([]byte(token))]++
	}
	for _, account := range retiring {
		if !storedCodexOAuthAccount(account) {
			continue
		}
		if account.Auth.Tokens != nil {
			if token := strings.TrimSpace(account.Auth.Tokens.RefreshToken); token != "" {
				retiringRefresh[sha256.Sum256([]byte(token))] = struct{}{}
			}
		}
	}
	candidateDuplicateChains := 0
	for _, count := range candidateRefresh {
		if count > 1 {
			candidateDuplicateChains++
		}
	}
	sharedRefresh := 0
	for fingerprint := range candidateRefresh {
		if _, shared := retiringRefresh[fingerprint]; shared {
			sharedRefresh++
		}
	}

	result := codexIsolationComparisonResult{
		RootsDistinct:                   rootsDistinct,
		CandidateStoreAnchored:          candidateStoreAnchored,
		RetiringStoreAnchored:           retiringStoreAnchored,
		EffectiveStoresDistinct:         effectiveStoresDistinct,
		CandidateAccountCount:           len(candidate),
		RetiringAccountCount:            len(retiring),
		CandidateDuplicateSelectorCount: candidateDuplicateSelectors,
		RetiringDuplicateSelectorCount:  retiringDuplicateSelectors,
		NormalizedSelectorsUnique:       selectorsUnique,
		NonzeroAccountInventory:         nonzeroInventory,
		AccountInventoryMatch:           inventoryMatch,
		CandidateMissingIdentityCount:   candidateMissingIdentity,
		RetiringMissingIdentityCount:    retiringMissingIdentity,
		ImmutableIdentitiesComplete:     identitiesComplete,
		CandidateOAuthAccountCount:      candidateOAuthCount,
		CandidateUntrustedOAuthCount:    candidateUntrusted,
		CandidateOAuthOriginsTrusted:    candidateUntrusted == 0,
		CandidateMissingRefreshCount:    candidateMissingRefresh,
		CandidateDuplicateChainCount:    candidateDuplicateChains,
		CandidateOAuthChainsValid:       candidateMissingRefresh == 0 && candidateDuplicateChains == 0,
		SharedOAuthRefreshTokenCount:    sharedRefresh,
		OAuthRefreshTokenChainsUnique:   sharedRefresh == 0,
	}
	result.OK = result.RootsDistinct && result.CandidateStoreAnchored && result.RetiringStoreAnchored &&
		result.EffectiveStoresDistinct && result.NonzeroAccountInventory &&
		result.AccountInventoryMatch && result.NormalizedSelectorsUnique &&
		result.ImmutableIdentitiesComplete &&
		result.CandidateOAuthOriginsTrusted &&
		result.CandidateOAuthChainsValid && result.OAuthRefreshTokenChainsUnique
	return result, nil
}

func storedCodexOAuthAccount(account accounts.StoredCodexAccount) bool {
	return !account.IsAPIKey() && account.ProviderOrDefault() == accounts.ProviderCodex
}

func trustedCodexOAuthOrigin(origin accounts.CodexOAuthCredentialOrigin) bool {
	return origin == accounts.CodexOAuthOriginIsolatedServerLogin ||
		origin == accounts.CodexOAuthOriginServerAttested
}

type codexStoredAccountIdentity struct {
	provider           accounts.Provider
	authMode           string
	immutableAccountID string
}

func codexStoredAccountInventory(stored []accounts.StoredCodexAccount) (map[string]codexStoredAccountIdentity, int, int) {
	inventory := make(map[string]codexStoredAccountIdentity, len(stored))
	missingIdentity := 0
	duplicateSelectors := 0
	for _, account := range stored {
		if id := strings.ToLower(strings.TrimSpace(account.Email)); id != "" {
			if _, exists := inventory[id]; exists {
				duplicateSelectors++
			}
			identity := codexStoredAccountIdentity{provider: account.ProviderOrDefault()}
			if account.IsAPIKey() {
				identity.authMode = "apikey"
			} else {
				identity.authMode = "oauth"
				identity.immutableAccountID = strings.TrimSpace(accounts.ExtractChatGPTAccountID(account.Auth))
				if identity.immutableAccountID == "" {
					missingIdentity++
				}
			}
			inventory[id] = identity
		}
	}
	return inventory, missingIdentity, duplicateSelectors
}

func equalStoredAccountInventories(left, right map[string]codexStoredAccountIdentity) bool {
	if len(left) != len(right) {
		return false
	}
	for id, identity := range left {
		if other, ok := right[id]; !ok || other != identity {
			return false
		}
	}
	return true
}

func normalizeStateRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", errors.New("state root is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	cursor := abs
	missing := make([]string, 0)
	for {
		resolved, evalErr := filepath.EvalSymlinks(cursor)
		if evalErr == nil {
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(evalErr, os.ErrNotExist) {
			return "", evalErr
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			return abs, nil
		}
		missing = append(missing, filepath.Base(cursor))
		cursor = parent
	}
}

func distinctStateRoots(candidate, retiring string) (bool, error) {
	if candidate == retiring {
		return false, nil
	}
	candidateInfo, candidateErr := os.Stat(candidate)
	if candidateErr != nil && !errors.Is(candidateErr, os.ErrNotExist) {
		return false, fmt.Errorf("stat candidate state root: %w", candidateErr)
	}
	retiringInfo, retiringErr := os.Stat(retiring)
	if retiringErr != nil && !errors.Is(retiringErr, os.ErrNotExist) {
		return false, fmt.Errorf("stat retiring state root: %w", retiringErr)
	}
	if candidateErr == nil && retiringErr == nil && os.SameFile(candidateInfo, retiringInfo) {
		return false, nil
	}
	return true, nil
}

func isolatedCodexAuthSharesActive(auth accounts.CodexAuthFile) bool {
	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil || !ok || active.Tokens == nil || auth.Tokens == nil {
		return false
	}
	return active.Tokens.RefreshToken != "" && active.Tokens.RefreshToken == auth.Tokens.RefreshToken
}

func codexIsolationDoctorCheck(store accounts.CodexStore) doctorCheck {
	targets, err := codexIsolationTargets(store)
	if err != nil {
		return doctorCheck{"fail", "Codex isolation", err.Error()}
	}
	if len(targets) > 0 {
		return doctorCheck{
			"fail", "Codex isolation",
			fmt.Sprintf("%d account(s) need isolated re-login; run '%s'", len(targets), codexIsolationRemediation),
		}
	}
	return doctorCheck{"ok", "Codex isolation", "all local OAuth accounts are isolated"}
}

func localCodexStoreServesLegacy(store accounts.CodexStore) bool {
	runner := srRunner{store: store}
	server, selected, err := runner.defaultRemoteServer()
	if err != nil {
		return false
	}
	if selected {
		return sameEndpoint(server.URL, localBaseURL())
	}
	configured, err := defaultCodexBaseURLForHealth()
	return err == nil && (configured == "" || sameEndpoint(configured, localBaseURL()))
}

func printCodexIsolationStatus(out io.Writer, store accounts.CodexStore) error {
	targets, err := codexIsolationTargets(store)
	if err != nil {
		return err
	}
	if len(targets) > 0 {
		_, err = fmt.Fprintf(out, "Codex isolation: %d account(s) need isolated re-login; run '%s'\n\n", len(targets), codexIsolationRemediation)
	}
	return err
}

func reportCodexIsolationRemaining(out io.Writer, store accounts.CodexStore) {
	remaining, err := codexIsolationTargets(store)
	if err != nil {
		fmt.Fprintf(out, "Could not recount remaining Codex accounts: %v\n", err)
		return
	}
	fmt.Fprintf(out, "%d account(s) still need migration.\n", len(remaining))
}

func (r srRunner) codexAccount(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: %s", codexAccountUsage)
	}
	switch args[0] {
	case "isolation-check":
		return r.checkCodexIsolation(args[1:])
	case "migrate-isolation":
		return r.migrateCodexIsolation(ctx, args[1:])
	case "enroll-isolated":
		return r.enrollCodexIsolation(ctx, args[1:])
	default:
		return fmt.Errorf("unknown Codex account command %q; usage: %s", args[0], codexAccountUsage)
	}
}

func (r srRunner) checkCodexIsolation(args []string) error {
	flags := flag.NewFlagSet("sr codex isolation-check", flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	jsonOutput := flags.Bool("json", false, "print machine-readable JSON")
	retiringStateDir := flags.String("retiring-state-dir", "", "compare against the retiring service state root")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: sr codex isolation-check [--json] [--retiring-state-dir PATH]")
	}
	comparisonRequested := false
	flags.Visit(func(parsed *flag.Flag) {
		if parsed.Name == "retiring-state-dir" {
			comparisonRequested = true
		}
	})
	if comparisonRequested && strings.TrimSpace(*retiringStateDir) == "" {
		return errors.New("--retiring-state-dir must not be empty")
	}
	targets, err := codexIsolationTargetsReadOnly(r.store)
	if err != nil {
		return err
	}
	result := codexIsolationCheckResult{
		SchemaVersion:              1,
		OK:                         len(targets) == 0,
		AccountsRequiringMigration: len(targets),
	}
	if comparisonRequested {
		normalizedRetiring, normalizeErr := normalizeStateRoot(*retiringStateDir)
		if normalizeErr != nil {
			return fmt.Errorf("normalize retiring state root: %w", normalizeErr)
		}
		comparison, compareErr := compareCodexIsolationStores(
			storepath.StateDir(), *retiringStateDir, r.store,
			accounts.CodexStoreForStateRootReadOnlyInspection(normalizedRetiring),
		)
		if compareErr != nil {
			return compareErr
		}
		result.Comparison = &comparison
		result.OK = result.OK && comparison.OK
	}
	if !result.OK {
		if result.Comparison != nil && !result.Comparison.OK {
			result.Remediation = codexIsolationComparisonRemediation
		} else {
			result.Remediation = codexIsolationRemediation
		}
	}
	if *jsonOutput {
		if err := json.NewEncoder(r.out).Encode(result); err != nil {
			return err
		}
	} else if result.OK {
		fmt.Fprintln(r.out, "Codex credential isolation preflight passed.")
	} else if result.Comparison != nil && !result.Comparison.OK {
		fmt.Fprintf(r.out, "Codex credential isolation comparison failed: %s.\n", result.Remediation)
	} else {
		fmt.Fprintf(r.out, "Codex credential isolation preflight failed: %d account(s) require migration; run '%s'.\n", result.AccountsRequiringMigration, result.Remediation)
	}
	if !result.OK {
		return errCodexIsolationCheckFailed
	}
	return nil
}

func (r srRunner) migrateCodexIsolation(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet(codexIsolationRemediation, flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	deviceAuth := flags.Bool("device-auth", false, "use codex login --device-auth")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("usage: %s [--device-auth]", codexIsolationRemediation)
	}
	targets, err := codexIsolationTargets(r.store)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		fmt.Fprintln(r.out, "All local Codex OAuth accounts are isolated. No migration needed.")
		return nil
	}
	originalActive, err := snapshotActiveCodexAuth()
	if err != nil {
		return fmt.Errorf("snapshot local Codex auth: %w", err)
	}
	fmt.Fprintf(r.out, "Found %d Codex OAuth account(s) that need isolated re-login.\n", len(targets))
	fmt.Fprintln(r.out, "Each login uses a temporary CODEX_HOME; ~/.codex/auth.json will not be changed.")

	for index, target := range targets {
		fmt.Fprintf(r.out, "\nSign in as %s (%d/%d).\n", target.Email, index+1, len(targets))
		auth, email, loginErr := r.isolatedCodexLogin(ctx, *deviceAuth)
		if loginErr != nil {
			reportCodexIsolationRemaining(r.out, r.store)
			return fmt.Errorf("isolated login for %s: %w", target.Email, loginErr)
		}
		unchanged, checkErr := originalActive.unchanged()
		if checkErr != nil {
			reportCodexIsolationRemaining(r.out, r.store)
			return fmt.Errorf("verify local Codex auth after login: %w", checkErr)
		}
		if !unchanged {
			reportCodexIsolationRemaining(r.out, r.store)
			return errors.New("local Codex auth changed unexpectedly; no stored credential was replaced for this login")
		}
		if !accounts.CanReplaceCodexOAuthIdentity(target.Auth, auth) {
			fmt.Fprintln(r.out, "No stored credential was changed for this login.")
			reportCodexIsolationRemaining(r.out, r.store)
			return fmt.Errorf("logged in as %s, expected %s", email, target.Email)
		}
		if isolatedCodexAuthSharesActive(auth) {
			fmt.Fprintln(r.out, "No stored credential was changed for this login.")
			reportCodexIsolationRemaining(r.out, r.store)
			return errors.New("isolated login returned the active Codex refresh-token chain; retry the isolated login")
		}
		err := proxy.PublishAccountDiskMutation(ctx, r.store.StoreDir(), func() (bool, error) {
			replaceErr := r.store.ReplaceStoredOAuthWithIsolated(ctx, target.Email, auth)
			return replaceErr == nil, replaceErr
		})
		if err != nil {
			reportCodexIsolationRemaining(r.out, r.store)
			return fmt.Errorf("replace isolated credential for %s: %w", target.Email, err)
		}
		fmt.Fprintf(r.out, "Migrated %s (%d remaining).\n", target.Email, len(targets)-index-1)
	}

	remaining, err := codexIsolationTargets(r.store)
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		return fmt.Errorf("migration incomplete: %d account(s) still need isolated re-login", len(remaining))
	}
	unchanged, err := originalActive.unchanged()
	if err != nil {
		return err
	}
	if !unchanged {
		return errors.New("local Codex auth changed unexpectedly during migration")
	}
	fmt.Fprintf(r.out, "\nMigrated %d Codex OAuth account(s). Local Codex auth was left unchanged.\n", len(targets))
	return nil
}

type codexEnrollmentInventory struct {
	retiring         []accounts.StoredCodexAccount
	retiringByEmail  map[string]accounts.StoredCodexAccount
	candidateByEmail map[string]accounts.StoredCodexAccount
	retiringRefresh  map[[sha256.Size]byte]struct{}
	candidateRefresh map[[sha256.Size]byte]struct{}
}

func rawCodexStoreForStateRoot(root string) accounts.CodexStore {
	return accounts.CodexStore{
		Dir:                   filepath.Join(root, "codex", "accounts"),
		DisableActiveAuthSync: true,
		RequireIsolatedOAuth:  true,
	}
}

func validateEnrollmentRoots(candidate, retiring string) error {
	if candidate == retiring {
		return errors.New("candidate and retiring state roots must be different")
	}
	for _, pair := range [][2]string{{candidate, retiring}, {retiring, candidate}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err != nil {
			return fmt.Errorf("compare candidate and retiring state roots: %w", err)
		}
		if rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return errors.New("candidate and retiring state roots must not be nested")
		}
	}
	return nil
}

func completeCodexOAuth(auth accounts.CodexAuthFile) bool {
	return auth.Tokens != nil && strings.TrimSpace(auth.Tokens.AccessToken) != "" &&
		strings.TrimSpace(auth.Tokens.RefreshToken) != "" && strings.TrimSpace(auth.Tokens.IDToken) != ""
}

func activeCodexRefreshToken() string {
	auth, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil || !ok || auth.Tokens == nil {
		return ""
	}
	return strings.TrimSpace(auth.Tokens.RefreshToken)
}

func inspectCodexEnrollmentInventories(
	candidateStore, retiringStore accounts.CodexStore,
) (codexEnrollmentInventory, error) {
	retiring, err := retiringStore.ListStoredReadOnly()
	if err != nil {
		return codexEnrollmentInventory{}, fmt.Errorf("inspect retiring credential store: %w", err)
	}
	candidate, err := candidateStore.ListStoredReadOnly()
	if err != nil {
		return codexEnrollmentInventory{}, fmt.Errorf("inspect candidate credential store: %w", err)
	}

	inventory := codexEnrollmentInventory{
		retiring:         make([]accounts.StoredCodexAccount, 0, len(retiring)),
		retiringByEmail:  make(map[string]accounts.StoredCodexAccount, len(retiring)),
		candidateByEmail: make(map[string]accounts.StoredCodexAccount, len(candidate)),
		retiringRefresh:  make(map[[sha256.Size]byte]struct{}, len(retiring)),
		candidateRefresh: make(map[[sha256.Size]byte]struct{}, len(candidate)),
	}
	for _, account := range retiring {
		if !storedCodexOAuthAccount(account) {
			continue
		}
		email := strings.ToLower(strings.TrimSpace(account.Email))
		accountID := strings.TrimSpace(accounts.ExtractChatGPTAccountID(account.Auth))
		if email == "" || !completeCodexOAuth(account.Auth) || accountID == "" {
			return codexEnrollmentInventory{}, errors.New("retiring Codex OAuth inventory must contain complete accounts with immutable identities")
		}
		if _, exists := inventory.retiringByEmail[email]; exists {
			return codexEnrollmentInventory{}, errors.New("retiring credential store has duplicate normalized account selectors")
		}
		fingerprint := sha256.Sum256([]byte(strings.TrimSpace(account.Auth.Tokens.RefreshToken)))
		if _, exists := inventory.retiringRefresh[fingerprint]; exists {
			return codexEnrollmentInventory{}, errors.New("retiring credential store has duplicate OAuth refresh-token chains")
		}
		inventory.retiringByEmail[email] = account
		inventory.retiringRefresh[fingerprint] = struct{}{}
		inventory.retiring = append(inventory.retiring, account)
	}
	if len(inventory.retiring) == 0 {
		return codexEnrollmentInventory{}, errors.New("retiring credential store has no Codex OAuth accounts")
	}

	activeRefresh := activeCodexRefreshToken()
	for _, account := range candidate {
		email := strings.ToLower(strings.TrimSpace(account.Email))
		if !storedCodexOAuthAccount(account) {
			if _, collides := inventory.retiringByEmail[email]; collides {
				return codexEnrollmentInventory{}, errors.New("candidate credential store has a non-Codex credential colliding with a retiring Codex account selector")
			}
			continue
		}
		if email == "" || !completeCodexOAuth(account.Auth) || !trustedCodexOAuthOrigin(account.OAuthCredentialOrigin) {
			return codexEnrollmentInventory{}, errors.New("candidate credential store is not an exact safe subset of the retiring OAuth inventory")
		}
		if _, exists := inventory.candidateByEmail[email]; exists {
			return codexEnrollmentInventory{}, errors.New("candidate credential store has duplicate normalized account selectors")
		}
		retiringAccount, exists := inventory.retiringByEmail[email]
		if !exists || strings.TrimSpace(accounts.ExtractChatGPTAccountID(account.Auth)) == "" ||
			strings.TrimSpace(accounts.ExtractChatGPTAccountID(account.Auth)) != strings.TrimSpace(accounts.ExtractChatGPTAccountID(retiringAccount.Auth)) {
			return codexEnrollmentInventory{}, errors.New("candidate credential store is not an exact safe subset of the retiring OAuth inventory")
		}
		refresh := strings.TrimSpace(account.Auth.Tokens.RefreshToken)
		fingerprint := sha256.Sum256([]byte(refresh))
		if _, exists := inventory.candidateRefresh[fingerprint]; exists {
			return codexEnrollmentInventory{}, errors.New("candidate credential store has duplicate OAuth refresh-token chains")
		}
		if _, shared := inventory.retiringRefresh[fingerprint]; shared || (activeRefresh != "" && refresh == activeRefresh) {
			return codexEnrollmentInventory{}, errors.New("candidate credential store shares an OAuth refresh-token chain")
		}
		inventory.candidateByEmail[email] = account
		inventory.candidateRefresh[fingerprint] = struct{}{}
	}
	return inventory, nil
}

func validateFreshEnrollmentAuth(
	auth accounts.CodexAuthFile,
	email string,
	target accounts.StoredCodexAccount,
	inventory codexEnrollmentInventory,
) error {
	if !completeCodexOAuth(auth) {
		return errors.New("isolated Codex login did not produce complete OAuth auth")
	}
	if !accounts.CanReplaceCodexOAuthIdentity(target.Auth, auth) {
		return errors.New("isolated Codex login immutable account identity does not match the retiring account")
	}
	wantID := strings.TrimSpace(accounts.ExtractChatGPTAccountID(target.Auth))
	gotID := strings.TrimSpace(accounts.ExtractChatGPTAccountID(auth))
	if gotID == "" || gotID != wantID {
		return errors.New("isolated Codex login immutable account identity does not match the retiring account")
	}
	refresh := strings.TrimSpace(auth.Tokens.RefreshToken)
	fingerprint := sha256.Sum256([]byte(refresh))
	if _, shared := inventory.retiringRefresh[fingerprint]; shared {
		return errors.New("isolated Codex login returned a retiring OAuth refresh-token chain")
	}
	if _, shared := inventory.candidateRefresh[fingerprint]; shared {
		return errors.New("isolated Codex login returned an already-enrolled OAuth refresh-token chain")
	}
	if active := activeCodexRefreshToken(); active != "" && refresh == active {
		return errors.New("isolated Codex login returned the active Codex refresh-token chain")
	}
	return nil
}

func snapshotStateTree(root string) (map[string]string, error) {
	snapshot := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) && path == root {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		kind := info.Mode().String()
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256([]byte(target))
			snapshot[rel] = fmt.Sprintf("%s:%x", kind, sum)
			return nil
		}
		if entry.Type().IsRegular() {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(body)
			snapshot[rel] = fmt.Sprintf("%s:%x", kind, sum)
			return nil
		}
		snapshot[rel] = kind
		return nil
	})
	return snapshot, err
}

func equalStateTreeSnapshots(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for path, value := range left {
		if right[path] != value {
			return false
		}
	}
	return true
}

func verifyEnrollmentSourcesUnchanged(originalActive activeCodexAuthSnapshot, retiringRoot string, retiringBefore map[string]string) error {
	unchanged, err := originalActive.unchanged()
	if err != nil {
		return fmt.Errorf("verify local Codex auth: %w", err)
	}
	if !unchanged {
		return errors.New("local Codex auth changed unexpectedly during isolated enrollment")
	}
	retiringAfter, err := snapshotStateTree(retiringRoot)
	if err != nil {
		return fmt.Errorf("verify retiring state tree: %w", err)
	}
	if !equalStateTreeSnapshots(retiringBefore, retiringAfter) {
		return errors.New("retiring Codex account store changed during isolated enrollment")
	}
	return nil
}

func codexEnrollmentComplete(inventory codexEnrollmentInventory) bool {
	return len(inventory.candidateByEmail) == len(inventory.retiringByEmail)
}

func codexEnrollmentTargets(
	inventory codexEnrollmentInventory,
	selectors []string,
) ([]accounts.StoredCodexAccount, error) {
	if len(selectors) == 0 {
		return append([]accounts.StoredCodexAccount(nil), inventory.retiring...), nil
	}
	targets := make([]accounts.StoredCodexAccount, 0, len(selectors))
	seen := make(map[string]struct{}, len(selectors))
	for _, raw := range selectors {
		selector := strings.ToLower(strings.TrimSpace(raw))
		if selector == "" {
			return nil, errors.New("--only account selector must not be empty")
		}
		if _, duplicate := seen[selector]; duplicate {
			return nil, fmt.Errorf("duplicate --only account selector %q", strings.TrimSpace(raw))
		}
		target, ok := inventory.retiringByEmail[selector]
		if !ok {
			return nil, fmt.Errorf("--only account %q is not a retiring Codex OAuth identity", strings.TrimSpace(raw))
		}
		seen[selector] = struct{}{}
		targets = append(targets, target)
	}
	return targets, nil
}

func selectedCodexEnrollmentComplete(
	inventory codexEnrollmentInventory,
	targets []accounts.StoredCodexAccount,
) bool {
	for _, target := range targets {
		selector := strings.ToLower(strings.TrimSpace(target.Email))
		if _, ok := inventory.candidateByEmail[selector]; !ok {
			return false
		}
	}
	return true
}

func (r srRunner) enrollCodexIsolation(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet(codexIsolatedEnrollmentCommand, flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	retiringStateDir := flags.String("retiring-state-dir", "", "read the retiring service state root")
	deviceAuth := flags.Bool("device-auth", false, "use codex login --device-auth")
	var only stringList
	flags.Var(&only, "only", "enroll one retiring Codex OAuth identity (repeatable; partial candidates cannot pass full activation preflight)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*retiringStateDir) == "" {
		return fmt.Errorf("usage: %s --retiring-state-dir PATH [--device-auth] [--only ACCOUNT]...", codexIsolatedEnrollmentCommand)
	}
	candidateRoot, err := normalizeStateRoot(storepath.StateDir())
	if err != nil {
		return fmt.Errorf("normalize candidate state root: %w", err)
	}
	retiringRoot, err := normalizeStateRoot(*retiringStateDir)
	if err != nil {
		return fmt.Errorf("normalize retiring state root: %w", err)
	}
	if err := validateEnrollmentRoots(candidateRoot, retiringRoot); err != nil {
		return err
	}
	candidateStore := rawCodexStoreForStateRoot(candidateRoot)
	retiringStore := rawCodexStoreForStateRoot(retiringRoot)
	inventory, err := inspectCodexEnrollmentInventories(candidateStore, retiringStore)
	if err != nil {
		return err
	}
	targets, err := codexEnrollmentTargets(inventory, only)
	if err != nil {
		return err
	}
	originalActive, err := snapshotActiveCodexAuth()
	if err != nil {
		return fmt.Errorf("snapshot local Codex auth: %w", err)
	}
	retiringBefore, err := snapshotStateTree(retiringStore.Dir)
	if err != nil {
		return fmt.Errorf("snapshot retiring state tree: %w", err)
	}

	pending := make([]accounts.StoredCodexAccount, 0, len(targets))
	for _, target := range targets {
		if _, enrolled := inventory.candidateByEmail[strings.ToLower(strings.TrimSpace(target.Email))]; !enrolled {
			pending = append(pending, target)
		}
	}
	if len(pending) == 0 {
		if err := verifyEnrollmentSourcesUnchanged(originalActive, retiringStore.Dir, retiringBefore); err != nil {
			return err
		}
		if codexEnrollmentComplete(inventory) {
			fmt.Fprintln(r.out, "Candidate already contains the complete isolated Codex account inventory. No enrollment needed.")
			return nil
		}
		if len(only) == 0 || !selectedCodexEnrollmentComplete(inventory, targets) {
			return errCodexIsolationCheckFailed
		}
		fmt.Fprintf(r.out, "Selected Codex OAuth account(s) are already enrolled. Candidate contains %d of %d retiring Codex OAuth identities. Full activation preflight remains blocked until the inventory is complete.\n", len(inventory.candidateByEmail), len(inventory.retiringByEmail))
		return nil
	}

	if len(only) == 0 {
		fmt.Fprintf(r.out, "Found %d retiring Codex OAuth account(s); %d require isolated enrollment.\n", len(inventory.retiring), len(pending))
	} else {
		fmt.Fprintf(r.out, "Selected %d of %d retiring Codex OAuth account(s); %d require isolated enrollment.\n", len(targets), len(inventory.retiring), len(pending))
	}
	fmt.Fprintln(r.out, "Each login uses a temporary CODEX_HOME; local Codex auth and the retiring state tree will not be changed.")
	for index, target := range pending {
		fmt.Fprintf(r.out, "\nSign in as %s (%d/%d).\n", target.Email, index+1, len(pending))
		auth, email, err := r.isolatedCodexLogin(ctx, *deviceAuth)
		if err != nil {
			return fmt.Errorf("isolated login for %s: %w", target.Email, err)
		}
		if err := verifyEnrollmentSourcesUnchanged(originalActive, retiringStore.Dir, retiringBefore); err != nil {
			return err
		}
		inventory, err = inspectCodexEnrollmentInventories(candidateStore, retiringStore)
		if err != nil {
			return err
		}
		if err := validateFreshEnrollmentAuth(auth, email, target, inventory); err != nil {
			return err
		}

		err = proxy.PublishAccountDiskMutation(ctx, candidateStore.StoreDir(), func() (bool, error) {
			if err := verifyEnrollmentSourcesUnchanged(originalActive, retiringStore.Dir, retiringBefore); err != nil {
				return false, err
			}
			latest, err := inspectCodexEnrollmentInventories(candidateStore, retiringStore)
			if err != nil {
				return false, err
			}
			selector := strings.ToLower(strings.TrimSpace(target.Email))
			if _, exists := latest.candidateByEmail[selector]; exists {
				return false, fmt.Errorf("candidate account %s changed during enrollment; rerun to resume", target.Email)
			}
			if err := validateFreshEnrollmentAuth(auth, email, target, latest); err != nil {
				return false, err
			}
			enrolled := target
			enrolled.Auth = auth
			enrolled.Auth.RefreshFailure = nil
			enrolled.OAuthCredentialOrigin = accounts.CodexOAuthOriginIsolatedServerLogin
			enrolled.MigrationBatchID = ""
			enrolled.Breadcrumbs = nil
			if err := candidateStore.SaveStored(enrolled); err != nil {
				return false, err
			}
			return true, nil
		})
		if err != nil {
			return fmt.Errorf("publish isolated credential for %s: %w", target.Email, err)
		}
		fmt.Fprintf(r.out, "Enrolled %s (%d remaining).\n", target.Email, len(pending)-index-1)
	}

	if err := verifyEnrollmentSourcesUnchanged(originalActive, retiringStore.Dir, retiringBefore); err != nil {
		return err
	}
	inventory, err = inspectCodexEnrollmentInventories(candidateStore, retiringStore)
	if err != nil {
		return err
	}
	if !selectedCodexEnrollmentComplete(inventory, targets) {
		return fmt.Errorf("selected Codex isolation comparison failed: %w", errCodexIsolationCheckFailed)
	}
	if !codexEnrollmentComplete(inventory) {
		if len(only) == 0 {
			return fmt.Errorf("final Codex isolation comparison failed: %w", errCodexIsolationCheckFailed)
		}
		fmt.Fprintf(r.out, "\nEnrolled %d selected Codex OAuth account(s). Candidate contains %d of %d retiring Codex OAuth identities; local Codex auth and the retiring account store are unchanged. This partial candidate is suitable for offline or canary validation, but full activation preflight remains blocked until the inventory is complete.\n", len(pending), len(inventory.candidateByEmail), len(inventory.retiringByEmail))
		return nil
	}
	fmt.Fprintf(r.out, "\nEnrolled %d Codex OAuth account(s). Candidate and retiring Codex OAuth inventories match; local Codex auth and the retiring account store are unchanged.\n", len(pending))
	return nil
}
