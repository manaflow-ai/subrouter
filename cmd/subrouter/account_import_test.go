package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
)

// fakeVault stands in for the hosted team vault. It records every upload so a
// test can assert exactly how many credentials left this machine.
type fakeVault struct {
	mu       sync.Mutex
	uploads  []broker.AccountUpload
	existing []broker.SharedAccount
}

func (v *fakeVault) start(t *testing.T) *broker.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/subrouter/accounts":
			v.mu.Lock()
			defer v.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": v.existing})
		case r.Method == http.MethodPost && r.URL.Path == "/api/subrouter/accounts":
			var upload broker.AccountUpload
			if err := json.NewDecoder(r.Body).Decode(&upload); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			v.mu.Lock()
			defer v.mu.Unlock()
			v.uploads = append(v.uploads, upload)
			label, _ := upload["label"].(string)
			kind, _ := upload["provider"].(string)
			shared := broker.SharedAccount{
				ID:    fmt.Sprintf("shared-%d", len(v.uploads)),
				Kind:  kind,
				Label: label,
			}
			v.existing = append(v.existing, shared)
			_ = json.NewEncoder(w).Encode(map[string]any{"account": shared})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return broker.NewClient(broker.Config{
		BaseURL:          server.URL,
		AccessToken:      "vault-access",
		RefreshToken:     "vault-refresh",
		TeamID:           "team-1",
		CredentialSource: broker.CredentialSourceTeam,
	})
}

func (v *fakeVault) uploadedLabels() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	labels := make([]string, 0, len(v.uploads))
	for _, upload := range v.uploads {
		label, _ := upload["label"].(string)
		labels = append(labels, label)
	}
	return labels
}

