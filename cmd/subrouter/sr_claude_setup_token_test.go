package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/proxy"
)

const testSetupToken = "sk-ant-oat01-TESTTOKENTESTTOKENTESTTOKENTESTTOKENTESTTOKENTESTTOKENTESTTOKENTESTTOKENTESTTOKENTESTTOK-FAKEFAKEAA"

// installFakeClaudeSetupToken puts a `claude` on PATH whose only verb is
// setup-token: it prints the token the way Claude Code does and must never be
// asked to /login or report auth status.
func installFakeClaudeSetupToken(t *testing.T, root string) string {
	t.Helper()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(root, "claude-args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellQuote(recordPath) + "\n" +
		"if [ \"$1\" = \"setup-token\" ]; then\n" +
		"  printf 'Your OAuth token (valid for 1 year):\\n\\n %s\\n' " + shellQuote(testSetupToken) + "\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
	return recordPath
}

func TestMintSetupTokenExtractsPrintedTokenWithoutPaste(t *testing.T) {
	root := t.TempDir()
	installFakeClaudeSetupToken(t, root)
	var out bytes.Buffer
	runner := claudeRunner{
		store:  claude.Store{Dir: filepath.Join(root, "store")},
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
	}
	token, err := runner.mintSetupToken(t.Context())
	if err != nil {
		t.Fatalf("mint setup token: %v\n%s", err, out.String())
	}
	if token != testSetupToken {
		t.Fatalf("token = %q, want printed setup token", token)
	}
}

func TestClaudeAddRequiresExplicitProfileName(t *testing.T) {
	root := t.TempDir()
	recordPath := installFakeClaudeSetupToken(t, root)
	var out bytes.Buffer
	runner := claudeRunner{
		store:       claude.Store{Dir: filepath.Join(root, "store")},
		in:          strings.NewReader(""),
		out:         &out,
		errOut:      &out,
		verifyToken: func(context.Context, string) error { return nil },
	}
	err := runner.run(t.Context(), []string{"add"})
	if err == nil || !strings.Contains(err.Error(), "profile name is required") {
		t.Fatalf("add without name error = %v, want explicit profile-name error", err)
	}
	if _, statErr := os.Stat(recordPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("claude setup-token was invoked without a profile name, stat error = %v", statErr)
	}
	if profiles := runner.store.ListProfiles(); len(profiles) != 0 {
		t.Fatalf("profiles created without a name: %+v", profiles)
	}
}

func readStoredClaudeCredential(t *testing.T, store claude.Store, name string) claude.CredentialInfo {
	t.Helper()
	profile, ok := store.FindProfile(name)
	if !ok {
		t.Fatalf("profile %q was not registered", name)
	}
	body, err := os.ReadFile(filepath.Join(store.InstancePath(profile.Name), ".credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		ClaudeAIOAuth claude.CredentialInfo `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	return raw.ClaudeAIOAuth
}

func TestClaudeAddDefaultsToSetupTokenAndRecordsExpiry(t *testing.T) {
	root := t.TempDir()
	// The account generation marker lives under <store>/accounts, so the ref
	// must be opened on the same root the claude store publishes to.
	store := claude.Store{Dir: root}
	accountStore := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	ref, err := proxy.OpenAccountRef(accountStore, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	beforeGeneration := ref.Generation()
	recordPath := installFakeClaudeSetupToken(t, root)
	issued := time.Date(2026, 9, 2, 22, 0, 0, 0, time.UTC)
	verified := ""
	// Production passes the terminal *os.File straight to `claude setup-token`
	// and then reads the pasted token from the same file. A pipe reproduces
	// that fd passthrough; a strings.Reader would be drained into the child.
	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdinReader.Close()
	if _, err := stdinWriter.WriteString(testSetupToken + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := stdinWriter.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runner := claudeRunner{
		store:  store,
		in:     stdinReader,
		out:    &out,
		errOut: &out,
		now:    func() time.Time { return issued },
		verifyToken: func(_ context.Context, token string) error {
			verified = token
			return nil
		},
	}
	if err := runner.run(t.Context(), []string{"add", "work"}); err != nil {
		t.Fatalf("add: %v\n%s", err, out.String())
	}
	if verified != testSetupToken {
		t.Fatalf("verified token = %q", verified)
	}
	args, _ := os.ReadFile(recordPath)
	if strings.TrimSpace(string(args)) != "setup-token" {
		t.Fatalf("claude was invoked with %q, want only setup-token", string(args))
	}
	credential := readStoredClaudeCredential(t, store, "work")
	if credential.AccessToken != testSetupToken || credential.RefreshToken != "" {
		t.Fatalf("stored credential = %+v", credential)
	}
	if want := issued.Add(claude.SetupTokenLifetime).UnixMilli(); credential.ExpiresAt != want {
		t.Fatalf("expiresAt = %d, want %d", credential.ExpiresAt, want)
	}
	if len(credential.Scopes) != 1 || credential.Scopes[0] != claude.SetupTokenScope {
		t.Fatalf("scopes = %v", credential.Scopes)
	}
	if !strings.Contains(out.String(), "Expires 2027-09-02 (in 365 days)") {
		t.Fatalf("output did not state the expiry:\n%s", out.String())
	}
	// The fake claude prints the token once, as Claude Code does on the
	// terminal; Subrouter itself must never repeat it.
	if got := strings.Count(out.String(), testSetupToken); got != 1 {
		t.Fatalf("token appeared %d times in output, want once (from claude):\n%s", got, out.String())
	}
	triggerClaudeAccountReload(t, ref)
	if ref.Generation() <= beforeGeneration {
		t.Fatalf("account generation = %d, want greater than %d", ref.Generation(), beforeGeneration)
	}
	if !containsClaudeAccount(ref.All(), "work") {
		t.Fatalf("running account snapshot did not load the setup-token profile: %+v", ref.All())
	}
}

func TestClaudeAddTokenFlagNeedsNoClaudeCLI(t *testing.T) {
	root := t.TempDir()
	store := claude.Store{Dir: filepath.Join(root, "store")}
	emptyBin := filepath.Join(root, "empty-bin")
	if err := os.MkdirAll(emptyBin, 0o755); err != nil {
		t.Fatal(err)
	}
	// No claude anywhere on PATH; the system dirs stay so the store can still
	// reach macOS `security` for its keychain bookkeeping.
	t.Setenv("PATH", emptyBin+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
	var out bytes.Buffer
	runner := claudeRunner{
		store:       store,
		in:          strings.NewReader(""),
		out:         &out,
		errOut:      &out,
		verifyToken: func(context.Context, string) error { return nil },
	}
	if err := runner.run(t.Context(), []string{"add", "--token", testSetupToken, "personal"}); err != nil {
		t.Fatalf("add --token: %v\n%s", err, out.String())
	}
	if credential := readStoredClaudeCredential(t, store, "personal"); credential.AccessToken != testSetupToken {
		t.Fatalf("stored credential = %+v", credential)
	}

	// `--token -` reads the token from stdin so it never appears in argv.
	out.Reset()
	runner.in = strings.NewReader(testSetupToken + "\n")
	if err := runner.run(t.Context(), []string{"add", "stdin", "--token", "-"}); err != nil {
		t.Fatalf("add --token -: %v\n%s", err, out.String())
	}
	if credential := readStoredClaudeCredential(t, store, "stdin"); credential.AccessToken != testSetupToken {
		t.Fatalf("stored credential = %+v", credential)
	}
}

func TestClaudeAddRejectedOrMalformedTokenStoresNothing(t *testing.T) {
	root := t.TempDir()
	store := claude.Store{Dir: filepath.Join(root, "store")}
	t.Setenv("PATH", root+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
	var out bytes.Buffer
	verifyCalls := 0
	runner := claudeRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		verifyToken: func(context.Context, string) error {
			verifyCalls++
			return claude.ErrSetupTokenRejected
		},
	}
	err := runner.run(t.Context(), []string{"add", "work", "--token", "sk-ant-api03-" + strings.Repeat("k", 60)})
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("API key error = %v", err)
	}
	if verifyCalls != 0 {
		t.Fatal("a malformed token must be rejected before contacting Anthropic")
	}
	err = runner.run(t.Context(), []string{"add", "work", "--token", testSetupToken})
	if !errors.Is(err, claude.ErrSetupTokenRejected) || !strings.Contains(err.Error(), "claude setup-token") {
		t.Fatalf("rejected token error = %v", err)
	}
	if _, ok := store.FindProfile("work"); ok {
		t.Fatal("a rejected token must not register a profile")
	}
	if len(store.ListProfiles()) != 0 {
		t.Fatalf("profiles = %+v, want none", store.ListProfiles())
	}
}

func TestClaudeLoginKeepsBrowserOAuthFlow(t *testing.T) {
	root := t.TempDir()
	store := claude.Store{Dir: root}
	installSuccessfulClaudeLoginCLI(t, root, "work@example.com")
	var out bytes.Buffer
	runner := claudeRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		verifyToken: func(context.Context, string) error {
			t.Fatal("the OAuth login must not run setup-token verification")
			return nil
		},
	}
	for _, args := range [][]string{{"login", "work"}, {"add", "--oauth", "classic"}} {
		if err := runner.run(t.Context(), args); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out.String())
		}
	}
	for _, name := range []string{"work", "classic"} {
		credential := readStoredClaudeCredential(t, store, name)
		if credential.RefreshToken != "claude-refresh" || credential.AccessToken != "claude-access" {
			t.Fatalf("%s: stored credential = %+v, want the OAuth pair Claude wrote", name, credential)
		}
	}
	if err := runner.run(t.Context(), []string{"login", "work", "--token", testSetupToken}); err == nil {
		t.Fatal("login must refuse --token")
	}
}

func TestParseClaudeAddArgs(t *testing.T) {
	cases := []struct {
		args    []string
		want    claudeAddOptions
		wantErr bool
	}{
		{nil, claudeAddOptions{}, false},
		{[]string{"work"}, claudeAddOptions{name: "work"}, false},
		{[]string{"--token", "tok", "work"}, claudeAddOptions{name: "work", token: "tok"}, false},
		{[]string{"work", "--token=tok"}, claudeAddOptions{name: "work", token: "tok"}, false},
		{[]string{"--token", "-"}, claudeAddOptions{tokenFromStdin: true}, false},
		{[]string{"--oauth", "work"}, claudeAddOptions{name: "work", oauth: true}, false},
		{[]string{"--setup-token", "work"}, claudeAddOptions{name: "work"}, false},
		{[]string{"--oauth", "--token", "tok"}, claudeAddOptions{}, true},
		{[]string{"--token"}, claudeAddOptions{}, true},
		{[]string{"a", "b"}, claudeAddOptions{}, true},
		{[]string{"--bogus"}, claudeAddOptions{}, true},
	}
	for _, tc := range cases {
		got, err := parseClaudeAddArgs(tc.args)
		if (err != nil) != tc.wantErr {
			t.Errorf("%v: err = %v, wantErr %v", tc.args, err, tc.wantErr)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("%v: got %+v, want %+v", tc.args, got, tc.want)
		}
	}
}

func TestDisplayClaudeProfilesShowsSetupTokenExpiry(t *testing.T) {
	now := time.Date(2026, 9, 2, 22, 0, 0, 0, time.UTC)
	fresh := claude.SetupTokenCredential(testSetupToken, now)
	soon := claude.SetupTokenCredential(testSetupToken, now.Add(-claude.SetupTokenLifetime+10*24*time.Hour))
	expired := claude.SetupTokenCredential(testSetupToken, now.Add(-claude.SetupTokenLifetime-24*time.Hour))
	oauth := claude.CredentialInfo{AccessToken: "a", RefreshToken: "r", ExpiresAt: now.Add(time.Hour).UnixMilli()}

	if line := setupTokenStatusLine(claude.ProfileInfo{Name: "oauth", Credential: &oauth}, false, now); line != "" {
		t.Fatalf("OAuth profile line = %q, want none", line)
	}
	if line := setupTokenStatusLine(claude.ProfileInfo{Name: "fresh", Credential: &fresh}, false, now); line != "setup token, expires 2027-09-02 (in 365 days)" {
		t.Fatalf("fresh line = %q", line)
	}
	if line := setupTokenStatusLine(claude.ProfileInfo{Name: "soon", Credential: &soon}, false, now); !strings.Contains(line, "expires 2026-09-12 (in 10 days)") || !strings.Contains(line, "sr claude add soon") {
		t.Fatalf("soon line = %q", line)
	}
	if line := setupTokenStatusLine(claude.ProfileInfo{Name: "old", Credential: &expired}, false, now); !strings.Contains(line, "expired 2026-09-01") || !strings.Contains(line, "sr claude add old") {
		t.Fatalf("expired line = %q", line)
	}

	// Without Claude Code on the machine there is no auth status; the stored
	// setup token still describes the profile instead of "not logged in".
	var out bytes.Buffer
	displayClaudeProfiles(&out, []claude.ProfileInfo{{Name: "fresh", Credential: &fresh}}, false)
	if !strings.Contains(out.String(), "setup token, expires 2027-09-02") || strings.Contains(out.String(), "not logged in") {
		t.Fatalf("list output:\n%s", out.String())
	}
}

func TestSRClaudeHelpDocumentsSetupTokenAndLogin(t *testing.T) {
	for _, want := range []string{"sr claude add <name>", "setup token", "--token TOKEN|-", "--oauth", "sr claude login [name]"} {
		if !strings.Contains(srClaudeHelp, want) {
			t.Errorf("help is missing %q", want)
		}
	}
}
