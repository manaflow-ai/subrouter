package main

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// cliAutoupdateFixture serves one release from disk, so the updater can be
// exercised without GitHub.
type cliAutoupdateFixture struct {
	script      string
	installDir  string
	versionFile string
	env         []string
}

func newCLIAutoupdateFixture(t *testing.T, tag string) cliAutoupdateFixture {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	assetDir := t.TempDir()
	version := strings.TrimPrefix(tag, "v")
	asset := fmt.Sprintf("subrouter_%s_%s_%s", version, runtime.GOOS, releaseArchForTest(runtime.GOARCH))
	// The updater runs the installed binary with --help, so the fixture release
	// has to be executable rather than an inert blob.
	body := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(filepath.Join(assetDir, asset), body, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if err := os.WriteFile(
		filepath.Join(assetDir, "SHA256SUMS"),
		[]byte(fmt.Sprintf("%x  %s\n", sum, asset)),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	releaseJSON := filepath.Join(assetDir, "release.json")
	if err := os.WriteFile(releaseJSON, []byte(fmt.Sprintf(`{"tag_name":%q}`, tag)), 0o644); err != nil {
		t.Fatal(err)
	}

	installDir := filepath.Join(t.TempDir(), "bin")
	versionFile := filepath.Join(t.TempDir(), "cli-version")
	return cliAutoupdateFixture{
		script:      filepath.Join(repoRoot, "deploy", "macos", "subrouter-cli-autoupdate.sh"),
		installDir:  installDir,
		versionFile: versionFile,
		env: append(os.Environ(),
			"SUBROUTER_RELEASE_API_URL=file://"+releaseJSON,
			"SUBROUTER_INSTALL_URL=file://"+filepath.Join(repoRoot, "install.sh"),
			"SUBROUTER_DOWNLOAD_BASE=file://"+assetDir,
			"SUBROUTER_INSTALL_DIR="+installDir,
			"SUBROUTER_VERSION_FILE="+versionFile,
		),
	}
}

func (f cliAutoupdateFixture) run(t *testing.T) string {
	t.Helper()
	command := exec.Command(mustLookPath(t, "bash"), f.script)
	command.Env = f.env
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("cli autoupdate failed: %v\n%s", err, output)
	}
	return string(output)
}

func TestCLIAutoupdateInstallsAndThenStaysQuiet(t *testing.T) {
	requireDeployScriptTools(t, "bash", "curl", "python3")
	fixture := newCLIAutoupdateFixture(t, "v0.1.52")

	if output := fixture.run(t); !strings.Contains(output, "updating CLI none -> v0.1.52") {
		t.Fatalf("first run did not install:\n%s", output)
	}
	marker, err := os.ReadFile(fixture.versionFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(marker)) != "v0.1.52" {
		t.Fatalf("version marker = %q, want v0.1.52", marker)
	}

	// A second run must not reinstall. Anything that downloads on every tick
	// makes the agent expensive enough that someone eventually removes it.
	binary := filepath.Join(fixture.installDir, "subrouter")
	before, err := os.Stat(binary)
	if err != nil {
		t.Fatal(err)
	}
	if output := fixture.run(t); strings.Contains(output, "updating CLI") {
		t.Fatalf("second run reinstalled an already current CLI:\n%s", output)
	}
	after, err := os.Stat(binary)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("second run replaced the binary")
	}
}

// A marker can outlive the binary it describes: a cleanup script, a new laptop
// restored from a partial backup, an interrupted install. The updater must fix
// that rather than trusting the marker and leaving the machine with no CLI.
func TestCLIAutoupdateReinstallsWhenTheBinaryIsMissing(t *testing.T) {
	requireDeployScriptTools(t, "bash", "curl", "python3")
	fixture := newCLIAutoupdateFixture(t, "v0.1.52")
	fixture.run(t)

	if err := os.Remove(filepath.Join(fixture.installDir, "subrouter")); err != nil {
		t.Fatal(err)
	}
	if output := fixture.run(t); !strings.Contains(output, "updating CLI") {
		t.Fatalf("updater trusted a stale marker over a missing binary:\n%s", output)
	}
	if _, err := os.Stat(filepath.Join(fixture.installDir, "subrouter")); err != nil {
		t.Fatalf("binary was not restored: %v", err)
	}
}

