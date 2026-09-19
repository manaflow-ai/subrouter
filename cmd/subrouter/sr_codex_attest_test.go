package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

type attestRoundTripFunc func(*http.Request) (*http.Response, error)

func (f attestRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func attestRefreshClient(t *testing.T, rotate func(email string) accounts.CodexAuthFile) *http.Client {
	t.Helper()
	return &http.Client{Transport: attestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var payload struct {
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		email := strings.TrimPrefix(payload.RefreshToken, "legacy-")
		next := rotate(email)
		body, _ := json.Marshal(map[string]string{
			"access_token":  next.Tokens.AccessToken,
			"refresh_token": next.Tokens.RefreshToken,
			"id_token":      next.Tokens.IDToken,
		})
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}
}

func TestCodexAttestLegacyRepairsStateRootWithoutLogin(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	stateRoot := filepath.Join(root, "state")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))

	store := rawCodexStoreForStateRoot(stateRoot)
	saveLegacyCodexAccount(t, store, "alpha@example.com", "acct-alpha", "legacy-alpha@example.com")
	saveLegacyCodexAccount(t, store, "beta@example.com", "acct-beta", "legacy-beta@example.com")
	isolated := testCodexAuth("gamma@example.com", "acct-gamma")
	isolated.Tokens.RefreshToken = "isolated-gamma"
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email: "gamma@example.com", OAuthCredentialOrigin: accounts.CodexOAuthOriginIsolatedServerLogin, Auth: isolated,
	}); err != nil {
		t.Fatal(err)
	}

	client := attestRefreshClient(t, func(email string) accounts.CodexAuthFile {
		next := testCodexAuth(email, "acct-"+strings.TrimSuffix(email, "@example.com"))
		next.Tokens.RefreshToken = "rotated-" + email
		return next
	})
	fake := &recordingSRCommandRunner{}
	var out bytes.Buffer
	runner := srRunner{store: accounts.DefaultCodexStore(), in: strings.NewReader(""), out: &out, errOut: &out, client: client, cmd: fake}

	if err := runner.codexAccount(context.Background(), []string{"attest-legacy", "--state-dir", stateRoot, "--dry-run"}); err != nil {
		t.Fatalf("dry run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "2 stored Codex OAuth account(s) would be attested") {
		t.Fatalf("dry run output = %q", out.String())
	}
	for _, email := range []string{"alpha@example.com", "beta@example.com"} {
		stored, _, _ := store.FindStored(email)
		if stored.OAuthCredentialOrigin != "" {
			t.Fatalf("dry run modified %s: %q", email, stored.OAuthCredentialOrigin)
		}
	}

	out.Reset()
	if err := runner.codexAccount(context.Background(), []string{"attest-legacy", "--state-dir", stateRoot, "--only", "beta@example.com"}); err != nil {
		t.Fatalf("attest beta: %v\n%s", err, out.String())
	}
	beta, _, _ := store.FindStored("beta@example.com")
	if beta.OAuthCredentialOrigin != accounts.CodexOAuthOriginServerAttested || beta.Auth.Tokens.RefreshToken != "rotated-beta@example.com" {
		t.Fatalf("beta not attested: origin=%q refresh=%q", beta.OAuthCredentialOrigin, beta.Auth.Tokens.RefreshToken)
	}
	alpha, _, _ := store.FindStored("alpha@example.com")
	if alpha.OAuthCredentialOrigin != "" || alpha.Auth.Tokens.RefreshToken != "legacy-alpha@example.com" {
		t.Fatalf("--only touched alpha: origin=%q refresh=%q", alpha.OAuthCredentialOrigin, alpha.Auth.Tokens.RefreshToken)
	}

	out.Reset()
	if err := runner.codexAccount(context.Background(), []string{"attest-legacy", "--state-dir", stateRoot}); err != nil {
		t.Fatalf("attest rest: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Attested 1, skipped 0, failed 0.") {
		t.Fatalf("second run output = %q", out.String())
	}
	remaining, err := codexIsolationTargets(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("remaining targets = %d, want 0", len(remaining))
	}
	gamma, _, _ := store.FindStored("gamma@example.com")
	if gamma.Auth.Tokens.RefreshToken != "isolated-gamma" {
		t.Fatal("already-isolated record was rotated")
	}
	if fake.commandCount("codex", "login") != 0 {
		t.Fatal("attest-legacy must not run codex login")
	}

	out.Reset()
	if err := runner.codexAccount(context.Background(), []string{"attest-legacy", "--state-dir", stateRoot}); err != nil {
		t.Fatalf("idempotent run: %v", err)
	}
	if !strings.Contains(out.String(), "Nothing to attest") {
		t.Fatalf("idempotent output = %q", out.String())
	}
}
