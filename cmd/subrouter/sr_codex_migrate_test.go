package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func saveLegacyCodexAccount(t *testing.T, store accounts.CodexStore, email, accountID, refresh string) {
	t.Helper()
	auth := testCodexAuth(email, accountID)
	auth.Tokens.RefreshToken = refresh
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   email,
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    auth,
	}); err != nil {
		t.Fatal(err)
	}
}

func (r *recordingSRCommandRunner) commandCount(parts ...string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, command := range r.commands {
		if commandHasPrefix(command, parts) {
			count++
		}
	}
	return count
}

func TestCodexAccountCommandsAreReservedWithoutHijackingCodexLauncher(t *testing.T) {
	for _, test := range []struct {
		args []string
		want bool
	}{
		{[]string{"codex", "migrate-isolation"}, true},
		{[]string{"codex", "migrate-isolation", "--device-auth"}, true},
		{[]string{"codex", "enroll-isolated", "--retiring-state-dir", "/tmp/retiring"}, true},
		{[]string{"codex", "isolation-check"}, true},
		{[]string{"codex", "isolation-check", "--json"}, true},
		{[]string{"codex", "attest-legacy"}, true},
		{[]string{"codex", "attest-legacy", "--state-dir", "/var/lib/subrouter", "--dry-run"}, true},
		{[]string{"codex"}, false},
		{[]string{"codex", "resume", "thread-id"}, false},
		{[]string{"status"}, false},
	} {
		if got := isCodexAccountCommand(test.args); got != test.want {
			t.Fatalf("isCodexAccountCommand(%q) = %v, want %v", test.args, got, test.want)
		}
	}
}

func TestCodexEnrollIsolatedDispatchDoesNotImportLegacyStore(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	candidateRoot := filepath.Join(root, "candidate")
	retiringRoot := filepath.Join(root, "retiring")
	legacyRoot := filepath.Join(home, ".codex-accounts")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("SUBROUTER_STATE_DIR", candidateRoot)

	legacy := accounts.CodexStore{Dir: legacyRoot}
	saveLegacyCodexAccount(t, legacy, "legacy@example.com", "acct-legacy", "legacy-refresh")
	retiring := rawCodexStoreForStateRoot(retiringRoot)
	saveLegacyCodexAccount(t, retiring, "alpha@example.com", "acct-alpha", "retiring-alpha")

	store := codexStoreForCommand([]string{"codex", "enroll-isolated", "--retiring-state-dir", retiringRoot})
	if store.Dir != filepath.Join(candidateRoot, "codex", "accounts") {
		t.Fatalf("candidate store = %q, want raw candidate root", store.Dir)
	}
	if stored, err := store.ListStoredReadOnly(); err != nil || len(stored) != 0 {
		t.Fatalf("candidate store before enrollment = %d accounts, err=%v", len(stored), err)
	}
	if _, err := os.Stat(filepath.Join(candidateRoot, "migration.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected legacy migration marker: %v", err)
	}
}

func TestCodexEnrollIsolatedResumesSafeSubsetAndPreservesSources(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	candidateRoot := filepath.Join(root, "candidate")
	retiringRoot := filepath.Join(root, "retiring")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("SUBROUTER_STATE_DIR", candidateRoot)

	active := testCodexAuth("interactive@example.com", "acct-interactive")
	active.Tokens.RefreshToken = "active-refresh"
	if err := accounts.WriteActiveCodexAuth(active); err != nil {
		t.Fatal(err)
	}
	activeBefore, err := os.ReadFile(accounts.DefaultCodexAuthPath())
	if err != nil {
		t.Fatal(err)
	}
	retiring := rawCodexStoreForStateRoot(retiringRoot)
	saveLegacyCodexAccount(t, retiring, "alpha@example.com", "acct-alpha", "retiring-alpha")
	saveLegacyCodexAccount(t, retiring, "beta@example.com", "acct-beta", "retiring-beta")
	if err := retiring.SaveStored(accounts.StoredCodexAccount{
		Email: "qwen:unrelated", Provider: accounts.ProviderQwen,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "unrelated-key"},
	}); err != nil {
		t.Fatal(err)
	}
	retiringBefore := snapshotTestTree(t, retiringRoot)

	candidate := rawCodexStoreForStateRoot(candidateRoot)
	alpha := testCodexAuth("alpha@example.com", "acct-alpha")
	alpha.Tokens.RefreshToken = "candidate-alpha"
	if err := candidate.SaveStored(accounts.StoredCodexAccount{
		Email:                 "alpha@example.com",
		OAuthCredentialOrigin: accounts.CodexOAuthOriginIsolatedServerLogin,
		Auth:                  alpha,
	}); err != nil {
		t.Fatal(err)
	}
	beta := testCodexAuth("beta@example.com", "acct-beta")
	beta.Tokens.RefreshToken = "candidate-beta"
	fake := &recordingSRCommandRunner{loginAuth: beta, onLogin: func(_ []string) {
		liveDir := filepath.Join(retiringRoot, "sessions")
		if err := os.MkdirAll(liveDir, 0o700); err != nil {
			t.Error(err)
			return
		}
		if err := os.WriteFile(filepath.Join(liveDir, "live.json"), []byte("changing live state"), 0o600); err != nil {
			t.Error(err)
		}
	}}
	var out bytes.Buffer
	runner := srRunner{store: accounts.DefaultCodexStore(), in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}
	if err := runner.codexAccount(context.Background(), []string{
		"enroll-isolated", "--retiring-state-dir", retiringRoot, "--device-auth",
	}); err != nil {
		t.Fatal(err)
	}
	if got := fake.commandCount("codex", "login", "--device-auth"); got != 1 {
		t.Fatalf("login count = %d, want one missing account", got)
	}
	inventory, err := inspectCodexEnrollmentInventories(candidate, retiring)
	if err != nil || !codexEnrollmentComplete(inventory) {
		t.Fatalf("final Codex OAuth inventory complete=%v, err=%v", codexEnrollmentComplete(inventory), err)
	}
	activeAfter, err := os.ReadFile(accounts.DefaultCodexAuthPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(activeBefore, activeAfter) {
		t.Fatal("isolated enrollment changed interactive auth")
	}
	retiringAccountsBefore := filteredTreeSnapshot(retiringBefore, filepath.Join("codex", "accounts"))
	retiringAccountsAfter := filteredTreeSnapshot(snapshotTestTree(t, retiringRoot), filepath.Join("codex", "accounts"))
	if !reflect.DeepEqual(retiringAccountsAfter, retiringAccountsBefore) {
		t.Fatalf("retiring account store changed: before=%v after=%v", retiringAccountsBefore, retiringAccountsAfter)
	}
	if !strings.Contains(out.String(), "1 require isolated enrollment") || !strings.Contains(out.String(), "inventories match") {
		t.Fatalf("unexpected output:\n%s", out.String())
	}
}

