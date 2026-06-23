package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/selectacct"
)

func TestSRListReadsNativeCodexStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "b@example.com",
		AddedAt: "2026-04-28T00:00:00Z",
		Auth: accounts.CodexAuthFile{Tokens: &accounts.CodexTokens{
			AccessToken: "access",
			IDToken:     "id",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "apikey:paid",
		AddedAt: "2026-04-28T00:00:00Z",
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-test",
		},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"list"}); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if !strings.Contains(got, "b@example.com") {
		t.Fatalf("list output missing OAuth account:\n%s", got)
	}
	if !strings.Contains(got, "paid (api key)") {
		t.Fatalf("list output missing API-key account:\n%s", got)
	}
}

func TestSRTraceShowsOAuthBreadcrumbs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "a@example.com",
		AddedAt: "2026-04-28T00:00:00Z",
		Auth: accounts.CodexAuthFile{AuthMode: "chatgpt", Tokens: &accounts.CodexTokens{
			AccessToken:  "access-token",
			RefreshToken: "raw-refresh-token",
			IDToken:      "id-token",
			AccountID:    "acct_123",
		}},
		Breadcrumbs: []accounts.CodexAuthBreadcrumb{{
			At:              "2026-05-19T08:00:00Z",
			Event:           "refresh_terminal_failure",
			Source:          "oauth_refresh",
			Reason:          "proxy.score-accounts",
			Host:            "subrouter-team",
			PID:             123,
			PPID:            1,
			Executable:      "/usr/local/bin/subrouter",
			WorkingDir:      "/var/lib/subrouter",
			SourcePath:      "/var/lib/subrouter/codex/accounts/a@example.com.json",
			RefreshFP:       "refreshfp",
			OldRefreshFP:    "oldrefreshfp",
			StatusCode:      http.StatusUnauthorized,
			ProviderType:    "invalid_request_error",
			ProviderCode:    "refresh_token_reused",
			ProviderMessage: "Your refresh token has already been used to generate a new access token. Please try signing in again.",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"trace", "a@example.com"}); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	for _, want := range []string{
		"OAuth breadcrumbs for a@example.com",
		"refresh_terminal_failure",
		"reason=\"proxy.score-accounts\"",
		"host=\"subrouter-team\"",
		"pid=\"123\"",
		"refresh=\"refreshfp\"",
		"old_refresh=\"oldrefreshfp\"",
		"provider_code=\"refresh_token_reused\"",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("trace output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "raw-refresh-token") {
		t.Fatalf("trace output leaked raw refresh token:\n%s", got)
	}
}

func TestSRSwitchAPIKeyWritesCodexAuthJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "apikey:paid",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-test",
			Tokens:       &accounts.CodexTokens{IDToken: ""},
		},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"switch", "paid"}); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var active map[string]any
	if err := json.Unmarshal(body, &active); err != nil {
		t.Fatal(err)
	}
	if active["auth_mode"] != "apikey" {
		t.Fatalf("auth_mode = %v, want apikey", active["auth_mode"])
	}
	if active["OPENAI_API_KEY"] != "sk-test" {
		t.Fatalf("OPENAI_API_KEY = %v, want sk-test", active["OPENAI_API_KEY"])
	}
	if _, ok := active["tokens"]; ok {
		t.Fatal("tokens should be stripped for API-key auth")
	}
	var openCodeAuth map[string]map[string]any
	readJSONFile(t, filepath.Join(home, ".local", "share", "opencode", "auth.json"), &openCodeAuth)
	if got := openCodeAuth["openai"]["type"]; got != "api" {
		t.Fatalf("opencode API-key type = %v, want api", got)
	}
	if got := openCodeAuth["openai"]["key"]; got != "sk-test" {
		t.Fatalf("opencode API key was not synced")
	}
	var piAuth map[string]map[string]any
	readJSONFile(t, filepath.Join(home, ".pi", "agent", "auth.json"), &piAuth)
	if got := piAuth["openai"]["type"]; got != "api_key" {
		t.Fatalf("pi API-key type = %v, want api_key", got)
	}
	if got := piAuth["openai"]["key"]; got != "sk-test" {
		t.Fatalf("pi API key was not synced")
	}
	if !strings.Contains(out.String(), "Switched to apikey:paid") {
		t.Fatalf("missing switch confirmation:\n%s", out.String())
	}
}