// The GitHub API is rate limited per source IP: a busy address gets 403 on
// every call, and an updater that only knows the API stops updating silently.
// The releases/latest redirect answers the same question without a limit, so
// the updater must prefer it and must not need the API at all.
func TestCLIAutoupdateResolvesTheTagFromTheReleaseRedirect(t *testing.T) {
	requireDeployScriptTools(t, "bash", "curl", "python3")
	fixture := newCLIAutoupdateFixture(t, "v0.1.52")

	redirects := 0
	releases := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases/latest" {
			redirects++
			http.Redirect(w, r, "/releases/tag/v0.1.52", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer releases.Close()

	// No API URL at all, and the environment is otherwise the working fixture:
	// if the script still needs the API this run fails.
	env := make([]string, 0, len(fixture.env))
	for _, entry := range fixture.env {
		if strings.HasPrefix(entry, "SUBROUTER_RELEASE_API_URL=") {
			continue
		}
		env = append(env, entry)
	}
	fixture.env = append(env,
		"SUBROUTER_RELEASE_LATEST_URL="+releases.URL+"/releases/latest",
		"SUBROUTER_RELEASE_API_URL=",
	)

	fixture.run(t)
	if redirects == 0 {
		t.Fatal("the updater never asked the releases/latest redirect")
	}
	installed, err := os.ReadFile(fixture.versionFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(installed)) != "v0.1.52" {
		t.Fatalf("installed version = %q, want the tag from the redirect", installed)
	}
}

// installCLIReporting puts a binary in the install dir that answers `version`
// with the given version, standing in for a CLI built outside the releases.
func (f cliAutoupdateFixture) installCLIReporting(t *testing.T, version string) string {
	t.Helper()
	if err := os.MkdirAll(f.installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(f.installDir, "subrouter")
	body := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = version ]; then echo \"sr %s (commit test, built now)\"; fi\nexit 0\n", version)
	if err := os.WriteFile(binary, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return body
}

// The CLI updater must not replace a build from main (a pseudo-version newer
// than the latest release) or a plain development build with an older release.
func TestCLIAutoupdateNeverDowngrades(t *testing.T) {
	requireDeployScriptTools(t, "bash", "curl", "python3")
	for _, tc := range []struct {
		installed string
		want      string
	}{
		{"v0.1.53-0.20260927095747-b5bd50354af3", "newer than v0.1.52; not downgrading"},
		{"v0.1.52-rc.1.0.20260927095747-b5bd50354af3+dirty", ""},
		{"v0.2.0", "newer than v0.1.52; not downgrading"},
		{"v0.1.100", "newer than v0.1.52; not downgrading"},
		{"devel", "development build; not replacing it"},
	} {
		t.Run(tc.installed, func(t *testing.T) {
			fixture := newCLIAutoupdateFixture(t, "v0.1.52")
			body := fixture.installCLIReporting(t, tc.installed)
			output := fixture.run(t)
			current, err := os.ReadFile(filepath.Join(fixture.installDir, "subrouter"))
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if !strings.Contains(output, "updating CLI") || string(current) == body {
					t.Fatalf("older prerelease build was not updated:\n%s", output)
				}
				return
			}
			if !strings.Contains(output, tc.want) || strings.Contains(output, "updating CLI") {
				t.Fatalf("output = %q, want %q and no install", output, tc.want)
			}
			if string(current) != body {
				t.Fatal("the updater replaced a newer or development CLI")
			}
		})
	}
}

// An older pseudo-version build (made before the latest release) updates.
func TestCLIAutoupdateUpgradesAnOlderPseudoVersion(t *testing.T) {
	requireDeployScriptTools(t, "bash", "curl", "python3")
	fixture := newCLIAutoupdateFixture(t, "v0.1.52")
	fixture.installCLIReporting(t, "v0.1.52-0.20260101000000-abcdefabcdef")
	if output := fixture.run(t); !strings.Contains(output, "updating CLI none -> v0.1.52") {
		t.Fatalf("older pseudo-version was not updated:\n%s", output)
	}
}