func TestCodexEnrollIsolatedOnlyBuildsExplicitPartialCandidate(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	candidateRoot := filepath.Join(root, "candidate")
	retiringRoot := filepath.Join(root, "retiring")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("SUBROUTER_STATE_DIR", candidateRoot)
	if err := accounts.WriteActiveCodexAuth(testCodexAuth("interactive@example.com", "acct-interactive")); err != nil {
		t.Fatal(err)
	}
	retiring := rawCodexStoreForStateRoot(retiringRoot)
	saveLegacyCodexAccount(t, retiring, "alpha@example.com", "acct-alpha", "retiring-alpha")
	saveLegacyCodexAccount(t, retiring, "beta@example.com", "acct-beta", "retiring-beta")
	retiringBefore := snapshotTestTree(t, retiringRoot)

	beta := testCodexAuth("beta@example.com", "acct-beta")
	beta.Tokens.RefreshToken = "candidate-beta"
	fake := &recordingSRCommandRunner{loginAuth: beta}
	var out bytes.Buffer
	runner := srRunner{store: accounts.DefaultCodexStore(), in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}
	if err := runner.codexAccount(context.Background(), []string{
		"enroll-isolated", "--retiring-state-dir", retiringRoot,
		"--only", "beta@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if got := fake.commandCount("codex", "login"); got != 1 {
		t.Fatalf("login count = %d, want one", got)
	}
	stored, err := rawCodexStoreForStateRoot(candidateRoot).ListStoredReadOnly()
	if err != nil || len(stored) != 1 || stored[0].Email != "beta@example.com" {
		t.Fatalf("candidate accounts = %#v, err=%v", stored, err)
	}
	for _, want := range []string{"Selected 1 of 2", "partial candidate", "full activation preflight remains blocked"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
	if after := snapshotTestTree(t, retiringRoot); !reflect.DeepEqual(after, retiringBefore) {
		t.Fatalf("partial enrollment changed retiring state: before=%v after=%v", retiringBefore, after)
	}

	var checkOut bytes.Buffer
	checkRunner := srRunner{store: rawCodexStoreForStateRoot(candidateRoot), in: strings.NewReader(""), out: &checkOut, errOut: &checkOut}
	checkErr := checkRunner.codexAccount(context.Background(), []string{
		"isolation-check", "--json", "--retiring-state-dir", retiringRoot,
	})
	if !errors.Is(checkErr, errCodexIsolationCheckFailed) {
		t.Fatalf("full isolation check error = %v, want activation-blocking failure", checkErr)
	}
	var result codexIsolationCheckResult
	if err := json.Unmarshal(checkOut.Bytes(), &result); err != nil {
		t.Fatalf("decode full isolation result %q: %v", checkOut.String(), err)
	}
	if result.OK || result.Comparison == nil || result.Comparison.AccountInventoryMatch {
		t.Fatalf("partial candidate unexpectedly passed full comparison: %#v", result)
	}
}

func TestCodexEnrollIsolatedOnlyRejectsInvalidSelectionBeforeLogin(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "unknown", args: []string{"--only", "missing@example.com"}, want: "not a retiring Codex OAuth identity"},
		{name: "duplicate", args: []string{"--only", "alpha@example.com", "--only", "ALPHA@example.com"}, want: "duplicate --only"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			candidateRoot := filepath.Join(root, "candidate")
			retiringRoot := filepath.Join(root, "retiring")
			t.Setenv("HOME", filepath.Join(root, "home"))
			t.Setenv("SUBROUTER_STATE_DIR", candidateRoot)
			retiring := rawCodexStoreForStateRoot(retiringRoot)
			saveLegacyCodexAccount(t, retiring, "alpha@example.com", "acct-alpha", "retiring-alpha")
			before := snapshotTestTree(t, root)
			fake := &recordingSRCommandRunner{}
			runner := srRunner{store: accounts.DefaultCodexStore(), in: strings.NewReader(""), out: io.Discard, errOut: io.Discard, cmd: fake}
			args := append([]string{"enroll-isolated", "--retiring-state-dir", retiringRoot}, test.args...)
			err := runner.codexAccount(context.Background(), args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if got := fake.commandCount("codex", "login"); got != 0 {
				t.Fatalf("login count = %d, want zero", got)
			}
			if after := snapshotTestTree(t, root); !reflect.DeepEqual(after, before) {
				t.Fatalf("invalid selection mutated state: before=%v after=%v", before, after)
			}
		})
	}
}