func TestSRSwitchSyncsOpenCodeAndPiAuth(t *testing.T) {
	home := t.TempDir()
	xdgData := t.TempDir()
	piDir := filepath.Join(home, "pi-agent")
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", xdgData)
	t.Setenv("PI_CODING_AGENT_DIR", piDir)

	access := testJWT(map[string]any{
		"exp": time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_test",
		},
	})
	id := testJWT(map[string]any{
		"exp":   time.Now().Add(time.Hour).Unix(),
		"email": "sync@example.com",
	})
	store := accounts.DefaultCodexStore()
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "sync@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth: accounts.CodexAuthFile{Tokens: &accounts.CodexTokens{
			AccessToken:  access,
			RefreshToken: "refresh-test",
			IDToken:      id,
		}},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"switch", "sync@example.com"}); err != nil {
		t.Fatal(err)
	}

	var openCodeAuth map[string]map[string]any
	readJSONFile(t, filepath.Join(xdgData, "opencode", "auth.json"), &openCodeAuth)
	if got := openCodeAuth["openai"]["type"]; got != "oauth" {
		t.Fatalf("opencode openai type = %v, want oauth", got)
	}
	if got := openCodeAuth["openai"]["access"]; got != access {
		t.Fatalf("opencode access was not synced")
	}
	if got := openCodeAuth["openai"]["accountId"]; got != "acct_test" {
		t.Fatalf("opencode accountId = %v, want acct_test", got)
	}

	var piAuth map[string]map[string]any
	readJSONFile(t, filepath.Join(piDir, "auth.json"), &piAuth)
	if got := piAuth["openai-codex"]["type"]; got != "oauth" {
		t.Fatalf("pi openai-codex type = %v, want oauth", got)
	}
	if got := piAuth["openai-codex"]["access"]; got != access {
		t.Fatalf("pi access was not synced")
	}
	if got := piAuth["openai-codex"]["accountId"]; got != "acct_test" {
		t.Fatalf("pi accountId = %v, want acct_test", got)
	}
}

func TestSRAddRestoresPreviouslyActiveAccount(t *testing.T) {
	home := t.TempDir()
	xdgData := t.TempDir()
	piDir := filepath.Join(home, "pi-agent")
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", xdgData)
	t.Setenv("PI_CODING_AGENT_DIR", piDir)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("alice@example.com", "acct_active")
	addedAuth := testCodexAuth("founders@example.com", "acct_added")
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "alice@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    activeAuth,
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}
	installFakeCodexLogin(t, home, addedAuth)

	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"add"}); err != nil {
		t.Fatal(err)
	}

	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing active auth")
	}
	email, err := accounts.ExtractEmailFromJWT(active.Tokens.IDToken)
	if err != nil {
		t.Fatal(err)
	}
	if email != "alice@example.com" {
		t.Fatalf("active email = %q, want alice@example.com", email)
	}
	if _, found, err := store.FindStored("founders@example.com"); err != nil {
		t.Fatal(err)
	} else if !found {
		t.Fatal("newly logged-in account was not imported")
	}
	if !strings.Contains(out.String(), "Restored active account: alice@example.com") {
		t.Fatalf("missing restore message:\n%s", out.String())
	}
}

func TestSRAutoSwitchesWhenActiveAccountIsExhausted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("alice@example.com", "acct_active")
	bestAuth := testCodexAuth("bob@example.com", "acct_best")
	for email, auth := range map[string]accounts.CodexAuthFile{
		"alice@example.com": activeAuth,
		"bob@example.com":   bestAuth,
	} {
		if err := store.SaveStored(accounts.StoredCodexAccount{
			Email:   email,
			AddedAt: time.Now().UTC().Format(time.RFC3339),
			Auth:    auth,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	usageByToken := map[string]float64{
		activeAuth.Tokens.AccessToken: 100,
		bestAuth.Tokens.AccessToken:   1,
	}
	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			used := usageByToken[strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")]
			return usageResponse(used), nil
		})},
	}

	if err := runner.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing active auth")
	}
	email, err := accounts.ExtractEmailFromJWT(active.Tokens.IDToken)
	if err != nil {
		t.Fatal(err)
	}
	if email != "bob@example.com" {
		t.Fatalf("active email = %q, want bob@example.com", email)
	}
	if !strings.Contains(out.String(), "Auto-switched to bob@example.com") {
		t.Fatalf("missing auto-switch confirmation:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Switch to (#):") {
		t.Fatalf("prompt should be skipped after auto-switch:\n%s", out.String())
	}
}

func TestSRPickSwitchesToRecommendedAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("alice@example.com", "acct_active")
	bestAuth := testCodexAuth("bob@example.com", "acct_best")
	for email, auth := range map[string]accounts.CodexAuthFile{
		"alice@example.com": activeAuth,
		"bob@example.com":   bestAuth,
	} {
		if err := store.SaveStored(accounts.StoredCodexAccount{
			Email:   email,
			AddedAt: time.Now().UTC().Format(time.RFC3339),
			Auth:    auth,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	usageByToken := map[string]float64{
		activeAuth.Tokens.AccessToken: 80,
		bestAuth.Tokens.AccessToken:   1,
	}
	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			used := usageByToken[strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")]
			return usageResponse(used), nil
		})},
	}

	if err := runner.run(context.Background(), []string{"pick"}); err != nil {
		t.Fatal(err)
	}

	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing active auth")
	}
	email, err := accounts.ExtractEmailFromJWT(active.Tokens.IDToken)
	if err != nil {
		t.Fatal(err)
	}
	if email != "bob@example.com" {
		t.Fatalf("active email = %q, want bob@example.com", email)
	}
	if !strings.Contains(out.String(), "Picked recommended account: bob@example.com") {
		t.Fatalf("missing pick confirmation:\n%s", out.String())
	}
	if strings.Contains(out.String(), "alice@example.com") {
		t.Fatalf("successful pick should only display the picked row:\n%s", out.String())
	}
}

