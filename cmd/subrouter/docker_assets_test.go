package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
		`--cloud-base-url`,
		`${SUBROUTER_CLOUD_BASE_URL:-https://cmux.com}`,
		`--cloud-credential-source`,
		`team`,
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

func TestDockerSecretInitializerCreatesPrivateRegularFiles(t *testing.T) {
	requireDockerSecretScriptTools(t)
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	secretDir := filepath.Join(t.TempDir(), "secrets")
	command := exec.Command("sh", filepath.Join(repoRoot, "deploy", "docker", "init-secrets.sh"))
	command.Env = append(os.Environ(), "SUBROUTER_DOCKER_SECRET_DIR="+secretDir)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("init-secrets.sh failed: %v\n%s", err, output)
	}
	info, err := os.Stat(secretDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("secret directory mode = %o, want 700", got)
	}
	for _, name := range []string{"proxy-token", "admin-token", "account-import-token"} {
		path := filepath.Join(secretDir, name)
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("%s mode = %s, want regular file", name, info.Mode())
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode = %o, want 600", name, got)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(strings.TrimSpace(string(body))) != 64 {
			t.Fatalf("%s length = %d, want 64 hex characters", name, len(strings.TrimSpace(string(body))))
		}
	}
}

func TestDockerSecretInitializerRejectsSymlinkDestination(t *testing.T) {
	requireDockerSecretScriptTools(t)
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	secretDir := filepath.Join(t.TempDir(), "secrets")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	previous := []byte("do not overwrite\n")
	if err := os.WriteFile(outside, previous, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(secretDir, "proxy-token")); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sh", filepath.Join(repoRoot, "deploy", "docker", "init-secrets.sh"))
	command.Env = append(os.Environ(), "SUBROUTER_DOCKER_SECRET_DIR="+secretDir)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("init-secrets.sh accepted symlink destination:\n%s", output)
	}
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(previous) {
		t.Fatalf("symlink target changed to %q", got)
	}
}

func TestDockerSecretInitializerDoesNotKeepInterruptedSecret(t *testing.T) {
	requireDockerSecretScriptTools(t)
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	secretDir := filepath.Join(t.TempDir(), "secrets")
	fakeBin := t.TempDir()
	opensslPath := filepath.Join(fakeBin, "openssl")
	if err := os.WriteFile(opensslPath, []byte("#!/bin/sh\nprintf partial\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sh", filepath.Join(repoRoot, "deploy", "docker", "init-secrets.sh"))
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SUBROUTER_DOCKER_SECRET_DIR="+secretDir,
	)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("interrupted secret generation unexpectedly succeeded:\n%s", output)
	}
	if _, err := os.Lstat(filepath.Join(secretDir, "proxy-token")); !os.IsNotExist(err) {
		t.Fatalf("interrupted secret exists: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(secretDir, ".*.tmp.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary secrets left behind: %v", matches)
	}
}

func requireDockerSecretScriptTools(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX deployment scripts are not runnable on Windows")
	}
	for _, name := range []string{"sh", "openssl", "mktemp"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s is required for this deployment-script test", name)
		}
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
