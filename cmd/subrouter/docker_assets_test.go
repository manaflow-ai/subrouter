package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerfileBuildsAHealthCheckedNonRootRuntime(t *testing.T) {
	body := readRepositoryFile(t, "Dockerfile")
	for _, want := range []string{
		"AS build",
		"gcr.io/distroless/static-debian12:nonroot",
		"USER 65532:65532",
		"HEALTHCHECK",
		`["/usr/local/bin/subrouter", "probe"`,
		`ENTRYPOINT ["/usr/local/bin/subrouter"]`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Dockerfile missing %q", want)
		}
	}
}

func TestDockerComposeDefinesHardenedLocalAndTeamModes(t *testing.T) {
	body := readRepositoryFile(t, filepath.Join("deploy", "docker", "compose.yaml"))
	for _, want := range []string{
		"subrouter-local:",
		"subrouter-team:",
		`profiles: ["local"]`,
		`profiles: ["team"]`,
		"read_only: true",
		"cap_drop:",
		"- ALL",
		"no-new-privileges:true",
		"pids_limit: 256",
		"mem_limit: 256m",
		"SUBROUTER_PROXY_TOKEN_FILE: /run/secrets/proxy_token",
		"SUBROUTER_ADMIN_TOKEN_FILE: /run/secrets/admin_token",
		"SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE: /run/secrets/account_import_token",
		"SUBROUTER_CLOUD_CONFIG: /run/secrets/team_cloud_config",
		"${SUBROUTER_BIND_IP:-127.0.0.1}",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("compose.yaml missing %q", want)
		}
	}
	if strings.Contains(body, "SUBROUTER_ADMIN_TOKEN:") ||
		strings.Contains(body, "SUBROUTER_ACCOUNT_IMPORT_TOKEN:") ||
		strings.Contains(body, "SUBROUTER_PROXY_TOKEN:") {
		t.Fatal("compose.yaml exposes a control token through container environment")
	}
}

func TestDockerProfilesReserveMemoryOutsideTheGoHeap(t *testing.T) {
	dockerfile := readRepositoryFile(t, "Dockerfile")
	if !strings.Contains(dockerfile, "GOMEMLIMIT=192MiB") {
		t.Fatal("Dockerfile has no Go heap limit below its 256 MiB container limit")
	}
	compose := readRepositoryFile(t, filepath.Join("deploy", "docker", "compose.yaml"))
	if got := strings.Count(compose, "GOMEMLIMIT: 192MiB"); got != 2 {
		t.Fatalf("compose GOMEMLIMIT entries = %d, want one for each profile", got)
	}
}

func TestProbeChecksHealthWithoutCredentials(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_subrouter/health" {
			t.Fatalf("path = %q, want health endpoint", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer healthy.Close()
	if err := run([]string{"probe", "--url", healthy.URL}); err != nil {
		t.Fatalf("healthy probe failed: %v", err)
	}

	unhealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer unhealthy.Close()
	if err := run([]string{"probe", "--url", unhealthy.URL}); err == nil {
		t.Fatal("unhealthy probe succeeded")
	}
}

func TestServeReadsControlTokensFromSecretFiles(t *testing.T) {
	for _, envName := range []string{
		"SUBROUTER_PROXY_TOKEN_FILE",
		"SUBROUTER_ADMIN_TOKEN_FILE",
		"SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE",
	} {
		t.Run(envName, func(t *testing.T) {
			tempDir := t.TempDir()
			t.Setenv(envName, filepath.Join(tempDir, "missing-secret"))
			t.Setenv("SUBROUTER_STATE_DIR", tempDir)
			t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(tempDir, "cloud.json"))
			err := serve([]string{
				"--addr", "invalid:::",
				"--fetch-usage=false",
				"--sr-switch-interval=0",
			})
			if err == nil || !strings.Contains(err.Error(), envName) {
				t.Fatalf("serve error = %v, want missing %s secret error", err, envName)
			}
		})
	}
}

func readRepositoryFile(t *testing.T, relativePath string) string {
	t.Helper()
	path := filepath.Join("..", "..", relativePath)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