func TestSRPickSucceedsWhenRecommendedActiveHasQuota(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("alice@example.com", "acct_active")
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "alice@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    activeAuth,
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return usageResponseWindows(20, 20), nil
		})},
	}

	if err := runner.run(context.Background(), []string{"pick"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Already using recommended account: alice@example.com") {
		t.Fatalf("missing already using confirmation:\n%s", out.String())
	}
}

func TestSRPickFailsWhenNoRecommendedAccountHasQuota(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("alice@example.com", "acct_active")
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "alice@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    activeAuth,
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return usageResponseWindows(100, 100), nil
		})},
	}

	err := runner.run(context.Background(), []string{"pick"})
	if err == nil || !strings.Contains(err.Error(), "no recommended account has quota") {
		t.Fatalf("err = %v, want no quota failure", err)
	}
	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing active auth")
	}
	email, err := accounts.ExtractEmailFromJWT(active.Tokens.IDToken)
	if err != nil {
		t.Fatal(err)
	}
	if email != "alice@example.com" {
		t.Fatalf("active email = %q, want alice@example.com", email)
	}
}

func TestSRDefaultShowsCookedAccountAndDoesNotPromptWhenAllCooked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("alice@example.com", "acct_active")
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "alice@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    activeAuth,
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return usageResponseWindows(0, 100), nil
		})},
	}

	if err := runner.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if !strings.Contains(got, "All of your OAuth accounts are cooked") {
		t.Fatalf("missing all-cooked warning:\n%s", got)
	}
	if !strings.Contains(got, "active, cooked") || !strings.Contains(got, "cooked, canno") {
		t.Fatalf("missing cooked row state:\n%s", got)
	}
	if strings.Contains(got, "Switch to (#):") {
		t.Fatalf("should not prompt when all OAuth accounts are cooked:\n%s", got)
	}
}

func TestSRDefaultShowsTemporarilyCookedAccountWhenShortWindowConsumed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("alice@example.com", "acct_active")
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "alice@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    activeAuth,
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader("\n"),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return usageResponseWindows(100, 55), nil
		})},
	}

	if err := runner.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if !strings.Contains(got, "active, temp") {
		t.Fatalf("missing temporarily cooked marker:\n%s", got)
	}
	if !strings.Contains(got, "temp cooked") {
		t.Fatalf("missing temporarily cooked reason:\n%s", got)
	}
	if strings.Contains(got, "active, cooked") || strings.Contains(got, "All of your OAuth accounts are cooked") {
		t.Fatalf("short-window saturation should not be treated as permanently cooked:\n%s", got)
	}
	if strings.Contains(got, "recommended") {
		t.Fatalf("temporarily cooked account should not be recommended:\n%s", got)
	}
	if strings.Contains(got, "Switch to (#):") {
		t.Fatalf("should not prompt when every OAuth account is unavailable for new sessions:\n%s", got)
	}
}

func TestSRInteractiveRefusesTemporarilyCookedSelection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("active@example.com", "acct_active")
	tempCookedAuth := testCodexAuth("temp@example.com", "acct_temp")
	for email, auth := range map[string]accounts.CodexAuthFile{
		"active@example.com": activeAuth,
		"temp@example.com":   tempCookedAuth,
	} {
		if err := store.SaveStored(accounts.StoredCodexAccount{
			Email:   email,
			AddedAt: time.Now().UTC().Format(time.RFC3339),
			Auth:    auth,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader("2\n"),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
			if token == tempCookedAuth.Tokens.AccessToken {
				return usageResponseWindows(100, 20), nil
			}
			return usageResponseWindows(0, 20), nil
		})},
	}

	err := runner.run(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "account is temporarily cooked") {
		t.Fatalf("err = %v, want temporarily cooked refusal", err)
	}

	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing active auth")
	}
	email, err := accounts.ExtractEmailFromJWT(active.Tokens.IDToken)
	if err != nil {
		t.Fatal(err)
	}
	if email != "active@example.com" {
		t.Fatalf("active email = %q, want active@example.com", email)
	}
}

func TestSRInteractiveRefusesCookedSelection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("active@example.com", "acct_active")
	cookedAuth := testCodexAuth("cooked@example.com", "acct_cooked")
	for email, auth := range map[string]accounts.CodexAuthFile{
		"active@example.com": activeAuth,
		"cooked@example.com": cookedAuth,
	} {
		if err := store.SaveStored(accounts.StoredCodexAccount{
			Email:   email,
			AddedAt: time.Now().UTC().Format(time.RFC3339),
			Auth:    auth,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader("2\n"),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
			if token == cookedAuth.Tokens.AccessToken {
				return usageResponseWindows(0, 100), nil
			}
			return usageResponseWindows(0, 0), nil
		})},
	}

	err := runner.run(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "account is cooked") {
		t.Fatalf("err = %v, want cooked refusal", err)
	}

	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing active auth")
	}
	email, err := accounts.ExtractEmailFromJWT(active.Tokens.IDToken)
	if err != nil {
		t.Fatal(err)
	}
	if email != "active@example.com" {
		t.Fatalf("active email = %q, want active@example.com", email)
	}
}

func TestSRSwitchRefusesCookedAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("active@example.com", "acct_active")
	cookedAuth := testCodexAuth("cooked@example.com", "acct_cooked")
	for email, auth := range map[string]accounts.CodexAuthFile{
		"active@example.com": activeAuth,
		"cooked@example.com": cookedAuth,
	} {
		if err := store.SaveStored(accounts.StoredCodexAccount{
			Email:   email,
			AddedAt: time.Now().UTC().Format(time.RFC3339),
			Auth:    auth,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return usageResponseWindows(0, 100), nil
		})},
	}

	err := runner.run(context.Background(), []string{"switch", "cooked@example.com"})
	if err == nil || !strings.Contains(err.Error(), "account is cooked") {
		t.Fatalf("err = %v, want cooked refusal", err)
	}

	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing active auth")
	}
	email, err := accounts.ExtractEmailFromJWT(active.Tokens.IDToken)
	if err != nil {
		t.Fatal(err)
	}
	if email != "active@example.com" {
		t.Fatalf("active email = %q, want active@example.com", email)
	}
}

func TestSRRankRowsKeepsAPIKeyAsFallback(t *testing.T) {
	rows := []srUsageRow{
		{
			email:    "apikey:paid",
			authMode: accounts.AuthModeAPIKey,
			score:    selectacct.Score{AccountID: "apikey:paid", Headroom: 1, ShortHeadroom: 1},
		},
		{
			email:    "later@example.com",
			authMode: accounts.AuthModeOAuth,
			score: selectacct.Score{
				AccountID:              "later@example.com",
				Headroom:               1,
				ShortHeadroom:          1,
				ShortResetAfterSeconds: int64((5 * time.Hour) / time.Second),
				ExpiryPressure:         1 / float64((5*time.Hour)/time.Second),
			},
		},
		{
			email:    "soon@example.com",
			authMode: accounts.AuthModeOAuth,
			score: selectacct.Score{
				AccountID:              "soon@example.com",
				Headroom:               0.90,
				ShortHeadroom:          0.90,
				ShortResetAfterSeconds: int64((2 * time.Hour) / time.Second),
				ExpiryPressure:         0.90 / float64((2*time.Hour)/time.Second),
			},
		},
	}

	rankUsageRows(rows)

	if rows[0].email != "soon@example.com" {
		t.Fatalf("first = %s, want soon@example.com", rows[0].email)
	}
	if rows[len(rows)-1].email != "apikey:paid" {
		t.Fatalf("last = %s, want apikey:paid", rows[len(rows)-1].email)
	}
	if !rows[0].gtoRecommended {
		t.Fatal("best row should be marked recommended")
	}
}

func TestSRRankRowsFallsBackToAPIKeyBeforeExhaustedOAuth(t *testing.T) {
	rows := []srUsageRow{
		{
			email:    "empty@example.com",
			authMode: accounts.AuthModeOAuth,
			score:    selectacct.Score{AccountID: "empty@example.com", Headroom: 0, ShortHeadroom: 0},
		},
		{
			email:    "apikey:paid",
			authMode: accounts.AuthModeAPIKey,
			score:    selectacct.Score{AccountID: "apikey:paid", Headroom: 0.01, ShortHeadroom: 0.01},
		},
	}

	rankUsageRows(rows)

	if rows[0].email != "apikey:paid" {
		t.Fatalf("first = %s, want apikey:paid", rows[0].email)
	}
	if !rows[0].gtoRecommended {
		t.Fatal("API-key fallback should be recommended when OAuth is exhausted")
	}
}

func TestSRRankRowsKeepsTemporarilyCookedAboveCooked(t *testing.T) {
	rows := []srUsageRow{
		{
			email:        "cooked@example.com",
			authMode:     accounts.AuthModeOAuth,
			score:        selectacct.Score{AccountID: "cooked@example.com", Headroom: 0, ShortHeadroom: 1, ShortResetAfterSeconds: int64((5 * time.Hour) / time.Second)},
			cooked:       true,
			cookedReason: "7d limit fully consumed",
		},
		{
			email:            "temp@example.com",
			authMode:         accounts.AuthModeOAuth,
			score:            selectacct.Score{AccountID: "temp@example.com", Headroom: 0, ShortHeadroom: 0, ShortResetAfterSeconds: int64((3 * time.Hour) / time.Second)},
			tempCooked:       true,
			tempCookedReason: "5h limit fully consumed",
		},
	}

	rankUsageRows(rows)

	if rows[0].email != "temp@example.com" {
		t.Fatalf("first = %s, want temp@example.com", rows[0].email)
	}
	if rows[0].gtoRecommended || rows[1].gtoRecommended {
		t.Fatalf("unavailable rows should not be recommended: %#v", rows)
	}
}

