package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoldenOptionsRequireV0154Candidate(t *testing.T) {
	previousHooks := goldenTestHooks
	goldenTestHooks.enabled = false
	t.Cleanup(func() { goldenTestHooks = previousHooks })

	args := []string{
		"--predecessor-version", "v0.1.51",
		"--predecessor-sha256", goldenPinnedPredecessorSHA256,
		"--candidate-tag", "v0.1.54",
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
		t.Fatalf("v0.1.54 candidate was rejected: %v", err)
	}
	for index := range args {
		if args[index] == "v0.1.54" {
			args[index] = "v0.1.53"
			break
		}
	}
	if _, err := parseGoldenArgs(args); err == nil {
		t.Fatal("superseded v0.1.53 candidate was accepted")
	}
}

func TestGoldenGitHubJSONUsesLocalGitHubAuthentication(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	fakeBin := t.TempDir()
	fakeGH := filepath.Join(fakeBin, "gh")
	if err := os.WriteFile(fakeGH, []byte("#!/bin/sh\nprintf '%s\\n' golden-test-token\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer golden-test-token" {
			http.Error(writer, "authentication required", http.StatusForbidden)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"sha":"authenticated"}`))
	}))
	t.Cleanup(server.Close)

	var response struct {
		SHA string `json:"sha"`
	}
	if err := getGoldenGitHubJSON(context.Background(), server.Client(), server.URL, &response); err != nil {
		t.Fatalf("authenticated GitHub request failed: %v", err)
	}
	if response.SHA != "authenticated" {
		t.Fatalf("sha = %q, want authenticated", response.SHA)
	}
}