func TestCodexEnrollIsolatedOnlyResumesCompletedSelection(t *testing.T) {
	root := t.TempDir()
	candidateRoot := filepath.Join(root, "candidate")
	retiringRoot := filepath.Join(root, "retiring")
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", candidateRoot)
	retiring := rawCodexStoreForStateRoot(retiringRoot)
	saveLegacyCodexAccount(t, retiring, "alpha@example.com", "acct-alpha", "retiring-alpha")
	saveLegacyCodexAccount(t, retiring, "beta@example.com", "acct-beta", "retiring-beta")
	candidate := rawCodexStoreForStateRoot(candidateRoot)
	alpha := testCodexAuth("alpha@example.com", "acct-alpha")
	alpha.Tokens.RefreshToken = "candidate-alpha"
	if err := candidate.SaveStored(accounts.StoredCodexAccount{
		Email:                 "alpha@example.com",
		Provider:              accounts.ProviderCodex,
		OAuthCredentialOrigin: accounts.CodexOAuthOriginIsolatedServerLogin,
		Auth:                  alpha,
	}); err != nil {
		t.Fatal(err)
	}
	fake := &recordingSRCommandRunner{}
	var out bytes.Buffer
	runner := srRunner{store: candidate, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}
	if err := runner.codexAccount(context.Background(), []string{
		"enroll-isolated", "--retiring-state-dir", retiringRoot,
		"--only", "alpha@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if got := fake.commandCount("codex", "login"); got != 0 {
		t.Fatalf("login count = %d, want zero", got)
	}
	for _, want := range []string{"already enrolled", "contains 1 of 2", "Full activation preflight remains blocked"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
}

func filteredTreeSnapshot(snapshot map[string]string, prefix string) map[string]string {
	filtered := make(map[string]string)
	for path, value := range snapshot {
		if path == prefix || strings.HasPrefix(path, prefix+string(filepath.Separator)) {
			filtered[path] = value
		}
	}
	return filtered
}

func TestCodexEnrollIsolatedRejectsRelatedRootsBeforeLogin(t *testing.T) {
	root := t.TempDir()
	candidateRoot := filepath.Join(root, "candidate")
	retiringRoot := filepath.Join(candidateRoot, "retiring")
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", candidateRoot)
	retiring := rawCodexStoreForStateRoot(retiringRoot)
	saveLegacyCodexAccount(t, retiring, "alpha@example.com", "acct-alpha", "retiring-alpha")
	before := snapshotTestTree(t, root)
	fake := &recordingSRCommandRunner{}
	runner := srRunner{store: accounts.DefaultCodexStore(), in: strings.NewReader(""), out: io.Discard, errOut: io.Discard, cmd: fake}
	err := runner.codexAccount(context.Background(), []string{"enroll-isolated", "--retiring-state-dir", retiringRoot})
	if err == nil || !strings.Contains(err.Error(), "must not be nested") {
		t.Fatalf("error = %v, want nested-root rejection", err)
	}
	if got := fake.commandCount("codex", "login"); got != 0 {
		t.Fatalf("login count = %d, want zero", got)
	}
	if after := snapshotTestTree(t, root); !reflect.DeepEqual(after, before) {
		t.Fatalf("root rejection mutated state: before=%v after=%v", before, after)
	}
}

func TestCodexEnrollIsolatedRejectsWrongImmutableIdentityBeforeSave(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	candidateRoot := filepath.Join(root, "candidate")
	retiringRoot := filepath.Join(root, "retiring")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("SUBROUTER_STATE_DIR", candidateRoot)
	active := testCodexAuth("interactive@example.com", "acct-interactive")
	if err := accounts.WriteActiveCodexAuth(active); err != nil {
		t.Fatal(err)
	}
	retiring := rawCodexStoreForStateRoot(retiringRoot)
	saveLegacyCodexAccount(t, retiring, "alpha@example.com", "acct-alpha", "retiring-alpha")
	wrong := testCodexAuth("alpha@example.com", "acct-other")
	wrong.Tokens.RefreshToken = "candidate-wrong"
	fake := &recordingSRCommandRunner{loginAuth: wrong}
	runner := srRunner{store: accounts.DefaultCodexStore(), in: strings.NewReader(""), out: io.Discard, errOut: io.Discard, cmd: fake}
	err := runner.codexAccount(context.Background(), []string{"enroll-isolated", "--retiring-state-dir", retiringRoot})
	if err == nil || !strings.Contains(err.Error(), "immutable account identity") {
		t.Fatalf("error = %v, want immutable-identity rejection", err)
	}
	stored, findErr := rawCodexStoreForStateRoot(candidateRoot).ListStoredReadOnly()
	if findErr != nil || len(stored) != 0 {
		t.Fatalf("candidate stored accounts = %d, err=%v", len(stored), findErr)
	}
}

func TestCodexEnrollIsolatedRejectsNonCodexSelectorCollisionBeforeLogin(t *testing.T) {
	root := t.TempDir()
	candidateRoot := filepath.Join(root, "candidate")
	retiringRoot := filepath.Join(root, "retiring")
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", candidateRoot)

	retiring := rawCodexStoreForStateRoot(retiringRoot)
	saveLegacyCodexAccount(t, retiring, "alpha@example.com", "acct-alpha", "retiring-alpha")
	candidate := rawCodexStoreForStateRoot(candidateRoot)
	if err := candidate.SaveStored(accounts.StoredCodexAccount{
		Email:    "ALPHA@example.com",
		Provider: accounts.ProviderQwen,
		Auth:     accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "qwen-key"},
	}); err != nil {
		t.Fatal(err)
	}
	before := snapshotTestTree(t, candidateRoot)
	fake := &recordingSRCommandRunner{}
	runner := srRunner{store: candidate, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard, cmd: fake}
	err := runner.codexAccount(context.Background(), []string{"enroll-isolated", "--retiring-state-dir", retiringRoot})
	if err == nil || !strings.Contains(err.Error(), "colliding with a retiring Codex account selector") {
		t.Fatalf("error = %v, want selector-collision rejection", err)
	}
	if got := fake.commandCount("codex", "login"); got != 0 {
		t.Fatalf("login count = %d, want zero", got)
	}
	if after := snapshotTestTree(t, candidateRoot); !reflect.DeepEqual(after, before) {
		t.Fatalf("collision rejection mutated candidate: before=%v after=%v", before, after)
	}
}

func TestCodexEnrollIsolatedRejectsRetiringAccountDriftBeforeSave(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	candidateRoot := filepath.Join(root, "candidate")
	retiringRoot := filepath.Join(root, "retiring")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("SUBROUTER_STATE_DIR", candidateRoot)
	if err := accounts.WriteActiveCodexAuth(testCodexAuth("interactive@example.com", "acct-interactive")); err != nil {
		t.Fatal(err)
	}
	retiring := rawCodexStoreForStateRoot(retiringRoot)
	saveLegacyCodexAccount(t, retiring, "alpha@example.com", "acct-alpha", "retiring-alpha")
	fresh := testCodexAuth("alpha@example.com", "acct-alpha")
	fresh.Tokens.RefreshToken = "candidate-alpha"
	fake := &recordingSRCommandRunner{loginAuth: fresh, onLogin: func(_ []string) {
		account, ok, err := retiring.FindStored("alpha@example.com")
		if err != nil || !ok {
			t.Errorf("find retiring account: ok=%v err=%v", ok, err)
			return
		}
		account.Label = "changed during login"
		if err := retiring.SaveStored(account); err != nil {
			t.Error(err)
		}
	}}
	runner := srRunner{store: accounts.DefaultCodexStore(), in: strings.NewReader(""), out: io.Discard, errOut: io.Discard, cmd: fake}
	err := runner.codexAccount(context.Background(), []string{"enroll-isolated", "--retiring-state-dir", retiringRoot})
	if err == nil || !strings.Contains(err.Error(), "retiring Codex account store changed") {
		t.Fatalf("error = %v, want retiring account drift rejection", err)
	}
	stored, findErr := rawCodexStoreForStateRoot(candidateRoot).ListStoredReadOnly()
	if findErr != nil || len(stored) != 0 {
		t.Fatalf("candidate stored accounts = %d, err=%v", len(stored), findErr)
	}
}

func runCodexIsolationJSONCheck(t *testing.T, store accounts.CodexStore) (codexIsolationCheckResult, error) {
	t.Helper()
	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	err := runner.codexAccount(context.Background(), []string{"isolation-check", "--json"})
	var result codexIsolationCheckResult
	if decodeErr := json.Unmarshal(out.Bytes(), &result); decodeErr != nil {
		t.Fatalf("decode isolation check %q: %v", out.String(), decodeErr)
	}
	return result, err
}

func TestCodexIsolationJSONCheckRejectsMissingOriginWithoutMutation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.DefaultCodexStore()
	saveLegacyCodexAccount(t, store, "legacy@example.com", "acct-legacy", "legacy-refresh")
	path := filepath.Join(store.Dir, "legacy@example.com.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	result, checkErr := runCodexIsolationJSONCheck(t, store)
	if !errors.Is(checkErr, errCodexIsolationCheckFailed) {
		t.Fatalf("check error = %v, want isolation failure", checkErr)
	}
	if result.SchemaVersion != 1 || result.OK || result.AccountsRequiringMigration != 1 || result.Remediation != codexIsolationRemediation {
		t.Fatalf("check result = %#v", result)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("isolation check mutated the stored credential")
	}
}

func TestCodexIsolationJSONCheckAcceptsTrustedOrigin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.DefaultCodexStore()
	auth := testCodexAuth("isolated@example.com", "acct-isolated")
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:                 "isolated@example.com",
		OAuthCredentialOrigin: accounts.CodexOAuthOriginIsolatedServerLogin,
		Auth:                  auth,
	}); err != nil {
		t.Fatal(err)
	}

	result, err := runCodexIsolationJSONCheck(t, store)
	if err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != 1 || !result.OK || result.AccountsRequiringMigration != 0 || result.Remediation != "" {
		t.Fatalf("check result = %#v", result)
	}
}