func TestSRRankRowsGroupsProvidersSeparately(t *testing.T) {
	rows := []srUsageRow{
		{
			email:    "claude-inactive@example.com",
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderClaude,
			score:    selectacct.Score{AccountID: "claude-inactive@example.com", Headroom: 1, ShortHeadroom: 1},
		},
		{
			email:    "codex-low@example.com",
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderCodex,
			score:    selectacct.Score{AccountID: "codex-low@example.com", Headroom: 0.3, ShortHeadroom: 0.3},
		},
		{
			email:    "claude-active@example.com",
			active:   true,
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderClaude,
			score:    selectacct.Score{AccountID: "claude-active@example.com", Headroom: 0.5, ShortHeadroom: 0.5},
		},
		{
			email:    "codex-high@example.com",
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderCodex,
			score:    selectacct.Score{AccountID: "codex-high@example.com", Headroom: 0.9, ShortHeadroom: 0.9},
		},
	}

	rankUsageRows(rows)

	got := []string{rows[0].email, rows[1].email, rows[2].email, rows[3].email}
	want := []string{"codex-high@example.com", "codex-low@example.com", "claude-active@example.com", "claude-inactive@example.com"}
	if !slices.Equal(got, want) {
		t.Fatalf("ranked rows = %#v, want %#v", got, want)
	}
	if !rows[0].gtoRecommended {
		t.Fatal("best Codex row should still be recommended")
	}
	if rows[2].gtoRecommended || rows[3].gtoRecommended {
		t.Fatalf("Claude rows should not be marked as Codex recommendations: %#v", rows)
	}
}

func TestScoreFromWindowsUsesAllDisplayedRateLimits(t *testing.T) {
	score := scoreFromWindows("a@example.com", []accounts.UsageWindow{
		{Name: "primary", UsedPercent: 10, LimitWindowSeconds: int64((5 * time.Hour) / time.Second), ResetAfterSeconds: int64((3 * time.Hour) / time.Second)},
		{Name: "secondary", UsedPercent: 10, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAfterSeconds: int64((6 * 24 * time.Hour) / time.Second)},
		{Name: "GPT-5.3-Codex-Spark (weekly)", UsedPercent: 100, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAfterSeconds: int64((2 * 24 * time.Hour) / time.Second)},
	})

	if score.Headroom != 0 {
		t.Fatalf("headroom = %.2f, want 0", score.Headroom)
	}
}

func TestRenderBarSupportsANSIColors(t *testing.T) {
	plain := renderBar(25, false)
	if !strings.HasPrefix(plain, "[") || !strings.HasSuffix(plain, "]") {
		t.Fatalf("plain bar = %q", plain)
	}

	colored := renderBar(25, true)
	if !strings.Contains(colored, ansiBGGreen) || !strings.Contains(colored, ansiBGGray) || !strings.HasSuffix(colored, ansiReset) {
		t.Fatalf("colored bar missing ANSI segments: %q", colored)
	}
}

func TestDisplayUsageRowsGridWhenForced(t *testing.T) {
	t.Setenv("COLUMNS", "200")
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{
		{
			email:          "lawrence@cmux.com",
			planType:       "pro",
			active:         true,
			gtoRecommended: true,
			gtoReason:      "45% bottleneck left, 5h resets in 1h",
			authMode:       accounts.AuthModeOAuth,
			windows: []accounts.UsageWindow{
				{Name: "primary", UsedPercent: 55, LimitWindowSeconds: int64((5 * time.Hour) / time.Second), ResetAfterSeconds: int64(time.Hour / time.Second)},
				{Name: "secondary", UsedPercent: 20, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAfterSeconds: int64((4 * 24 * time.Hour) / time.Second)},
				{Name: "GPT-5.3-Codex-Spark/primary", UsedPercent: 8, LimitWindowSeconds: int64((5 * time.Hour) / time.Second), ResetAfterSeconds: int64((30 * time.Minute) / time.Second)},
				{Name: "GPT-5.3-Codex-Spark/secondary", UsedPercent: 2, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAfterSeconds: int64((6 * 24 * time.Hour) / time.Second)},
			},
			credits:            &accounts.CreditsInfo{Balance: "0"},
			complimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Consumed: true},
		},
	}, true)

	got := out.String()
	if !strings.Contains(got, "Account") || !strings.Contains(got, "Spark wk") || !strings.Contains(got, "1x reset") {
		t.Fatalf("grid header missing:\n%s", got)
	}
	if !strings.Contains(got, "lawrence@cmux.com") || !strings.Contains(got, "active rec") || !strings.Contains(got, "used") {
		t.Fatalf("grid row missing state:\n%s", got)
	}
	if strings.Contains(got, "pick") {
		t.Fatalf("forced grid should not render detailed pick label:\n%s", got)
	}
	lines := strings.Split(got, "\n")
	separator := ""
	for _, line := range lines {
		if strings.Contains(line, "───") {
			separator = line
			break
		}
	}
	if separator == "" || strings.Contains(separator, "===") || strings.Contains(separator, "---") {
		t.Fatalf("grid separator should be a thin Unicode rule:\n%s", got)
	}
}