// importRunner builds a runner over a temp store seeded with OAuth credentials.
// HOME is redirected so the test cannot restart or inspect the real daemon.
func importRunner(t *testing.T, emails ...string) (srRunner, *bytes.Buffer, accounts.CodexStore) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	for _, email := range emails {
		if err := store.SaveStored(accounts.StoredCodexAccount{
			Email:    email,
			Provider: accounts.ProviderCodex,
			AddedAt:  time.Now().UTC().Format(time.RFC3339),
			Auth: accounts.CodexAuthFile{Tokens: &accounts.CodexTokens{
				AccessToken:  "access-" + email,
				RefreshToken: "refresh-" + email,
				IDToken:      "id-" + email,
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	return srRunner{program: "sr", store: store, out: &out, errOut: &out}, &out, store
}

func activeLabels(t *testing.T, store accounts.CodexStore) []string {
	t.Helper()
	stored, err := store.ListStored()
	if err != nil {
		t.Fatal(err)
	}
	labels := make([]string, 0, len(stored))
	for _, item := range stored {
		labels = append(labels, item.Email)
	}
	return labels
}

func migratedRecordExists(t *testing.T, store accounts.CodexStore, email string) bool {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(store.Dir, accounts.MigratedDirName, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range matches {
		if strings.Contains(filepath.Base(path), strings.ReplaceAll(email, "+", "_")) {
			return true
		}
	}
	return false
}

// Uploading one credential must upload exactly that one, hand over only that
// one, and leave the rest of the store refreshable here.
func TestImportOnlyUploadsAndHandsOverASingleCredential(t *testing.T) {
	runner, out, store := importRunner(t, "one@example.com", "two@example.com")
	vault := &fakeVault{}
	client := vault.start(t)

	if err := runner.cloudAccountImport(
		context.Background(), client, []string{"--only", "one@example.com"},
	); err != nil {
		t.Fatalf("import --only: %v\n%s", err, out.String())
	}

	if labels := vault.uploadedLabels(); len(labels) != 1 || labels[0] != "one@example.com" {
		t.Fatalf("uploaded %v, want exactly [one@example.com]", labels)
	}
	active := activeLabels(t, store)
	if len(active) != 1 || active[0] != "two@example.com" {
		t.Fatalf("active store = %v, want only two@example.com", active)
	}
	if !migratedRecordExists(t, store, "one@example.com") {
		t.Error("no rollback record for the handed-over credential")
	}
	if migratedRecordExists(t, store, "two@example.com") {
		t.Error("an untouched credential was handed over")
	}
}

// The bulk path uploads every credential and leaves nothing refreshable here,
// which is what stops this machine and the vault from rotating the same chains.
func TestImportAllUploadsAndHandsOverEveryCredential(t *testing.T) {
	runner, out, store := importRunner(t, "one@example.com", "two@example.com", "three@example.com")
	vault := &fakeVault{}
	client := vault.start(t)

	if err := runner.cloudAccountImport(
		context.Background(), client, []string{"--all", "--yes"},
	); err != nil {
		t.Fatalf("import --all --yes: %v\n%s", err, out.String())
	}

	if labels := vault.uploadedLabels(); len(labels) != 3 {
		t.Fatalf("uploaded %v, want 3", labels)
	}
	if active := activeLabels(t, store); len(active) != 0 {
		t.Fatalf("active store still holds %v; this machine would keep refreshing them", active)
	}
	for _, email := range []string{"one@example.com", "two@example.com", "three@example.com"} {
		if !migratedRecordExists(t, store, email) {
			t.Errorf("no rollback record for %s", email)
		}
	}
	if !strings.Contains(out.String(), "no longer refreshes them") {
		t.Errorf("output does not state the ownership change:\n%s", out.String())
	}
}

func TestImportAllContinuesAfterDefaultClaudeCredentialHasNoNamedProfile(t *testing.T) {
	runner, out, store := importRunner(t, "later@example.com")
	defaultClaudeDir := filepath.Join(os.Getenv("HOME"), ".claude")
	if err := os.MkdirAll(defaultClaudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(defaultClaudeDir, ".credentials.json"),
		[]byte(`{"claudeAiOauth":{"accessToken":"claude-access","refreshToken":"claude-refresh"}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := defaultSRServerStore(store).save(srServerFile{
		Default: "cmux",
		Servers: []srServerConfig{{
			Name: "cmux", URL: "https://sr.cmux.com", TenantKey: "srt_test",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	vault := &fakeVault{}
	client := vault.start(t)

	if err := runner.cloudAccountImport(
		context.Background(), client, []string{"--all", "--yes"},
	); err != nil {
		t.Fatalf("import --all --yes: %v\n%s", err, out.String())
	}
	labels := vault.uploadedLabels()
	if len(labels) != 2 || labels[0] != "default" || labels[1] != "later@example.com" {
		t.Fatalf("uploaded labels = %v, want default followed by later@example.com", labels)
	}
	if active := activeLabels(t, store); len(active) != 0 {
		t.Fatalf("later Codex credential was not handed over: %v", active)
	}
}

func TestHostedOpenAIAPIKeyRejectsAnthropicPrefix(t *testing.T) {
	vault := &fakeVault{}
	client := vault.start(t)
	var output bytes.Buffer
	runner := srRunner{
		program: "sr",
		in:      strings.NewReader("work\nsk-ant-secret\n"),
		out:     &output,
		errOut:  &output,
	}
	err := runner.hostedAPIKeyAdd(context.Background(), client, "openai-key")
	if err == nil || !strings.Contains(err.Error(), "Anthropic") {
		t.Fatalf("OpenAI add error = %v, want Anthropic-prefix rejection", err)
	}
	if labels := vault.uploadedLabels(); len(labels) != 0 {
		t.Fatalf("uploaded mislabeled Anthropic key as OpenAI: %v", labels)
	}
}

// A bulk import without --yes must refuse rather than uploading everything on a
// typo, and must change nothing.
func TestImportAllRequiresConfirmation(t *testing.T) {
	runner, _, store := importRunner(t, "one@example.com", "two@example.com")
	vault := &fakeVault{}
	client := vault.start(t)

	if err := runner.cloudAccountImport(
		context.Background(), client, []string{"--all"},
	); err == nil {
		t.Fatal("bulk import proceeded without --yes")
	}
	if labels := vault.uploadedLabels(); len(labels) != 0 {
		t.Fatalf("uploaded %v despite refusing", labels)
	}
	if active := activeLabels(t, store); len(active) != 2 {
		t.Fatalf("active store = %v, want both credentials untouched", active)
	}
}

func TestImportDryRunChangesNothing(t *testing.T) {
	runner, out, store := importRunner(t, "one@example.com", "two@example.com")
	vault := &fakeVault{}
	client := vault.start(t)

	if err := runner.cloudAccountImport(
		context.Background(), client, []string{"--all", "--dry-run"},
	); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if labels := vault.uploadedLabels(); len(labels) != 0 {
		t.Fatalf("dry run uploaded %v", labels)
	}
	if active := activeLabels(t, store); len(active) != 2 {
		t.Fatalf("dry run changed the store: %v", active)
	}
	if !strings.Contains(out.String(), "would be uploaded") {
		t.Errorf("dry run did not preview the change:\n%s", out.String())
	}
}

// Re-running an import must not upload a second copy of a credential the vault
// already holds, or the vault accumulates duplicate chains for one account.
func TestImportSkipsCredentialsTheVaultAlreadyHas(t *testing.T) {
	runner, out, store := importRunner(t, "one@example.com")
	vault := &fakeVault{existing: []broker.SharedAccount{
		{ID: "shared-existing", Kind: "codex", Label: "one@example.com"},
	}}
	client := vault.start(t)

	if err := runner.cloudAccountImport(
		context.Background(), client, []string{"--all", "--yes"},
	); err != nil {
		t.Fatalf("import: %v", err)
	}
	if labels := vault.uploadedLabels(); len(labels) != 0 {
		t.Fatalf("uploaded %v, want none: the vault already has it", labels)
	}
	if !strings.Contains(out.String(), "already shared") {
		t.Errorf("output did not explain the skip:\n%s", out.String())
	}
	if active := activeLabels(t, store); len(active) != 1 {
		t.Fatalf("a skipped credential was handed over anyway: %v", active)
	}
}

// Importing twice in a row must be safe: the second run finds the credential
// already shared and already handed over, and does nothing.
func TestImportIsIdempotentAcrossRuns(t *testing.T) {
	runner, _, store := importRunner(t, "one@example.com", "two@example.com")
	vault := &fakeVault{}
	client := vault.start(t)

	for round := 1; round <= 2; round++ {
		if err := runner.cloudAccountImport(
			context.Background(), client, []string{"--all", "--yes"},
		); err != nil && round == 1 {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	if labels := vault.uploadedLabels(); len(labels) != 2 {
		t.Fatalf("uploaded %v across two runs, want each credential once", labels)
	}
	if active := activeLabels(t, store); len(active) != 0 {
		t.Fatalf("active store = %v after two imports", active)
	}
}