func TestCodexIsolationJSONCheckRejectsTrustedOriginSharingInteractiveChain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.DefaultCodexStore()
	auth := testCodexAuth("shared@example.com", "acct-shared")
	auth.Tokens.RefreshToken = "shared-refresh"
	if err := accounts.WriteActiveCodexAuth(auth); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:                 "shared@example.com",
		OAuthCredentialOrigin: accounts.CodexOAuthOriginServerAttested,
		Auth:                  auth,
	}); err != nil {
		t.Fatal(err)
	}

	result, err := runCodexIsolationJSONCheck(t, store)
	if !errors.Is(err, errCodexIsolationCheckFailed) {
		t.Fatalf("check error = %v, want isolation failure", err)
	}
	if result.OK || result.AccountsRequiringMigration != 1 || result.Remediation != codexIsolationRemediation {
		t.Fatalf("check result = %#v", result)
	}
}

func TestCodexIsolationJSONCheckRemainsAvailableWithMalformedRoutingConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	configPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runner := srRunner{
		store: accounts.DefaultCodexStore(), in: strings.NewReader(""), out: &out, errOut: &out,
	}
	if err := runner.run(context.Background(), []string{"codex", "isolation-check", "--json"}); err != nil {
		t.Fatal(err)
	}
	var result codexIsolationCheckResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.AccountsRequiringMigration != 0 {
		t.Fatalf("check result = %#v", result)
	}
}

func TestSubrouterProgramDispatchesDocumentedCodexIsolationCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	if err := runForProgram("subrouter", []string{"codex", "migrate-isolation"}); err != nil {
		t.Fatalf("documented subrouter migration command: %v", err)
	}
}