func TestUsageGridResetCellStates(t *testing.T) {
	ineligible := false
	cases := []struct {
		name string
		row  srUsageRow
		want string
	}{
		{
			name: "unknown",
			row:  srUsageRow{authMode: accounts.AuthModeOAuth, provider: accounts.ProviderCodex},
			want: "unknown",
		},
		{
			name: "available",
			row: srUsageRow{
				authMode:           accounts.AuthModeOAuth,
				provider:           accounts.ProviderCodex,
				complimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Available: true},
			},
			want: "avail",
		},
		{
			name: "consumed",
			row: srUsageRow{
				authMode:           accounts.AuthModeOAuth,
				provider:           accounts.ProviderCodex,
				complimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Consumed: true},
			},
			want: "used",
		},
		{
			name: "ineligible",
			row: srUsageRow{
				authMode:           accounts.AuthModeOAuth,
				provider:           accounts.ProviderCodex,
				complimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Eligible: &ineligible},
			},
			want: "not elig",
		},
		{
			name: "api key blank",
			row: srUsageRow{
				authMode:           accounts.AuthModeAPIKey,
				provider:           accounts.ProviderCodex,
				complimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Available: true},
			},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := usageGridResetCell(tc.row).Text; got != tc.want {
				t.Fatalf("reset cell = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDisplayUsageRowsGridGroupsProviders(t *testing.T) {
	t.Setenv("COLUMNS", "200")
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{
		{
			email:    "codex@example.com",
			planType: "pro",
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderCodex,
			score:    selectacct.Score{AccountID: "codex@example.com", Headroom: 0.9, ShortHeadroom: 0.9},
		},
		{
			email:    "claude@example.com",
			planType: "claude",
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderClaude,
			score:    selectacct.Score{AccountID: "claude@example.com", Headroom: 0.8, ShortHeadroom: 0.8},
		},
	}, true)

	got := out.String()
	codexGroup := strings.Index(got, "Codex accounts")
	codexRow := strings.Index(got, "codex@example.com")
	claudeGroup := strings.Index(got, "Claude profiles")
	claudeRow := strings.Index(got, "claude@example.com")
	if codexGroup < 0 || codexRow < 0 || claudeGroup < 0 || claudeRow < 0 {
		t.Fatalf("grouped rows missing labels or accounts:\n%s", got)
	}
	if !(codexGroup < codexRow && codexRow < claudeGroup && claudeGroup < claudeRow) {
		t.Fatalf("provider grouping order is unclear:\n%s", got)
	}
}

func TestDisplayUsageRowsGridUsesClaudeLimitLabels(t *testing.T) {
	t.Setenv("COLUMNS", "160")
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{
		{
			email:    "lawrence@manaflow.ai",
			planType: "claude",
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderClaude,
			score:    selectacct.Score{AccountID: "lawrence@manaflow.ai", Headroom: 0.46, ShortHeadroom: 0.89, ShortResetAfterSeconds: int64((3 * time.Hour) / time.Second)},
			windows: []accounts.UsageWindow{
				{Name: "5h", UsedPercent: 11, ResetAfterSeconds: int64((3 * time.Hour) / time.Second)},
				{Name: "7d", UsedPercent: 54, ResetAfterSeconds: int64((5 * 24 * time.Hour) / time.Second)},
				{Name: "opus-weekly", UsedPercent: 100, ResetAfterSeconds: int64((5 * 24 * time.Hour) / time.Second)},
				{Name: "sonnet-weekly", UsedPercent: 0, ResetAfterSeconds: int64((5 * 24 * time.Hour) / time.Second)},
			},
		},
	}, false)

	got := out.String()
	if !strings.Contains(got, "Session") || !strings.Contains(got, "Weekly") || !strings.Contains(got, "Opus wk") || !strings.Contains(got, "Sonnet wk") {
		t.Fatalf("Claude grid missing Claude-specific labels:\n%s", got)
	}
	if strings.Contains(got, "  5h  ") || strings.Contains(got, "  7d  ") || strings.Contains(got, "Spark") {
		t.Fatalf("Claude grid should not use Codex labels:\n%s", got)
	}
	if !strings.Contains(got, "session reset") {
		t.Fatalf("Claude pick reason should use session terminology:\n%s", got)
	}
}

func TestDisplayUsageRowsGridCompactsForNarrowTerminals(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{
		{
			email:          "lawrencechen2002@gmail.com",
			planType:       "pro",
			gtoRecommended: true,
			authMode:       accounts.AuthModeOAuth,
			provider:       accounts.ProviderCodex,
			score:          selectacct.Score{AccountID: "lawrencechen2002@gmail.com", Headroom: 0.67, ShortHeadroom: 0.96, ShortResetAfterSeconds: int64(time.Minute / time.Second)},
			windows: []accounts.UsageWindow{
				{Name: "primary", UsedPercent: 4, LimitWindowSeconds: int64((5 * time.Hour) / time.Second), ResetAfterSeconds: int64(time.Minute / time.Second)},
				{Name: "secondary", UsedPercent: 33, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAfterSeconds: int64((5 * 24 * time.Hour) / time.Second)},
				{Name: "GPT-5.3-Codex-Spark/primary", UsedPercent: 0, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)},
				{Name: "GPT-5.3-Codex-Spark/secondary", UsedPercent: 0, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
			},
			credits: &accounts.CreditsInfo{Balance: "0"},
		},
		{
			email:    "austin@manaflow.ai",
			planType: "claude",
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderClaude,
			score:    selectacct.Score{AccountID: "austin@manaflow.ai", Headroom: 0.95, ShortHeadroom: 0.95},
			windows: []accounts.UsageWindow{
				{Name: "5h", UsedPercent: 5, ResetAfterSeconds: int64((4 * time.Hour) / time.Second)},
				{Name: "7d", UsedPercent: 1, ResetAfterSeconds: int64((2 * 24 * time.Hour) / time.Second)},
			},
		},
	}, false)

	got := out.String()
	if strings.Contains(got, "Spark") || strings.Contains(got, "Spark wk") {
		t.Fatalf("narrow grid should omit Spark columns:\n%s", got)
	}
	if strings.HasPrefix(strings.TrimLeft(got, "\n"), "#") {
		t.Fatalf("non-numbered grid should not render blank # column:\n%s", got)
	}
	if !strings.Contains(got, "Use") || !strings.Contains(got, "Claude profiles") {
		t.Fatalf("narrow grid missing core columns or groups:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if line == "" {
			continue
		}
		if width := utf8.RuneCountInString(line); width > 80 {
			t.Fatalf("line width = %d, want <= 80:\n%s\nfull output:\n%s", width, line, got)
		}
	}
}

