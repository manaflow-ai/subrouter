package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallScriptRecordsTheResolvedReleaseVersion(t *testing.T) {
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	assetDir := t.TempDir()
	asset := fmt.Sprintf("subrouter_0.1.52_%s_%s", runtime.GOOS, releaseArchForTest(runtime.GOARCH))
	if runtime.GOOS == "windows" {
		asset += ".exe"
	}
	body := []byte("verified release bytes\n")
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

	installDir := filepath.Join(t.TempDir(), "bin")
	versionFile := filepath.Join(t.TempDir(), "subrouter-version")
	command := exec.Command("/bin/sh", filepath.Join(repoRoot, "install.sh"))
	command.Env = append(os.Environ(),
		"SUBROUTER_VERSION=0.1.52",
		"SUBROUTER_INSTALL_DIR="+installDir,
		"SUBROUTER_INSTALL_ALIASES=0",
		"SUBROUTER_DOWNLOAD_BASE=file://"+assetDir,
		"SUBROUTER_VERSION_FILE="+versionFile,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, output)
	}
	got, err := os.ReadFile(versionFile)
	if err != nil {
		t.Fatalf("read installed version: %v", err)
	}
	if string(got) != "v0.1.52\n" {
		t.Fatalf("installed version = %q, want v0.1.52", got)
	}
}

func TestGCPVerifierAlertsWhenEveryConfiguredProviderAccountIsUnusable(t *testing.T) {
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	fakeBin := t.TempDir()
	writeExecutableTestFile(t, filepath.Join(fakeBin, "curl"), `#!/bin/sh
case "$*" in
  *"/_subrouter/health"*) printf '%s\n' '{"ok":true}' ;;
  *"/_subrouter/usage-status"*) cat "$SUBROUTER_VERIFY_USAGE_FIXTURE" ;;
  *) exit 1 ;;
esac
`)
	writeExecutableTestFile(t, filepath.Join(fakeBin, "systemctl"), `#!/bin/sh
case "$*" in
  *"is-active"*) [ "$1" = "is-active" ] && printf '%s\n' active; exit 0 ;;
esac
exit 0
`)
	writeExecutableTestFile(t, filepath.Join(fakeBin, "journalctl"), "#!/bin/sh\nexit 0\n")

	fixture := filepath.Join(t.TempDir(), "usage.json")
	if err := os.WriteFile(fixture, []byte(`[
  {"id":"claude-a","provider":"claude","auth_mode":"oauth","auth_checked":true,"auth_valid":false,"error":"unauthorized"},
  {"id":"claude-b","provider":"claude","auth_mode":"oauth","auth_checked":true,"auth_valid":false,"error":"rate limited"},
  {"id":"codex-a","provider":"codex","auth_mode":"oauth","auth_checked":true,"auth_valid":false,"error":"refresh token reused"}
]`), 0o600); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	versionFile := filepath.Join(t.TempDir(), "version")
	if err := os.WriteFile(versionFile, []byte("v0.1.52\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("/bin/bash", filepath.Join(repoRoot, "deploy", "gcp", "subrouter-verify.sh"))
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SUBROUTER_VERIFY_STATE="+stateDir,
		"SUBROUTER_VERIFY_VERSION_FILE="+versionFile,
		"SUBROUTER_VERIFY_USAGE_FIXTURE="+fixture,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("subrouter-verify.sh failed: %v\n%s", err, output)
	}
	text := string(output)
	for _, want := range []string{
		"[ALERT] no usable Claude accounts: 0/2",
		"[ALERT] no usable Codex accounts: 0/1",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("verifier output missing %q:\n%s", want, text)
		}
	}
	status, err := os.ReadFile(filepath.Join(stateDir, "status.json"))
	if err != nil {
		t.Fatalf("read verifier status: %v", err)
	}
	for _, want := range []string{`"healthy_claude":0`, `"total_claude":2`, `"healthy_codex":0`, `"total_codex":1`} {
		if !strings.Contains(string(status), want) {
			t.Fatalf("status missing %q: %s", want, status)
		}
	}
}