func TestCodexIsolationCheckEmptyStateDoesNotImportLegacyArchive(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	legacy := filepath.Join(home, ".codex-accounts")
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "login.log"), []byte("synthetic legacy login log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("SUBROUTER_STATE_DIR", stateDir)

	if err := runForProgram("subrouter", []string{"codex", "isolation-check", "--json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("isolation check created or migrated state: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(legacy, "login.log")); err != nil || string(body) != "synthetic legacy login log\n" {
		t.Fatalf("legacy archive changed: body=%q err=%v", body, err)
	}
}

func TestCodexIsolationCheckRejectsUntrustedLegacySourceWithoutMutation(t *testing.T) {
	home, stateDir, legacyStore := isolationLegacyTestState(t)
	saveLegacyCodexAccount(t, legacyStore, "legacy@example.com", "acct-legacy", "legacy-refresh")
	before := snapshotTestTree(t, home)

	store := accounts.DefaultCodexStoreForReadOnlyInspection()
	if store.Dir != legacyStore.Dir {
		t.Fatalf("inspection store = %q, want effective legacy source %q", store.Dir, legacyStore.Dir)
	}
	result, err := runCodexIsolationJSONCheck(t, store)
	if !errors.Is(err, errCodexIsolationCheckFailed) || result.OK || result.AccountsRequiringMigration != 1 {
		t.Fatalf("legacy isolation result=%#v err=%v, want one migration", result, err)
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("isolation check created candidate state: %v", err)
	}
	if after := snapshotTestTree(t, home); !reflect.DeepEqual(after, before) {
		t.Fatalf("legacy source changed: before=%v after=%v", before, after)
	}
}

func TestCodexIsolationCheckAcceptsTrustedLegacySourceWithoutMutation(t *testing.T) {
	home, stateDir, legacyStore := isolationLegacyTestState(t)
	auth := testCodexAuth("trusted@example.com", "acct-trusted")
	if err := legacyStore.SaveStored(accounts.StoredCodexAccount{
		Email:                 "trusted@example.com",
		OAuthCredentialOrigin: accounts.CodexOAuthOriginIsolatedServerLogin,
		Auth:                  auth,
	}); err != nil {
		t.Fatal(err)
	}
	before := snapshotTestTree(t, home)

	result, err := runCodexIsolationJSONCheck(t, accounts.DefaultCodexStoreForReadOnlyInspection())
	if err != nil || !result.OK || result.AccountsRequiringMigration != 0 {
		t.Fatalf("trusted legacy isolation result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("isolation check created candidate state: %v", err)
	}
	if after := snapshotTestTree(t, home); !reflect.DeepEqual(after, before) {
		t.Fatalf("legacy source changed: before=%v after=%v", before, after)
	}
}

func TestCodexIsolationCheckNonemptyCandidateWinsOverLegacy(t *testing.T) {
	home, stateDir, legacyStore := isolationLegacyTestState(t)
	saveLegacyCodexAccount(t, legacyStore, "legacy@example.com", "acct-legacy", "legacy-refresh")
	targetStore := accounts.CodexStore{Dir: filepath.Join(stateDir, "codex", "accounts")}
	auth := testCodexAuth("target@example.com", "acct-target")
	if err := targetStore.SaveStored(accounts.StoredCodexAccount{
		Email:                 "target@example.com",
		OAuthCredentialOrigin: accounts.CodexOAuthOriginIsolatedServerLogin,
		Auth:                  auth,
	}); err != nil {
		t.Fatal(err)
	}
	before := snapshotTestTree(t, filepath.Dir(home))

	store := accounts.DefaultCodexStoreForReadOnlyInspection()
	if store.Dir != targetStore.Dir {
		t.Fatalf("inspection store = %q, want nonempty candidate %q", store.Dir, targetStore.Dir)
	}
	result, err := runCodexIsolationJSONCheck(t, store)
	if err != nil || !result.OK || result.AccountsRequiringMigration != 0 {
		t.Fatalf("candidate isolation result=%#v err=%v", result, err)
	}
	if after := snapshotTestTree(t, filepath.Dir(home)); !reflect.DeepEqual(after, before) {
		t.Fatalf("credential sources changed: before=%v after=%v", before, after)
	}
}

func TestCodexIsolationComparisonFailsClosedAndDoesNotMutate(t *testing.T) {
	tests := []struct {
		name                            string
		setup                           func(*testing.T, accounts.CodexStore, accounts.CodexStore)
		sameRoot                        bool
		legacyAPIKey                    bool
		wantOK                          bool
		wantCandidate                   int
		wantRetiring                    int
		wantInventory                   bool
		wantRootsDistinct               bool
		wantStoreAnchored               bool
		wantStoresDistinct              bool
		wantShared                      int
		wantOriginsTrusted              bool
		wantMissingRefresh              int
		wantDuplicateChains             int
		wantCandidateDuplicateSelectors int
		wantRetiringDuplicateSelectors  int
	}{
		{name: "zero inventory", wantRootsDistinct: true, wantStoreAnchored: true, wantStoresDistinct: true, wantOriginsTrusted: true},
		{
			name: "incomplete candidate", wantCandidate: 1, wantRetiring: 2, wantRootsDistinct: true, wantStoreAnchored: true, wantStoresDistinct: true, wantOriginsTrusted: true,
			setup: func(t *testing.T, candidate, retiring accounts.CodexStore) {
				saveComparisonCodexAccount(t, candidate, "account-a", "candidate-a", accounts.CodexOAuthOriginIsolatedServerLogin)
				saveComparisonCodexAccount(t, retiring, "account-a", "retiring-a", "")
				saveComparisonCodexAccount(t, retiring, "account-b", "retiring-b", "")
			},
		},
		{
			name: "extra candidate", wantCandidate: 2, wantRetiring: 1, wantRootsDistinct: true, wantStoreAnchored: true, wantStoresDistinct: true, wantOriginsTrusted: true,
			setup: func(t *testing.T, candidate, retiring accounts.CodexStore) {
				saveComparisonCodexAccount(t, candidate, "account-a", "candidate-a", accounts.CodexOAuthOriginIsolatedServerLogin)
				saveComparisonCodexAccount(t, candidate, "account-b", "candidate-b", accounts.CodexOAuthOriginServerAttested)
				saveComparisonCodexAccount(t, retiring, "account-a", "retiring-a", "")
			},
		},
		{
			name: "same root", sameRoot: true, wantCandidate: 1, wantRetiring: 1, wantInventory: true, wantStoreAnchored: true, wantShared: 1, wantOriginsTrusted: true,
			setup: func(t *testing.T, candidate, _ accounts.CodexStore) {
				saveComparisonCodexAccount(t, candidate, "account-a", "same-chain", accounts.CodexOAuthOriginIsolatedServerLogin)
			},
		},
		{
			name: "shared refresh chain", wantCandidate: 1, wantRetiring: 1, wantInventory: true, wantRootsDistinct: true, wantStoreAnchored: true, wantStoresDistinct: true, wantShared: 1, wantOriginsTrusted: true,
			setup: func(t *testing.T, candidate, retiring accounts.CodexStore) {
				saveComparisonCodexAccount(t, candidate, "account-a", "shared-chain", accounts.CodexOAuthOriginIsolatedServerLogin)
				saveComparisonCodexAccount(t, retiring, "account-a", "shared-chain", "")
			},
		},
		{
			name: "untrusted candidate origin", wantCandidate: 1, wantRetiring: 1, wantInventory: true, wantRootsDistinct: true, wantStoreAnchored: true, wantStoresDistinct: true,
			setup: func(t *testing.T, candidate, retiring accounts.CodexStore) {
				saveComparisonCodexAccount(t, candidate, "account-a", "candidate-a", "")
				saveComparisonCodexAccount(t, retiring, "account-a", "retiring-a", "")
			},
		},
		{
			name: "duplicate candidate refresh chain", wantCandidate: 2, wantRetiring: 2, wantInventory: true, wantRootsDistinct: true, wantStoreAnchored: true, wantStoresDistinct: true, wantOriginsTrusted: true, wantDuplicateChains: 1,
			setup: func(t *testing.T, candidate, retiring accounts.CodexStore) {
				saveComparisonCodexAccount(t, candidate, "account-a", "candidate-shared", accounts.CodexOAuthOriginIsolatedServerLogin)
				saveComparisonCodexAccount(t, candidate, "account-b", "candidate-shared", accounts.CodexOAuthOriginServerAttested)
				saveComparisonCodexAccount(t, retiring, "account-a", "retiring-a", "")
				saveComparisonCodexAccount(t, retiring, "account-b", "retiring-b", "")
			},
		},
		{
			name: "missing candidate refresh token", wantCandidate: 1, wantRetiring: 1, wantInventory: true, wantRootsDistinct: true, wantStoreAnchored: true, wantStoresDistinct: true, wantOriginsTrusted: true, wantMissingRefresh: 1,
			setup: func(t *testing.T, candidate, retiring accounts.CodexStore) {
				saveComparisonCodexAccount(t, candidate, "account-a", "", accounts.CodexOAuthOriginIsolatedServerLogin)
				saveComparisonCodexAccount(t, retiring, "account-a", "retiring-a", "")
			},
		},
		{
			name: "api key legacy fallback is not candidate", legacyAPIKey: true, wantCandidate: 1, wantRetiring: 1, wantInventory: true, wantRootsDistinct: true, wantStoresDistinct: true, wantOriginsTrusted: true,
		},
		{
			name: "oauth substituted by api key", wantCandidate: 1, wantRetiring: 1, wantRootsDistinct: true, wantStoreAnchored: true, wantStoresDistinct: true, wantOriginsTrusted: true,
			setup: func(t *testing.T, candidate, retiring accounts.CodexStore) {
				saveComparisonCodexAccount(t, candidate, "account-a", "candidate-a", accounts.CodexOAuthOriginIsolatedServerLogin)
				saveComparisonAPIKeyAccount(t, retiring, "account-a", "sk-retiring")
			},
		},
		{
			name: "same selector different immutable account", wantCandidate: 1, wantRetiring: 1, wantRootsDistinct: true, wantStoreAnchored: true, wantStoresDistinct: true, wantOriginsTrusted: true,
			setup: func(t *testing.T, candidate, retiring accounts.CodexStore) {
				saveComparisonCodexAccountWithIdentity(t, candidate, "account-a", "immutable-candidate", "candidate-a", accounts.CodexOAuthOriginIsolatedServerLogin)
				saveComparisonCodexAccountWithIdentity(t, retiring, "account-a", "immutable-retiring", "retiring-a", "")
			},
		},
		{
			name: "duplicate normalized selector in candidate", wantCandidate: 2, wantRetiring: 1, wantRootsDistinct: true, wantStoreAnchored: true, wantStoresDistinct: true, wantOriginsTrusted: true, wantCandidateDuplicateSelectors: 1,
			setup: func(t *testing.T, candidate, retiring accounts.CodexStore) {
				saveComparisonCodexAccount(t, candidate, "account-a", "candidate-a", accounts.CodexOAuthOriginIsolatedServerLogin)
				writeComparisonCodexAccountFile(t, candidate, "duplicate-case.json", "ACCOUNT-A", "acct-account-a", "candidate-b", accounts.CodexOAuthOriginServerAttested)
				saveComparisonCodexAccount(t, retiring, "account-a", "retiring-a", "")
			},
		},
		{
			name: "duplicate normalized selector in retiring", wantCandidate: 1, wantRetiring: 2, wantRootsDistinct: true, wantStoreAnchored: true, wantStoresDistinct: true, wantOriginsTrusted: true, wantRetiringDuplicateSelectors: 1,
			setup: func(t *testing.T, candidate, retiring accounts.CodexStore) {
				saveComparisonCodexAccount(t, candidate, "account-a", "candidate-a", accounts.CodexOAuthOriginIsolatedServerLogin)
				saveComparisonCodexAccount(t, retiring, "account-a", "retiring-a", "")
				writeComparisonCodexAccountFile(t, retiring, "duplicate-case.json", "ACCOUNT-A", "acct-account-a", "retiring-b", "")
			},
		},
		{
			name: "isolated exact match", wantOK: true, wantCandidate: 1, wantRetiring: 1, wantInventory: true, wantRootsDistinct: true, wantStoreAnchored: true, wantStoresDistinct: true, wantOriginsTrusted: true,
			setup: func(t *testing.T, candidate, retiring accounts.CodexStore) {
				saveComparisonCodexAccount(t, candidate, "account-a", "candidate-a", accounts.CodexOAuthOriginIsolatedServerLogin)
				saveComparisonCodexAccount(t, retiring, "account-a", "retiring-a", "")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "home")
			candidateRoot := filepath.Join(root, "candidate")
			retiringRoot := filepath.Join(root, "retiring")
			if test.sameRoot {
				alias := filepath.Join(root, "root-alias")
				if err := os.Symlink(root, alias); err != nil {
					t.Fatal(err)
				}
				retiringRoot = filepath.Join(alias, "candidate")
			}
			t.Setenv("HOME", home)
			t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
			t.Setenv("SUBROUTER_STATE_DIR", candidateRoot)
			if test.legacyAPIKey {
				legacy := accounts.CodexStore{Dir: filepath.Join(home, ".codex-accounts", "accounts")}
				saveComparisonAPIKeyAccount(t, legacy, "apikey:paid", "sk-legacy")
				retiring := accounts.CodexStore{Dir: filepath.Join(retiringRoot, "codex", "accounts")}
				saveComparisonAPIKeyAccount(t, retiring, "apikey:paid", "sk-retiring")
			}
			candidateStore := accounts.CodexStoreForStateRootReadOnlyInspection(candidateRoot)
			retiringStore := accounts.CodexStoreForStateRootReadOnlyInspection(retiringRoot)
			if test.setup != nil {
				test.setup(t, candidateStore, retiringStore)
			}
			before := snapshotTestTree(t, root)

			var output bytes.Buffer
			runner := srRunner{store: candidateStore, out: &output, errOut: &output}
			err := runner.codexAccount(context.Background(), []string{
				"isolation-check", "--json", "--retiring-state-dir", retiringRoot,
			})
			var result codexIsolationCheckResult
			if decodeErr := json.Unmarshal(output.Bytes(), &result); decodeErr != nil {
				t.Fatalf("decode comparison %q: %v", output.String(), decodeErr)
			}
			if test.wantOK && err != nil {
				t.Fatalf("comparison error = %v", err)
			}
			if !test.wantOK && !errors.Is(err, errCodexIsolationCheckFailed) {
				t.Fatalf("comparison error = %v, want preflight failure", err)
			}
			comparison := result.Comparison
			if comparison == nil {
				t.Fatal("comparison result missing")
			}
			if result.OK != test.wantOK || comparison.OK != test.wantOK ||
				comparison.CandidateAccountCount != test.wantCandidate ||
				comparison.RetiringAccountCount != test.wantRetiring ||
				comparison.AccountInventoryMatch != test.wantInventory ||
				comparison.RootsDistinct != test.wantRootsDistinct ||
				comparison.CandidateStoreAnchored != test.wantStoreAnchored ||
				!comparison.RetiringStoreAnchored ||
				comparison.EffectiveStoresDistinct != test.wantStoresDistinct ||
				comparison.SharedOAuthRefreshTokenCount != test.wantShared ||
				comparison.CandidateOAuthOriginsTrusted != test.wantOriginsTrusted ||
				comparison.CandidateMissingRefreshCount != test.wantMissingRefresh ||
				comparison.CandidateDuplicateChainCount != test.wantDuplicateChains ||
				comparison.CandidateDuplicateSelectorCount != test.wantCandidateDuplicateSelectors ||
				comparison.RetiringDuplicateSelectorCount != test.wantRetiringDuplicateSelectors ||
				comparison.NormalizedSelectorsUnique != (test.wantCandidateDuplicateSelectors == 0 && test.wantRetiringDuplicateSelectors == 0) {
				t.Fatalf("comparison result = %#v, top-level ok=%v", comparison, result.OK)
			}
			if after := snapshotTestTree(t, root); !reflect.DeepEqual(after, before) {
				t.Fatalf("comparison mutated state: before=%v after=%v", before, after)
			}
			for _, secret := range []string{"account-a", "account-b", "candidate-a", "candidate-b", "retiring-a", "retiring-b", "shared-chain", "candidate-shared", "immutable-candidate", "immutable-retiring", "sk-legacy", "sk-retiring", candidateRoot, retiringRoot} {
				if secret != "" && strings.Contains(output.String(), secret) {
					t.Fatalf("comparison JSON leaked %q: %s", secret, output.String())
				}
			}
		})
	}
}

func saveComparisonCodexAccount(
	t *testing.T,
	store accounts.CodexStore,
	id, refreshToken string,
	origin accounts.CodexOAuthCredentialOrigin,
) {
	t.Helper()
	saveComparisonCodexAccountWithIdentity(t, store, id, "acct-"+id, refreshToken, origin)
}

func saveComparisonCodexAccountWithIdentity(
	t *testing.T,
	store accounts.CodexStore,
	id, immutableAccountID, refreshToken string,
	origin accounts.CodexOAuthCredentialOrigin,
) {
	t.Helper()
	auth := testCodexAuth(id, immutableAccountID)
	auth.Tokens.AccountID = immutableAccountID
	auth.Tokens.RefreshToken = refreshToken
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email: id, OAuthCredentialOrigin: origin, Auth: auth,
	}); err != nil {
		t.Fatal(err)
	}
}