func TestDisplayUsageRowsGridColorsWhenForced(t *testing.T) {
	t.Setenv("SR_USAGE_GRID", "1")
	t.Setenv("FORCE_COLOR", "1")
	t.Setenv("COLUMNS", "200")
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{
		{
			email:          "ok@example.com",
			gtoRecommended: true,
			gtoReason:      "90% bottleneck left",
			authMode:       accounts.AuthModeOAuth,
			score:          selectacct.Score{AccountID: "ok@example.com", Headroom: 0.9, ShortHeadroom: 0.9},
			windows:        []accounts.UsageWindow{{Name: "primary", UsedPercent: 10, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)}},
		},
		{
			email:            "temp@example.com",
			gtoReason:        "temporarily cooked, cannot start new session",
			authMode:         accounts.AuthModeOAuth,
			tempCooked:       true,
			tempCookedReason: "5h limit fully consumed",
			windows:          []accounts.UsageWindow{{Name: "primary", UsedPercent: 100, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)}},
		},
		{
			email:        "cooked@example.com",
			gtoReason:    "cooked, cannot switch",
			authMode:     accounts.AuthModeOAuth,
			cooked:       true,
			windows:      []accounts.UsageWindow{{Name: "secondary", UsedPercent: 100, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)}},
			planType:     "pro",
			cookedReason: "7d limit fully consumed",
		},
	}, true)

	got := out.String()
	for _, code := range []string{ansiGreen, ansiYellow, ansiRed, ansiBold + ansiWhite, ansiDim, ansiBGRowAlt} {
		if !strings.Contains(got, code) {
			t.Fatalf("grid output missing color %q:\n%q", code, got)
		}
	}
	if strings.Contains(got, "\x1b[32mok@example.com") {
		t.Fatalf("account column should keep account styling separate from state colors:\n%q", got)
	}
}

func TestUsageGridSuppressesShortWindowsByQuotaFamily(t *testing.T) {
	windows := []accounts.UsageWindow{
		{Name: "primary", UsedPercent: 10, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)},
		{Name: "secondary", UsedPercent: 100, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
		{Name: "GPT-5.3-Codex-Spark/primary", UsedPercent: 1, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)},
		{Name: "GPT-5.3-Codex-Spark/secondary", UsedPercent: 2, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
	}
	cooked := srUsageRow{cooked: true, windows: windows}
	if cell := usageGridShortWindowCell(cooked); cell.Text != "" {
		t.Fatalf("short window cell = %q, want blank when general weekly quota is cooked", cell.Text)
	}
	if cell := usageGridShortNamedWindowCell(cooked); cell.Text == "" {
		t.Fatal("Spark short window should remain visible when only general weekly quota is cooked")
	}

	tempCooked := srUsageRow{tempCooked: true, windows: []accounts.UsageWindow{
		{Name: "primary", UsedPercent: 100, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)},
		{Name: "secondary", UsedPercent: 2, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
		{Name: "GPT-5.3-Codex-Spark/primary", UsedPercent: 100, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)},
		{Name: "GPT-5.3-Codex-Spark/secondary", UsedPercent: 2, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
	}}
	if cell := usageGridShortWindowCell(tempCooked); cell.Text == "" {
		t.Fatal("temporarily cooked row should still show the short window")
	}
	if cell := usageGridShortNamedWindowCell(tempCooked); cell.Text == "" {
		t.Fatal("temporarily cooked row should still show the short named window")
	}

	sparkWeeklyCooked := srUsageRow{cooked: true, windows: []accounts.UsageWindow{
		{Name: "primary", UsedPercent: 10, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)},
		{Name: "secondary", UsedPercent: 2, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
		{Name: "GPT-5.3-Codex-Spark/primary", UsedPercent: 1, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)},
		{Name: "GPT-5.3-Codex-Spark/secondary", UsedPercent: 100, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
	}}
	if cell := usageGridShortWindowCell(sparkWeeklyCooked); cell.Text == "" {
		t.Fatal("general short window should remain visible when only Spark weekly quota is cooked")
	}
	if cell := usageGridShortNamedWindowCell(sparkWeeklyCooked); cell.Text != "" {
		t.Fatalf("Spark short window cell = %q, want blank when Spark weekly quota is cooked", cell.Text)
	}
}

