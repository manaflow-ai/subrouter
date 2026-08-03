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
	requireDeployScriptTools(t, "sh", "curl")
	requireChecksumTool(t)
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
	command := exec.Command(mustLookPath(t, "sh"), filepath.Join(repoRoot, "install.sh"))
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

func TestInstallScriptDoesNotReplaceBinaryWhenVersionMarkerCannotBeStaged(t *testing.T) {
	requireDeployScriptTools(t, "sh", "curl")
	requireChecksumTool(t)
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	assetDir := t.TempDir()
	asset := fmt.Sprintf("subrouter_0.1.52_%s_%s", runtime.GOOS, releaseArchForTest(runtime.GOARCH))
	body := []byte("new verified release bytes\n")
	if err := os.WriteFile(filepath.Join(assetDir, asset), body, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if err := os.WriteFile(filepath.Join(assetDir, "SHA256SUMS"), []byte(fmt.Sprintf("%x  %s\n", sum, asset)), 0o644); err != nil {
		t.Fatal(err)
	}

	installDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	installedPath := filepath.Join(installDir, "subrouter")
	previous := []byte("previous working binary\n")
	if err := os.WriteFile(installedPath, previous, 0o755); err != nil {
		t.Fatal(err)
	}
	blockedParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("block mkdir\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(mustLookPath(t, "sh"), filepath.Join(repoRoot, "install.sh"))
	command.Env = append(os.Environ(),
		"SUBROUTER_VERSION=0.1.52",
		"SUBROUTER_INSTALL_DIR="+installDir,
		"SUBROUTER_INSTALL_ALIASES=0",
		"SUBROUTER_DOWNLOAD_BASE=file://"+assetDir,
		"SUBROUTER_VERSION_FILE="+filepath.Join(blockedParent, "subrouter-version"),
	)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("install.sh unexpectedly succeeded:\n%s", output)
	}
	got, err := os.ReadFile(installedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(previous) {
		t.Fatalf("failed install replaced binary with %q, want previous bytes", got)
	}

	markerDirectory := t.TempDir()
	command = exec.Command(mustLookPath(t, "sh"), filepath.Join(repoRoot, "install.sh"))
	command.Env = append(os.Environ(),
		"SUBROUTER_VERSION=0.1.52",
		"SUBROUTER_INSTALL_DIR="+installDir,
		"SUBROUTER_INSTALL_ALIASES=0",
		"SUBROUTER_DOWNLOAD_BASE=file://"+assetDir,
		"SUBROUTER_VERSION_FILE="+markerDirectory,
	)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("install.sh accepted a directory as its version marker:\n%s", output)
	}
	got, err = os.ReadFile(installedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(previous) {
		t.Fatalf("directory marker install replaced binary with %q, want previous bytes", got)
	}
}

func TestGCPReleaseFetcherVerifiesBeforePublishingCandidate(t *testing.T) {
	requireDeployScriptTools(t, "bash", "curl", "sha256sum")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	assetDir := t.TempDir()
	asset := "subrouter_0.1.52_linux_amd64"
	body := []byte("linux release bytes\n")
	if err := os.WriteFile(filepath.Join(assetDir, asset), body, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	checksumPath := filepath.Join(assetDir, "SHA256SUMS")
	if err := os.WriteFile(checksumPath, []byte(fmt.Sprintf("%x  %s\n", sum, asset)), 0o644); err != nil {
		t.Fatal(err)
	}

	outputPath := filepath.Join(t.TempDir(), "subrouter-linux-amd64")
	run := func() ([]byte, error) {
		command := exec.Command(mustLookPath(t, "bash"), filepath.Join(repoRoot, "deploy", "gcp", "fetch-release.sh"))
		command.Env = append(os.Environ(),
			"SUBROUTER_RELEASE_TAG=v0.1.52",
			"SUBROUTER_RELEASE_ARCH=amd64",
			"SUBROUTER_RELEASE_BASE=file://"+assetDir,
			"SUBROUTER_RELEASE_OUTPUT="+outputPath,
		)
		return command.CombinedOutput()
	}
	if output, err := run(); err != nil {
		t.Fatalf("fetch release failed: %v\n%s", err, output)
	}
	got, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("candidate = %q, want verified release bytes", got)
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("candidate mode = %o, want executable", info.Mode().Perm())
	}

	previous := []byte("keep this verified candidate\n")
	if err := os.WriteFile(outputPath, previous, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(checksumPath, []byte(fmt.Sprintf("%064x  %s\n", 0, asset)), 0o644); err != nil {
		t.Fatal(err)
	}
	if output, err := run(); err == nil {
		t.Fatalf("tampered release unexpectedly succeeded:\n%s", output)
	}
	got, err = os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(previous) {
		t.Fatalf("failed verification replaced candidate with %q", got)
	}
}

func TestPublishSubrouterRejectsNonHTTPSManagedURLBeforeMutation(t *testing.T) {
	requireDeployScriptTools(t, "bash")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	command := exec.Command(mustLookPath(t, "bash"), filepath.Join(repoRoot, "deploy", "gcp", "publish-subrouter.sh"))
	command.Env = append(os.Environ(),
		"SERVER_URL=http://203.0.113.10:31415",
		"SUBROUTER_RELEASE_TAG=v0.1.52",
		"SR_BIN=definitely-not-an-installed-sr-binary",
	)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("publish script accepted non-HTTPS URL:\n%s", output)
	}
	if !strings.Contains(strings.ToLower(string(output)), "https") {
		t.Fatalf("publish error did not explain HTTPS requirement:\n%s", output)
	}
}

func TestGCPVerifierAlertsWhenEveryConfiguredProviderAccountIsUnusable(t *testing.T) {
	requireDeployScriptTools(t, "bash", "python3", "curl")
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

	command := exec.Command(mustLookPath(t, "bash"), filepath.Join(repoRoot, "deploy", "gcp", "subrouter-verify.sh"))
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
		"release_tag:",
		"operation:",
		"migrate-front",
		"id-token: write",
		"google-github-actions/auth@v3",
		"environment: subrouter-${{ inputs.deploy_environment }}",
		"deploy/gcp/fetch-release.sh",
		"release-source",
		"vcs.revision=${release_commit}",
		"vcs.modified=false",
		"deploy/gcp/migrate-to-front-slots.sh",
		"deploy/gcp/deploy-live-upgrade.sh",
		"SUBROUTER_DEPLOY_MODE: rollback-rehearsal",
		"SUBROUTER_RELEASE_SHA256_FILE",
		"https://staging.sr.cmux.com",
		"https://sr.cmux.com",
		"SUBROUTER_DEPLOY_TENANT_KEY",
	} {
		if !strings.Contains(string(workflow), want) {
			t.Fatalf("GCP deploy workflow missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"branches: [main]",
		"CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build",
		"subrouter-staging.cmux.dev",
	} {
		if strings.Contains(string(workflow), forbidden) {
			t.Fatalf("GCP deploy workflow contains forbidden mutable deploy path %q", forbidden)
		}
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
		"/_subrouter/front-status",
		"/_subrouter/switch",
		"prepare-slot",
		"set-front-default",
		"disable-slot",
		"retire-slot",
		"stop-drained-slot",
		"wait_for_front_drained",
		"front topology is not installed",
		"SUBROUTER_TRANSPORT_OBSERVER",
		"transport-evidence.jsonl",
		"rollback-rehearsal",
		"rollback_failed",
		"resume -o",
		"pinned connection(s)",
		"NRestarts",
		"oom_kill",
		"systemctl is-active",
		"/_subrouter/ready",
		`-C "${WORK_DIR}" -s read-only`,
		"SUBROUTER_DEPLOY_CLIENT_BASE_URL",
		"SUBROUTER_DEPLOY_TENANT_KEY",
		`--upstream "${CLIENT_BASE_URL}"`,
		`/opt/subrouter/releases/${RELEASE_TAG}/subrouter`,
	} {
		if !strings.Contains(string(script), want) {
			t.Fatalf("live GCP upgrade verifier missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"kill -STOP",
		"kill -CONT",
		`/usr/local/bin/subrouter.incoming`,
		`127.0.0.1:${local_port}:127.0.0.1:31415`,
	} {
		if strings.Contains(string(script), forbidden) {
			t.Fatalf("live GCP upgrade verifier routes users through IAP: %q", forbidden)
		}
	}
}

func TestGCPDeploymentEvidenceGateValidatesOutcomes(t *testing.T) {
	requireDeployScriptTools(t, "python3")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	validator := filepath.Join(repoRoot, "deploy", "gcp", "validate-deploy-evidence.py")
	run := func(expect string, evidence string) ([]byte, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "evidence.json")
		if err := os.WriteFile(path, []byte(evidence), 0o600); err != nil {
			t.Fatal(err)
		}
		command := exec.Command(mustLookPath(t, "python3"), validator, "--expect", expect, path)
		return command.CombinedOutput()
	}

	activation := `{
  "schema":"subrouter.gcp.deploy-evidence/v1",
  "evidence_type":"slot-activation",
  "mode":"deploy",
  "success":true,
  "run":{"id":"run-1","project":"project","zone":"zone","instance":"instance"},
  "release":{"tag":"v1.2.3","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","source_revision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","tag_on_main":true,"attestation_verified":true},
  "slots":{"before":"slot-a","candidate":"slot-b","final":"slot-b","old_generation":"old-generation","candidate_generation":"new-generation"},
  "checksums":{"installed_before":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","candidate_installed":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","installed_after":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
  "timestamps":{"upgrade_requested_at":"2026-08-02T10:00:00Z","activated_at":"2026-08-02T10:00:01Z","evidence_emitted_at":"2026-08-02T10:00:03Z"},
  "front":{"active_before":{"id":"slot-a","network":"tcp","address":"127.0.0.1:31417"},"active_after":{"id":"slot-b","network":"tcp","address":"127.0.0.1:31418"},"active_final":{"id":"slot-b","network":"tcp","address":"127.0.0.1:31418"}},
  "old_slot":{"before":{"accepting":true,"retiring":false,"front_active":true,"active_generation":"old-generation","active_connections":4,"inactive_connections":0,"service_active":true},"after":{"accepting":false,"retiring":true,"front_active":false,"active_generation":"old-generation","active_connections":4,"inactive_connections":0,"service_active":true}},
  "metrics":{"old_slot":{"nrestarts":{"before":0,"after":0},"oom_kill":{"before":0,"after":0},"run_scoped_peak_rss_bytes":150000000,"memory_max_bytes":201326592},"candidate_slot":{"nrestarts":{"before":0,"after":0},"oom_kill":{"before":0,"after":0},"run_scoped_peak_rss_bytes":180000000,"memory_max_bytes":201326592},"front":{"nrestarts":{"before":0,"after":0},"oom_kill":{"before":0,"after":0},"run_scoped_peak_rss_bytes":100000000,"memory_max_bytes":134217728}},
  "continuity":{"configured_original_clients":4,"pinned_original_connections_at_switch":4,"all_original_clients_pinned":true,"transports":["http","websocket"],"resumed_contexts":4,"resume_nonce_verified":true,"ci_evidence_role":"supplemental","golden_gate_role":"authoritative"},
  "rollback":{"performed":false,"requested_at":null,"activated_at":null,"from":null,"to":null},
  "retirement":{"target":"slot-a","requested_at":"2026-08-02T10:00:02Z","state":"pending","evidence_file_required":true}
}`
	if output, err := run("slot-activation", activation); err != nil {
		t.Fatalf("valid activation evidence was rejected: %v\n%s", err, output)
	}

	wrongFinal := strings.Replace(activation, `"final":"slot-b"`, `"final":"slot-a"`, 1)
	if output, err := run("slot-activation", wrongFinal); err == nil {
		t.Fatalf("candidate-inactive deploy evidence was accepted:\n%s", output)
	}
	missingOriginal := strings.Replace(activation, `"pinned_original_connections_at_switch":4`, `"pinned_original_connections_at_switch":3`, 1)
	if output, err := run("slot-activation", missingOriginal); err == nil {
		t.Fatalf("partial original-client evidence was accepted:\n%s", output)
	}
	overMemory := strings.Replace(activation, `"run_scoped_peak_rss_bytes":180000000`, `"run_scoped_peak_rss_bytes":201326593`, 1)
	if output, err := run("slot-activation", overMemory); err == nil {
		t.Fatalf("over-limit peak RSS evidence was accepted:\n%s", output)
	}

	retirement := `{
  "schema":"subrouter.gcp.deploy-evidence/v1",
  "evidence_type":"slot-retirement",
  "mode":"deploy",
  "success":true,
  "activation_evidence_sha256":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
  "run":{"id":"run-1","project":"project","zone":"zone","instance":"instance"},
  "slots":{"retired":"slot-a","active":"slot-b","retired_generation":"old-generation"},
  "front":{"active":{"id":"slot-b","network":"tcp","address":"127.0.0.1:31418"},"retired_connections_after":0},
  "retirement":{"requested_at":"2026-08-02T10:00:02Z","last_connection_closed_at":"2026-08-02T10:01:00Z","absent_at":"2026-08-02T10:01:01Z","absence_latency_ms":1000,"service_active_after":false,"control_socket_present_after":false,"enabled_after":false},
  "metrics":{"old_slot":{"nrestarts":{"before":0,"after":0},"oom_kill":{"before":0,"after":0}}},
  "evidence_emitted_at":"2026-08-02T10:01:02Z"
}`
	if output, err := run("slot-retirement", retirement); err != nil {
		t.Fatalf("valid retirement evidence was rejected: %v\n%s", err, output)
	}
	lateAbsence := strings.Replace(retirement, `"absence_latency_ms":1000`, `"absence_latency_ms":30001`, 1)
	if output, err := run("slot-retirement", lateAbsence); err == nil {
		t.Fatalf("late old-slot absence evidence was accepted:\n%s", output)
	}
}

func TestFrontSlotInstallerPersistsRebootAndRollbackTargets(t *testing.T) {
	requireDeployScriptTools(t, "bash", "curl", "jq")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	fakeBin := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "systemctl.log")
	writeExecutableTestFile(t, filepath.Join(fakeBin, "id"), "#!/bin/sh\nprintf '0\\n'\n")
	writeExecutableTestFile(t, filepath.Join(fakeBin, "python3"), "#!/bin/sh\nexit 0\n")
	writeExecutableTestFile(t, filepath.Join(fakeBin, "systemctl"), `#!/bin/sh
printf '%s\n' "$*" >>"$SYSTEMCTL_LOG"
exit 0
`)
	frontEnv := filepath.Join(t.TempDir(), "subrouter-front")
	run := func(args ...string) ([]byte, error) {
		command := exec.Command(mustLookPath(t, "bash"), append([]string{filepath.Join(repoRoot, "deploy", "gcp", "install-front-slots.sh")}, args...)...)
		command.Env = append(os.Environ(),
			"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"SYSTEMCTL_LOG="+logPath,
			"SUBROUTER_FRONT_ENV="+frontEnv,
		)
		return command.CombinedOutput()
	}
	if output, err := run("enable-slot", "slot-b"); err != nil {
		t.Fatalf("enable slot: %v\n%s", err, output)
	}
	if output, err := run("set-front-default", "slot-b"); err != nil {
		t.Fatalf("persist candidate: %v\n%s", err, output)
	}
	assertFrontEnvSlot(t, frontEnv, "slot-b", "127.0.0.1:31418")
	if output, err := run("disable-slot", "slot-a"); err != nil {
		t.Fatalf("disable old slot: %v\n%s", err, output)
	}
	if output, err := run("enable-slot", "slot-a"); err != nil {
		t.Fatalf("re-enable rollback slot: %v\n%s", err, output)
	}
	if output, err := run("set-front-default", "slot-a"); err != nil {
		t.Fatalf("persist rollback: %v\n%s", err, output)
	}
	assertFrontEnvSlot(t, frontEnv, "slot-a", "127.0.0.1:31417")

	logBody, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logBody)
	for _, want := range []string{
		"enable --now subrouter-slot@slot-b.service",
		"disable subrouter-slot@slot-a.service",
		"enable --now subrouter-slot@slot-a.service",
	} {
		if !strings.Contains(logText, want) {
			t.Fatalf("systemctl log missing %q:\n%s", want, logText)
		}
	}
	if strings.Contains(logText, "disable --now subrouter-slot@slot-a.service") {
		t.Fatalf("old slot was stopped while disabling reboot activation:\n%s", logText)
	}
}

func assertFrontEnvSlot(t *testing.T, path, slot, address string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"SUBROUTER_FRONT_BACKEND_ID=" + slot,
		"SUBROUTER_FRONT_BACKEND_NETWORK=tcp",
		"SUBROUTER_FRONT_BACKEND_ADDRESS=" + address,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("front environment missing %q:\n%s", want, body)
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

func requireDeployScriptTools(t *testing.T, names ...string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX deployment scripts are not runnable on Windows")
	}
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s is required for this deployment-script test", name)
		}
	}
}

func mustLookPath(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("look up %s: %v", name, err)
	}
	return path
}

func requireChecksumTool(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sha256sum"); err == nil {
		return
	}
	if _, err := exec.LookPath("shasum"); err == nil {
		return
	}
	t.Skip("sha256sum or shasum is required for this installer test")
}