func saveComparisonAPIKeyAccount(t *testing.T, store accounts.CodexStore, id, key string) {
	t.Helper()
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email: id,
		Auth:  accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: key},
	}); err != nil {
		t.Fatal(err)
	}
}

func writeComparisonCodexAccountFile(
	t *testing.T,
	store accounts.CodexStore,
	filename, id, immutableAccountID, refreshToken string,
	origin accounts.CodexOAuthCredentialOrigin,
) {
	t.Helper()
	auth := testCodexAuth(id, immutableAccountID)
	auth.Tokens.AccountID = immutableAccountID
	auth.Tokens.RefreshToken = refreshToken
	body, err := json.Marshal(accounts.StoredCodexAccount{
		Email: id, OAuthCredentialOrigin: origin, Auth: auth,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(store.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir, filename), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func isolationLegacyTestState(t *testing.T) (home, stateDir string, legacyStore accounts.CodexStore) {
	t.Helper()
	root := t.TempDir()
	home = filepath.Join(root, "home")
	stateDir = filepath.Join(root, "state")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("SUBROUTER_STATE_DIR", stateDir)
	legacyStore = accounts.CodexStore{Dir: filepath.Join(home, ".codex-accounts", "accounts")}
	return home, stateDir, legacyStore
}

func snapshotTestTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			snapshot[rel] = "<dir>"
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			snapshot[rel] = "<symlink>" + target
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snapshot[rel] = string(body)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestCodexMigrateIsolationRemainsAvailableWithMalformedRoutingConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	configPath := t.TempDir() + "/cloud.json"
	t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runner := srRunner{
		store: accounts.DefaultCodexStore(), in: strings.NewReader(""), out: &out, errOut: &out,
	}
	if err := runner.run(context.Background(), []string{"codex", "migrate-isolation"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No migration needed") {
		t.Fatalf("migration command did not run: %q", out.String())
	}
}

func TestCodexMigrateIsolationReenrollsExpectedAccountsAndPreservesActiveAuth(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	active := testCodexAuth("interactive@example.com", "acct-interactive")
	active.Tokens.RefreshToken = "interactive-refresh"
	if err := accounts.WriteActiveCodexAuth(active); err != nil {
		t.Fatal(err)
	}
	activeBefore, err := os.ReadFile(accounts.DefaultCodexAuthPath())
	if err != nil {
		t.Fatal(err)
	}
	saveLegacyCodexAccount(t, store, "alpha@example.com", "acct-alpha", "legacy-alpha")
	saveLegacyCodexAccount(t, store, "beta@example.com", "acct-beta", "legacy-beta")

	alpha := testCodexAuth("alpha@example.com", "acct-alpha")
	alpha.Tokens.RefreshToken = "isolated-alpha"
	beta := testCodexAuth("beta@example.com", "acct-beta")
	beta.Tokens.RefreshToken = "isolated-beta"
	fake := &recordingSRCommandRunner{loginAuths: []accounts.CodexAuthFile{alpha, beta}}
	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}
	if err := runner.codexAccount(context.Background(), []string{"migrate-isolation", "--device-auth"}); err != nil {
		t.Fatal(err)
	}

	activeAfter, err := os.ReadFile(accounts.DefaultCodexAuthPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(activeBefore, activeAfter) {
		t.Fatal("migration changed interactive Codex auth")
	}
	for _, want := range []struct{ email, refresh string }{
		{"alpha@example.com", "isolated-alpha"},
		{"beta@example.com", "isolated-beta"},
	} {
		stored, ok, err := store.FindStored(want.email)
		if err != nil || !ok {
			t.Fatalf("find %s: ok=%v err=%v", want.email, ok, err)
		}
		if stored.OAuthCredentialOrigin != accounts.CodexOAuthOriginIsolatedServerLogin {
			t.Fatalf("%s origin = %q", want.email, stored.OAuthCredentialOrigin)
		}
		if stored.Auth.Tokens == nil || stored.Auth.Tokens.RefreshToken != want.refresh {
			t.Fatalf("%s did not receive isolated credential", want.email)
		}
		if len(stored.Breadcrumbs) == 0 || stored.Breadcrumbs[len(stored.Breadcrumbs)-1].Event != "credential_reenrolled_isolated" {
			t.Fatalf("%s missing migration breadcrumb", want.email)
		}
	}
	if got := fake.commandCount("codex", "login", "--device-auth"); got != 2 {
		t.Fatalf("device login count = %d, want 2", got)
	}
	if targets, err := codexIsolationTargets(store); err != nil || len(targets) != 0 {
		t.Fatalf("remaining targets = %d, err=%v", len(targets), err)
	}
	for _, want := range []string{"Found 2", "Migrated 2 Codex OAuth account(s)", "Local Codex auth was left unchanged"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestCodexMigrateIsolationWrongIdentityDoesNotReplaceStoredAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	active := testCodexAuth("interactive@example.com", "acct-interactive")
	if err := accounts.WriteActiveCodexAuth(active); err != nil {
		t.Fatal(err)
	}
	saveLegacyCodexAccount(t, store, "expected@example.com", "acct-expected", "legacy-refresh")

	wrong := testCodexAuth("wrong@example.com", "acct-wrong")
	fake := &recordingSRCommandRunner{loginAuth: wrong}
	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}
	err := runner.codexAccount(context.Background(), []string{"migrate-isolation"})
	if err == nil || !strings.Contains(err.Error(), "expected expected@example.com") {
		t.Fatalf("error = %v", err)
	}
	stored, ok, findErr := store.FindStored("expected@example.com")
	if findErr != nil || !ok {
		t.Fatalf("find: ok=%v err=%v", ok, findErr)
	}
	if stored.OAuthCredentialOrigin != "" || stored.Auth.Tokens.RefreshToken != "legacy-refresh" {
		t.Fatal("wrong identity replaced stored credential")
	}
	if !strings.Contains(out.String(), "No stored credential was changed") || !strings.Contains(out.String(), "1 account(s) still need migration") {
		t.Fatalf("missing remaining-account report:\n%s", out.String())
	}
}

func TestCodexMigrateIsolationRejectsLoginThatSharesActiveRefreshChain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.DefaultCodexStore()
	active := testCodexAuth("expected@example.com", "acct-expected")
	active.Tokens.RefreshToken = "shared-refresh"
	if err := accounts.WriteActiveCodexAuth(active); err != nil {
		t.Fatal(err)
	}
	saveLegacyCodexAccount(t, store, "expected@example.com", "acct-expected", "legacy-refresh")

	fake := &recordingSRCommandRunner{loginAuth: active}
	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}
	err := runner.codexAccount(context.Background(), []string{"migrate-isolation"})
	if err == nil || !strings.Contains(err.Error(), "active Codex refresh-token chain") {
		t.Fatalf("error = %v", err)
	}
	stored, ok, findErr := store.FindStored("expected@example.com")
	if findErr != nil || !ok {
		t.Fatalf("find: ok=%v err=%v", ok, findErr)
	}
	if stored.OAuthCredentialOrigin != "" || stored.Auth.Tokens.RefreshToken != "legacy-refresh" {
		t.Fatal("shared active credential replaced stored account")
	}
}

func TestCodexIsolationTargetsIncludesTrustedCredentialSharedWithActiveAuth(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.DefaultCodexStore()
	active := testCodexAuth("shared@example.com", "acct-shared")
	active.Tokens.RefreshToken = "shared-refresh"
	if err := accounts.WriteActiveCodexAuth(active); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:                 "shared@example.com",
		OAuthCredentialOrigin: accounts.CodexOAuthOriginIsolatedServerLogin,
		Auth:                  active,
	}); err != nil {
		t.Fatal(err)
	}
	targets, err := codexIsolationTargets(store)
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets = %d, err=%v", len(targets), err)
	}
}