func TestDisplayUsageRowsIgnoresDetailedModeEnv(t *testing.T) {
	t.Setenv("SR_USAGE_GRID", "0")
	t.Setenv("CX_USAGE_GRID", "0")
	t.Setenv("COLUMNS", "200")
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{
		{
			email:     "a@example.com",
			gtoReason: "80% bottleneck left",
			authMode:  accounts.AuthModeOAuth,
		},
	}, true)

	got := out.String()
	if !strings.Contains(got, "Account") || !strings.Contains(got, "a@example.com") {
		t.Fatalf("grid output missing expected row:\n%s", got)
	}
	if strings.Contains(got, "1) a@example.com") || strings.Contains(got, "pick:") {
		t.Fatalf("detailed output should never be rendered:\n%s", got)
	}
}

func TestParseSRSwitchArgs(t *testing.T) {
	selector, opts, err := parseSRSwitchArgs([]string{"a@example.com", "--restart-gui"}, srSwitchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if selector != "a@example.com" {
		t.Fatalf("selector = %q", selector)
	}
	if !opts.restartCodexGUI {
		t.Fatal("restartCodexGUI should be true")
	}
}

func readJSONFile(t *testing.T, path string, out any) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatal(err)
	}
}

func testJWT(claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func testCodexAuth(email, accountID string) accounts.CodexAuthFile {
	access := testJWT(map[string]any{
		"exp": time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
		},
		"https://api.openai.com/profile": map[string]any{
			"email": email,
		},
	})
	id := testJWT(map[string]any{
		"exp":   time.Now().Add(time.Hour).Unix(),
		"email": email,
	})
	return accounts.CodexAuthFile{AuthMode: "chatgpt", Tokens: &accounts.CodexTokens{
		AccessToken:  access,
		RefreshToken: "refresh-" + accountID,
		IDToken:      id,
	}}
}

func installFakeCodexLogin(t *testing.T, home string, auth accounts.CodexAuthFile) {
	t.Helper()
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"test \"$1\" = login || exit 2\n" +
		"mkdir -p \"$HOME/.codex\"\n" +
		"cat > \"$HOME/.codex/auth.json\" <<'JSON'\n" +
		string(body) + "\nJSON\n"
	path := filepath.Join(binDir, "codex")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

type srRoundTripFunc func(*http.Request) (*http.Response, error)

func (f srRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func usageResponse(usedPercent float64) *http.Response {
	return usageResponseWindows(usedPercent, 0)
}

func usageResponseWindows(primaryUsedPercent, secondaryUsedPercent float64) *http.Response {
	body, _ := json.Marshal(map[string]any{
		"plan_type": "pro",
		"rate_limit": map[string]any{
			"primary_window": map[string]any{
				"used_percent":         primaryUsedPercent,
				"limit_window_seconds": int64((5 * time.Hour) / time.Second),
				"reset_after_seconds":  int64((3 * time.Hour) / time.Second),
			},
			"secondary_window": map[string]any{
				"used_percent":         secondaryUsedPercent,
				"limit_window_seconds": int64((7 * 24 * time.Hour) / time.Second),
				"reset_after_seconds":  int64((6 * 24 * time.Hour) / time.Second),
			},
		},
	})
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

func TestUsageErrorFootnoteShowsProviderAndReaddCommand(t *testing.T) {
	t.Setenv("COLUMNS", "200")
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{
		{
			email:    "codex-broken@example.com",
			authMode: accounts.AuthModeOAuth,
			err:      errors.New("usage fetch failed: 401 Unauthorized"),
		},
		{
			email:    "claude-broken@example.com",
			provider: accounts.ProviderClaude,
			err:      errors.New("Claude OAuth refresh failed: 400 Bad Request: invalid_grant"),
		},
		{
			email:    "codex-flaky@example.com",
			authMode: accounts.AuthModeOAuth,
			err:      errors.New("usage fetch failed: connection refused"),
		},
	}, false)
	text := out.String()
	if !strings.Contains(text, "codex-broken@example.com [codex]: usage fetch failed: 401 Unauthorized (re-add with: sr add)") {
		t.Fatalf("codex 401 footnote missing provider/re-add hint:\n%s", text)
	}
	if !strings.Contains(text, "claude-broken@example.com [claude]: Claude OAuth refresh failed: 400 Bad Request: invalid_grant (re-add with: sr claude add)") {
		t.Fatalf("claude invalid_grant footnote missing provider/re-add hint:\n%s", text)
	}
	if strings.Contains(text, "codex-flaky@example.com [codex]: usage fetch failed: connection refused (re-add") {
		t.Fatalf("transient error should not suggest re-add:\n%s", text)
	}
	if !strings.Contains(text, "codex-flaky@example.com [codex]: usage fetch failed: connection refused") {
		t.Fatalf("transient error footnote should still show provider:\n%s", text)
	}
}
