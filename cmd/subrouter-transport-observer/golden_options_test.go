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

func TestGoldenOptionsRequireV0168Candidate(t *testing.T) {
	previousHooks := goldenTestHooks
	goldenTestHooks.enabled = false
	t.Cleanup(func() { goldenTestHooks = previousHooks })

	args := []string{
		"--predecessor-version", "v0.1.51",
		"--predecessor-sha256", goldenPinnedPredecessorSHA256,
		"--candidate-tag", "v0.1.68",
		"--candidate-sha256", strings.Repeat("b", 64),
		"--candidate-revision", strings.Repeat("c", 40),
		"--deploy-evidence-validator", "validator",
		"--stream-lines", "100",
		"--migration-prepare", "true",
		"--migration-switch", "true",
		"--legacy-retirement", "true",
		"--activate", "true",
		"--rollback", "true",
		"--old-generation-check", "true",
	}
	if _, err := parseGoldenArgs(args); err != nil {
		t.Fatalf("v0.1.68 candidate was rejected: %v", err)
	}
	for index := range args {
		if args[index] == "v0.1.68" {
			args[index] = "v0.1.63"
			break
		}
	}
	if _, err := parseGoldenArgs(args); err == nil {
		t.Fatal("bootstrap v0.1.63 candidate was accepted")
	}
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
		if request.URL.String() != "https://api.github.com/repos/manaflow-ai/subrouter/commits/v0.1.51" {
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
	if err := getGoldenGitHubJSON(context.Background(), client, "https://api.github.com/repos/manaflow-ai/subrouter/commits/v0.1.51", &response); err != nil {
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
	const revision = "5eacb5411c0bd4a24f4e422d6366fa7bfd1843c8"
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
