package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoldenOptionsRequireProductionPredecessorV0160(t *testing.T) {
	previousHooks := goldenTestHooks
	goldenTestHooks.enabled = false
	t.Cleanup(func() { goldenTestHooks = previousHooks })

	args := []string{
		"--predecessor-version", "v0.1.60",
		"--predecessor-sha256", "769e504b731ef8b43db67e7651dcfe9ae169516570c7d2d2d211a6f997be1a7c",
		"--candidate-tag", "v0.1.80",
		"--candidate-sha256", strings.Repeat("b", 64),
		"--candidate-revision", strings.Repeat("c", 40),
		"--deploy-evidence-validator", "validator",
		"--account-id", "test@example.invalid",
		"--stream-lines", "100",
		"--migration-prepare", "true",
		"--migration-switch", "true",
		"--legacy-retirement", "true",
		"--activate", "true",
		"--rollback", "true",
		"--old-generation-check", "true",
	}
	if _, err := parseGoldenArgs(args); err != nil {
		t.Fatalf("production predecessor v0.1.60 was rejected: %v", err)
	}
}

func TestGoldenOptionsRequireV0180Candidate(t *testing.T) {
	previousHooks := goldenTestHooks
	goldenTestHooks.enabled = false
	t.Cleanup(func() { goldenTestHooks = previousHooks })

	args := []string{
		"--predecessor-version", "v0.1.60",
		"--predecessor-sha256", goldenPinnedPredecessorSHA256,
		"--candidate-tag", "v0.1.80",
		"--candidate-sha256", strings.Repeat("b", 64),
		"--candidate-revision", strings.Repeat("c", 40),
		"--deploy-evidence-validator", "validator",
		"--account-id", "test@example.invalid",
		"--stream-lines", "100",
		"--migration-prepare", "true",
		"--migration-switch", "true",
		"--legacy-retirement", "true",
		"--activate", "true",
		"--rollback", "true",
		"--old-generation-check", "true",
	}
	if _, err := parseGoldenArgs(args); err != nil {
		t.Fatalf("v0.1.80 candidate was rejected: %v", err)
	}
	for index := range args {
		if args[index] == "v0.1.80" {
			args[index] = "v0.1.79"
			break
		}
	}
	if _, err := parseGoldenArgs(args); err == nil {
		t.Fatal("previous v0.1.79 candidate was accepted")
	}
}

func TestGoldenOptionsPinSparkAndSelectedOAuthAccount(t *testing.T) {
	previousHooks := goldenTestHooks
	goldenTestHooks.enabled = true
	goldenTestHooks.evidenceValidator = "validator"
	t.Cleanup(func() { goldenTestHooks = previousHooks })

	args := []string{
		"--account-id", "test@example.invalid",
		"--migration-prepare", "true",
		"--migration-switch", "true",
		"--legacy-retirement", "true",
		"--activate", "true",
		"--rollback", "true",
		"--old-generation-check", "true",
	}
	options, err := parseGoldenArgs(args)
	if err != nil {
		t.Fatal(err)
	}
	if options.model != "gpt-5.3-codex-spark" {
		t.Fatalf("default model = %q, want gpt-5.3-codex-spark", options.model)
	}
	if options.streamLines != 400 {
		t.Fatalf("default stream lines = %d, want 400", options.streamLines)
	}
	if options.accountID != "test@example.invalid" {
		t.Fatalf("account ID = %q", options.accountID)
	}

	environment := goldenChildEnv(t.TempDir(), map[string]string{
		"SUBROUTER_CODEX_ACCOUNT_ID": options.accountID,
	})
	if !containsGoldenEnvironment(environment, "SUBROUTER_CODEX_ACCOUNT_ID=test@example.invalid") {
		t.Fatalf("golden child environment omitted the selected OAuth account: %v", environment)
	}
}

func containsGoldenEnvironment(environment []string, expected string) bool {
	for _, item := range environment {
		if item == expected {
			return true
		}
	}
	return false
}

func TestGoldenGitHubJSONUsesLocalGitHubAuthentication(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	fakeBin := t.TempDir()
	fakeGH := filepath.Join(fakeBin, "gh")
	fakeGHScript := "#!/bin/sh\n" +
		"test \"$#\" -eq 4 || exit 9\n" +
		"test \"$1\" = auth || exit 9\n" +
		"test \"$2\" = token || exit 9\n" +
		"test \"$3\" = --hostname || exit 9\n" +
		"test \"$4\" = github.com || exit 9\n" +
		"printf '%s\\n' golden-test-token\n"
	if err := os.WriteFile(fakeGH, []byte(fakeGHScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	client := &http.Client{Transport: goldenRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://api.github.com/repos/manaflow-ai/subrouter/commits/v0.1.60" {
			t.Fatalf("request URL = %q", request.URL.String())
		}
		if request.Header.Get("Authorization") != "Bearer golden-test-token" {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"sha":"authenticated"}`)),
			Request:    request,
		}, nil
	})}

	var response struct {
		SHA string `json:"sha"`
	}
	if err := getGoldenGitHubJSON(context.Background(), client, "https://api.github.com/repos/manaflow-ai/subrouter/commits/v0.1.60", &response); err != nil {
		t.Fatalf("authenticated GitHub request failed: %v", err)
	}
	if response.SHA != "authenticated" {
		t.Fatalf("sha = %q, want authenticated", response.SHA)
	}
}

func TestGoldenGitHubJSONDoesNotSendAuthenticationToUntrustedURL(t *testing.T) {
	t.Setenv("GH_TOKEN", "golden-test-token")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if authorization := request.Header.Get("Authorization"); authorization != "" {
			t.Errorf("authorization leaked to untrusted URL: %q", authorization)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"sha":"anonymous"}`))
	}))
	t.Cleanup(server.Close)

	var response struct {
		SHA string `json:"sha"`
	}
	if err := getGoldenGitHubJSON(context.Background(), server.Client(), server.URL, &response); err != nil {
		t.Fatalf("anonymous request failed: %v", err)
	}
	if response.SHA != "anonymous" {
		t.Fatalf("sha = %q, want anonymous", response.SHA)
	}
}

func TestGoldenGitHubComparisonURLRequestsMetadataOnlyPage(t *testing.T) {
	const revision = "e169e94f2bea9a0455a5831631fcbac220bd65f2"
	comparisonURL, err := url.Parse(goldenGitHubComparisonURL(revision))
	if err != nil {
		t.Fatal(err)
	}
	if comparisonURL.Scheme != "https" || comparisonURL.Host != "api.github.com" ||
		comparisonURL.Path != "/repos/manaflow-ai/subrouter/compare/"+revision+"...main" {
		t.Fatalf("comparison URL target = %q", comparisonURL.String())
	}
	query := comparisonURL.Query()
	if query.Get("per_page") != "1" || query.Get("page") != "2" || len(query) != 2 {
		t.Fatalf("comparison URL query = %q", comparisonURL.RawQuery)
	}
}

type goldenRoundTripFunc func(*http.Request) (*http.Response, error)

func (function goldenRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