func TestGCPDeployWorkflowRequiresLiveCodexDrainGate(t *testing.T) {
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	workflowPath := filepath.Join(repoRoot, ".github", "workflows", "gcp-deploy.yml")
	workflow, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"name: GCP Deploy",
		"workflow_dispatch:",
		"branches: [main]",
		"id-token: write",
		"google-github-actions/auth@v3",
		"environment: subrouter-staging",
		"deploy/gcp/deploy-live-upgrade.sh",
		"SUBROUTER_DEPLOY_MODE: rollback-rehearsal",
		"go version -m dist/subrouter-linux-amd64",
		"https://staging.sr.cmux.com/_subrouter/health",
		"https://sr.cmux.com/_subrouter/health",
		"SUBROUTER_DEPLOY_TENANT_KEY",
	} {
		if !strings.Contains(string(workflow), want) {
			t.Fatalf("GCP deploy workflow missing %q", want)
		}
	}
	if strings.Contains(string(workflow), "subrouter-staging.cmux.dev") {
		t.Fatal("GCP deploy workflow still probes the Cloudflare staging service")
	}

	// Regression: a healthy replacement process is insufficient proof for an
	// agent proxy. The release is deployable only when real WebSocket and HTTP
	// Codex sessions remain attached to the old worker, finish after the swap,
	// and resume through the new worker without a service restart or OOM kill.
	scriptPath := filepath.Join(repoRoot, "deploy", "gcp", "deploy-live-upgrade.sh")
	script, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"codex exec",
		"supports_websockets=false",
		"wait_for_active_generation_change",
		`candidate_generation="$(wait_for_active_generation_change "${old_generation}" "candidate generation")"`,
		`restored_generation="$(wait_for_active_generation_change "${candidate_generation}" "restored generation")"`,
		"find_stream_owner",
		"lsof",
		"SUBROUTER_TRANSPORT_OBSERVER",
		"transport-evidence.jsonl",
		"kill -STOP",
		"kill -CONT",
		"/_subrouter/upgrade",
		"rollback-rehearsal",
		"rollback_failed",
		"exec resume",
		"connections",
		"NRestarts",
		"oom_kill",
		"systemctl is-active",
		"/_subrouter/ready",
		`-C "${WORK_DIR}" -s read-only`,
		"SUBROUTER_DEPLOY_CLIENT_BASE_URL",
		"SUBROUTER_DEPLOY_TENANT_KEY",
		`--upstream "${CLIENT_BASE_URL}"`,
	} {
		if !strings.Contains(string(script), want) {
			t.Fatalf("live GCP upgrade verifier missing %q", want)
		}
	}
	for _, forbidden := range []string{
		`--upstream "${TUNNEL_BASE_URL}"`,
		`SUBROUTER_CODEX_BASE_URL="${TUNNEL_BASE_URL}/v1"`,
	} {
		if strings.Contains(string(script), forbidden) {
			t.Fatalf("live GCP upgrade verifier routes users through IAP: %q", forbidden)
		}
	}
}

func TestGCPBootstrapUsesLoadBalancerAndIAPWithoutTailscaleOrDirectSSH(t *testing.T) {
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	for _, relative := range []string{
		filepath.Join("deploy", "gcp", "create-subrouter-vm.sh"),
		filepath.Join("deploy", "gcp", "publish-subrouter.sh"),
		filepath.Join("deploy", "gcp", "startup.sh"),
	} {
		body, err := os.ReadFile(filepath.Join(repoRoot, relative))
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{
			"TAILSCALE_AUTH_KEY", "tailscale up", "api.ipify.org",
			"subrouter-allow-ssh",
		} {
			if strings.Contains(string(body), forbidden) {
				t.Fatalf("%s still contains private-network bootstrap %q", relative, forbidden)
			}
		}
	}
}

func releaseArchForTest(goarch string) string {
	switch goarch {
	case "arm":
		return "armv7"
	default:
		return goarch
	}
}

func writeExecutableTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