func TestCodexIsolationSummaryIsAggregateAndNoOpWhenClean(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.DefaultCodexStore()
	saveLegacyCodexAccount(t, store, "private@example.com", "acct-private", "secret-refresh")
	check := codexIsolationDoctorCheck(store)
	if check.status != "fail" || !strings.Contains(check.detail, "1 account(s)") || !strings.Contains(check.detail, codexIsolationRemediation) {
		t.Fatalf("doctor check = %#v", check)
	}
	if strings.Contains(check.detail, "private@example.com") || strings.Contains(check.detail, "secret-refresh") {
		t.Fatalf("doctor leaked account data: %q", check.detail)
	}
	var status bytes.Buffer
	if err := printCodexIsolationStatus(&status, store); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(status.String(), "private@example.com") || !strings.Contains(status.String(), codexIsolationRemediation) {
		t.Fatalf("status summary = %q", status.String())
	}

	stored, _, err := store.FindStored("private@example.com")
	if err != nil {
		t.Fatal(err)
	}
	stored.OAuthCredentialOrigin = accounts.CodexOAuthOriginIsolatedServerLogin
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"codex", "migrate-isolation"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No migration needed") {
		t.Fatalf("no-op output = %q", out.String())
	}
}

func TestDoctorReportsAggregateCodexIsolationRemediation(t *testing.T) {
	isolateCloudConfig(t)
	local := healthServer(t, 200)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")
	t.Setenv("HOME", t.TempDir())
	store := accounts.DefaultCodexStore()
	saveLegacyCodexAccount(t, store, "private@example.com", "acct-private", "secret-refresh")

	var out bytes.Buffer
	err := runDoctorWith(context.Background(), &fakeController{present: true}, nil, store, &out)
	if err == nil {
		t.Fatal("doctor accepted unisolated Codex account")
	}
	got := out.String()
	if !strings.Contains(got, "FAIL  Codex isolation") || !strings.Contains(got, codexIsolationRemediation) {
		t.Fatalf("doctor output missing aggregate remediation:\n%s", got)
	}
	if strings.Contains(got, "private@example.com") || strings.Contains(got, "secret-refresh") {
		t.Fatalf("doctor leaked account data:\n%s", got)
	}
}

func TestLocalCodexStoreServingDoesNotAssumeUnconfiguredSelectedRemoteIsLocal(t *testing.T) {
	isolateCloudConfig(t)
	t.Setenv("HOME", t.TempDir())
	store := accounts.DefaultCodexStore()
	serverStore := defaultSRServerStore(store)
	if err := serverStore.update(func(file *srServerFile) error {
		file.Default = "remote"
		file.Servers = []srServerConfig{{Name: "remote", URL: "https://remote.example.test"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if localCodexStoreServesLegacy(store) {
		t.Fatal("selected remote server was treated as the local credential store")
	}

	if err := serverStore.update(func(file *srServerFile) error {
		file.Default = "local-loopback"
		file.Servers = []srServerConfig{{Name: "local-loopback", URL: localBaseURL()}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !localCodexStoreServesLegacy(store) {
		t.Fatal("selected loopback server was not treated as the local credential store")
	}
}
