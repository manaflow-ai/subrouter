package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
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

func TestGCPStartupBuildsPreparedFrontTopologyFromPinnedReleaseMetadata(t *testing.T) {
	requireDeployScriptTools(t, "bash", "curl", "jq", "sha256sum")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	assetDir := t.TempDir()
	metadataDir := t.TempDir()
	installDir := t.TempDir()
	libexecDir := t.TempDir()
	releaseRoot := t.TempDir()
	stateDir := t.TempDir()
	versionFile := filepath.Join(t.TempDir(), "subrouter-version")
	commandLog := filepath.Join(t.TempDir(), "commands.log")
	revision := strings.Repeat("b", 40)
	binaryAsset := "subrouter_0.1.52_linux_amd64"
	assets := map[string][]byte{
		binaryAsset:              []byte("#!/bin/sh\n# vcs.revision=" + revision + "\n# vcs.modified=false\nprintf '%s\\n' \"$*\" >>\"$STARTUP_COMMAND_LOG\"\nexit 0\n"),
		"deployment-contract.py": []byte("#!/usr/bin/env python3\n"),
		"SOURCE_PROVENANCE.json": []byte(`{"tag":"v0.1.52","source_revision":"` + revision + `","tag_on_main":true}` + "\n"),
		"install.sh":             []byte("#!/bin/sh\nexit 0\n"),
		"install-front-slots.sh": []byte(`#!/bin/sh
printf '%s\n' "$*" >>"$STARTUP_COMMAND_LOG"
case "$1" in
  install-release)
    mkdir -p "$SUBROUTER_RELEASE_ROOT/$2"
    cp "$3" "$SUBROUTER_RELEASE_ROOT/$2/subrouter"
    chmod 0755 "$SUBROUTER_RELEASE_ROOT/$2/subrouter"
    ;;
  prepare-fresh-topology)
    mkdir -p "$SUBROUTER_STATE_DIR"
    printf '%s\n' "${3:-slot-a}" >"$SUBROUTER_STATE_DIR/front-topology-prepared"
    ;;
  *) exit 1 ;;
esac
`),
	}
	digests := make(map[string]string, len(assets)+1)
	manifest := strings.Builder{}
	for _, name := range []string{"SOURCE_PROVENANCE.json", "deployment-contract.py", "install.sh", "install-front-slots.sh", binaryAsset} {
		body := assets[name]
		digest := fmt.Sprintf("%x", sha256.Sum256(body))
		digests[name] = digest
		manifest.WriteString(digest + "  " + name + "\n")
		if err := os.WriteFile(filepath.Join(assetDir, name), body, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	manifestBody := []byte(manifest.String())
	digests["SHA256SUMS"] = fmt.Sprintf("%x", sha256.Sum256(manifestBody))
	if err := os.WriteFile(filepath.Join(assetDir, "SHA256SUMS"), manifestBody, 0o644); err != nil {
		t.Fatal(err)
	}
	metadata := fmt.Sprintf(`{
  "schema":"subrouter.gcp.vm-release-metadata/v1",
  "repository":"manaflow-ai/subrouter",
  "release_tag":"v0.1.52",
  "source_revision":"%s",
  "tag_on_main":true,
  "release_immutable":true,
  "strict_build_attestation_verified":true,
  "asset_digest_verified":true,
  "provenance_verified":true,
  "embedded_revision_verified":true,
  "verification_evidence_sha256":"%s",
  "assets":{"SHA256SUMS":"%s","SOURCE_PROVENANCE.json":"%s","deployment-contract.py":"%s","install.sh":"%s","install-front-slots.sh":"%s","%s":"%s"}
}
`, revision, strings.Repeat("c", 64), digests["SHA256SUMS"], digests["SOURCE_PROVENANCE.json"],
		digests["deployment-contract.py"], digests["install.sh"], digests["install-front-slots.sh"], binaryAsset, digests[binaryAsset])
	metadataDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(metadata)))
	if err := os.WriteFile(filepath.Join(metadataDir, "subrouter-release-metadata"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	metadataDigestPath := filepath.Join(metadataDir, "subrouter-release-metadata-sha256")
	if err := os.WriteFile(metadataDigestPath, []byte(metadataDigest+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	releaseState := fmt.Sprintf(`{"tag_name":"v0.1.52","draft":false,"immutable":true,"assets":[
{"name":"SHA256SUMS","digest":"sha256:%s"},
{"name":"SOURCE_PROVENANCE.json","digest":"sha256:%s"},
{"name":"deployment-contract.py","digest":"sha256:%s"},
{"name":"install.sh","digest":"sha256:%s"},
{"name":"install-front-slots.sh","digest":"sha256:%s"},
{"name":"%s","digest":"sha256:%s"}]}`,
		digests["SHA256SUMS"], digests["SOURCE_PROVENANCE.json"], digests["deployment-contract.py"], digests["install.sh"],
		digests["install-front-slots.sh"], binaryAsset, digests[binaryAsset])
	releaseStatePath := filepath.Join(t.TempDir(), "release.json")
	compareStatePath := filepath.Join(t.TempDir(), "compare.json")
	if err := os.WriteFile(releaseStatePath, []byte(releaseState), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(compareStatePath, []byte(fmt.Sprintf(`{"status":"ahead","merge_base_commit":{"sha":"%s"}}`, revision)), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func() ([]byte, error) {
		command := exec.Command(mustLookPath(t, "bash"), filepath.Join(repoRoot, "deploy", "gcp", "startup.sh"))
		command.Env = append(os.Environ(),
			"SUBROUTER_STARTUP_SKIP_PACKAGES=1",
			"SUBROUTER_STARTUP_SKIP_VERIFY_TIMER=1",
			"SUBROUTER_METADATA_BASE_URL=file://"+metadataDir,
			"SUBROUTER_RELEASE_API_URL=file://"+releaseStatePath,
			"SUBROUTER_COMPARE_API_URL=file://"+compareStatePath,
			"SUBROUTER_RELEASE_BASE=file://"+assetDir,
			"SUBROUTER_SYSTEM_INSTALL_DIR="+installDir,
			"SUBROUTER_SYSTEM_LIBEXEC_DIR="+libexecDir,
			"SUBROUTER_VERSION_FILE="+versionFile,
			"SUBROUTER_RELEASE_ROOT="+releaseRoot,
			"SUBROUTER_SLOT_ROOT="+filepath.Join(t.TempDir(), "slots"),
			"SUBROUTER_FRONT_ROOT="+filepath.Join(t.TempDir(), "front"),
			"SUBROUTER_CONTROL_ROOT="+filepath.Join(t.TempDir(), "control"),
			"SUBROUTER_STATE_DIR="+stateDir,
			"SUBROUTER_FRONT_ENV="+filepath.Join(t.TempDir(), "front-defaults"),
			"SUBROUTER_DEFAULTS_FILE="+filepath.Join(t.TempDir(), "defaults"),
			"SUBROUTER_SLOT_UNIT="+filepath.Join(t.TempDir(), "slot.service"),
			"SUBROUTER_FRONT_UNIT="+filepath.Join(t.TempDir(), "front.service"),
			"STARTUP_COMMAND_LOG="+commandLog,
		)
		return command.CombinedOutput()
	}
	if output, err := run(); err != nil {
		t.Fatalf("startup failed: %v\n%s", err, output)
	}
	marker, err := os.ReadFile(filepath.Join(stateDir, "front-topology-prepared"))
	if err != nil {
		t.Fatalf("read prepared marker: %v", err)
	}
	if string(marker) != "slot-a\n" {
		t.Fatalf("prepared slot = %q, want slot-a", marker)
	}
	retained, err := os.ReadFile(filepath.Join(releaseRoot, "v0.1.52", "subrouter"))
	if err != nil {
		t.Fatalf("read retained release: %v", err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(retained)) != digests[binaryAsset] {
		t.Fatal("retained release digest changed")
	}
	for _, path := range []string{
		filepath.Join(libexecDir, "subrouter-deployment-contract"),
		filepath.Join(releaseRoot, "v0.1.52", "deployment-contract.py"),
	} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read installed deployment contract %s: %v", path, err)
		}
		if fmt.Sprintf("%x", sha256.Sum256(body)) != digests["deployment-contract.py"] {
			t.Fatalf("deployment contract digest changed at %s", path)
		}
	}
	logBody, err := os.ReadFile(commandLog)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logBody)
	for _, want := range []string{
		"install-systemd --addr 0.0.0.0:31415 --cx-switch-interval 10m --start=false",
		"install-release v0.1.52",
		"prepare-fresh-topology v0.1.52 slot-a",
	} {
		if !strings.Contains(logText, want) {
			t.Fatalf("startup command log missing %q:\n%s", want, logText)
		}
	}

	if err := os.WriteFile(metadataDigestPath, []byte(strings.Repeat("0", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := run(); err == nil {
		t.Fatalf("startup accepted a mismatched metadata digest:\n%s", output)
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

func TestDeployLockReleasesWhenOwningShellIsKilled(t *testing.T) {
	requireDeployScriptTools(t, "awk", "bash", "chmod", "grep", "kill", "mkfifo", "mktemp", "rmdir", "sleep", "unlink")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	helper := filepath.Join(repoRoot, "deploy", "gcp", "deploy-lock.sh")
	fakeBin := t.TempDir()
	holderPID := filepath.Join(t.TempDir(), "holder.pid")
	longChildPID := filepath.Join(t.TempDir(), "long-child.pid")
	remoteLock := filepath.Join(t.TempDir(), "remote.lock")
	lockLog := filepath.Join(t.TempDir(), "deploy-lock.log")
	acquired := filepath.Join(t.TempDir(), "acquired")
	if err := os.WriteFile(lockLog, []byte("STALE\nLOCKED\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeGcloud := filepath.Join(fakeBin, "gcloud")
	writeExecutableTestFile(t, fakeGcloud, `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$$" >"$HOLDER_PID_FILE"
: >"$REMOTE_LOCK_FILE"
cleanup() { unlink "$REMOTE_LOCK_FILE" 2>/dev/null || true; }
trap cleanup EXIT
printf 'LOCKED\n'
while IFS= read -r heartbeat; do printf 'ACK %s\n' "$heartbeat"; done
`)

	harness := `
set -euo pipefail
source "$1"
GCLOUD_BINARY="$2"
INSTANCE=subrouter-staging
PROJECT_ID=project
ZONE=us-south1-a
DEPLOY_LOCK_FILE=/run/lock/subrouter-deploy.lock
subrouter_acquire_deploy_lock "$3" "$GCLOUD_BINARY" "$INSTANCE" "$PROJECT_ID" "$ZONE" "$DEPLOY_LOCK_FILE"
printf 'acquired\n' >"$4"
sleep 30 >/dev/null 2>&1 &
printf '%s\n' "$!" >"$5"
wait
`
	var output strings.Builder
	command := exec.Command(mustLookPath(t, "bash"), "-c", harness, "deploy-lock-test", helper, fakeGcloud, lockLog, acquired, longChildPID)
	command.Env = append(os.Environ(),
		"HOLDER_PID_FILE="+holderPID,
		"REMOTE_LOCK_FILE="+remoteLock,
		"SUBROUTER_DEPLOY_LOCK_HEARTBEAT_INTERVAL_SECONDS=0.05",
		"SUBROUTER_DEPLOY_LOCK_ACK_TIMEOUT_SECONDS=1",
		"SUBROUTER_DEPLOY_LOCK_HEARTBEAT_TIMEOUT_SECONDS=2",
	)
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = command.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		if childPIDBytes, err := os.ReadFile(longChildPID); err == nil {
			childPID, parseErr := strconv.Atoi(strings.TrimSpace(string(childPIDBytes)))
			if parseErr != nil {
				t.Errorf("parse long-lived child pid: %v", parseErr)
			} else if child, findErr := os.FindProcess(childPID); findErr != nil {
				t.Errorf("find long-lived child process: %v", findErr)
			} else if killErr := child.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
				t.Errorf("kill long-lived child process: %v", killErr)
			}
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("deploy lock process tree did not exit\n%s", output.String())
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, acquiredErr := os.Stat(acquired); acquiredErr == nil {
			if _, childErr := os.Stat(longChildPID); childErr != nil {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			if _, lockErr := os.Stat(remoteLock); lockErr != nil {
				t.Fatalf("helper reported acquisition without a held lock: %v", lockErr)
			}
			lockOutput, readErr := os.ReadFile(lockLog)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if strings.Contains(string(lockOutput), "STALE") {
				t.Fatalf("deployment lock accepted a stale acquisition marker:\n%s", lockOutput)
			}
			break
		}
		select {
		case <-done:
			t.Fatalf("lock owner exited before acquisition: %v\n%s", waitErr, output.String())
		default:
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("timed out acquiring fake deployment lock and its process tree did not exit\n%s", output.String())
			}
			t.Fatalf("timed out acquiring fake deployment lock\n%s", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("deploy lock descendants outlived the killed owner; holder pid file %s\n%s", holderPID, output.String())
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(remoteLock); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("remote lock survived owner death; holder pid file %s\n%s", holderPID, output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDeployLockTerminatesOwnerWhenHeartbeatAcknowledgementsStop(t *testing.T) {
	requireDeployScriptTools(t, "awk", "bash", "chmod", "grep", "kill", "mkfifo", "mktemp", "rmdir", "sleep", "unlink")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	helper := filepath.Join(repoRoot, "deploy", "gcp", "deploy-lock.sh")
	fakeBin := t.TempDir()
	remoteLock := filepath.Join(t.TempDir(), "remote.lock")
	lockLog := filepath.Join(t.TempDir(), "deploy-lock.log")
	acquired := filepath.Join(t.TempDir(), "acquired")
	fakeGcloud := filepath.Join(fakeBin, "gcloud")
	writeExecutableTestFile(t, fakeGcloud, `#!/usr/bin/env bash
set -euo pipefail
: >"$REMOTE_LOCK_FILE"
cleanup() { unlink "$REMOTE_LOCK_FILE" 2>/dev/null || true; }
trap cleanup EXIT
printf 'LOCKED\n'
while IFS= read -r _; do :; done
`)

	harness := `
set -euo pipefail
source "$1"
subrouter_acquire_deploy_lock "$2" "$3" instance project zone /run/lock/subrouter-deploy.lock
printf 'acquired\n' >"$4"
while :; do sleep 1; done
`
	var output strings.Builder
	command := exec.Command(mustLookPath(t, "bash"), "-c", harness, "deploy-lock-ack-test", helper, lockLog, fakeGcloud, acquired)
	command.Env = append(os.Environ(),
		"REMOTE_LOCK_FILE="+remoteLock,
		"SUBROUTER_DEPLOY_LOCK_HEARTBEAT_INTERVAL_SECONDS=0.05",
		"SUBROUTER_DEPLOY_LOCK_ACK_TIMEOUT_SECONDS=1",
		"SUBROUTER_DEPLOY_LOCK_HEARTBEAT_TIMEOUT_SECONDS=10",
	)
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = command.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("unacknowledged deploy lock process tree did not exit\n%s", output.String())
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, acquiredErr := os.Stat(acquired); acquiredErr == nil {
			if _, lockErr := os.Stat(remoteLock); lockErr != nil {
				t.Fatalf("helper reported acquisition without a held lock: %v", lockErr)
			}
			break
		}
		select {
		case <-done:
			t.Fatalf("lock owner exited before acquisition: %v\n%s", waitErr, output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out acquiring fake deployment lock\n%s", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case <-done:
		if waitErr == nil {
			t.Fatalf("lock owner exited successfully after acknowledgement loss\n%s", output.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("lock owner kept running without remote heartbeat acknowledgements\n%s", output.String())
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(remoteLock); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("remote lock survived acknowledgement failure\n%s", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDeployLockOwnerCleanupRemovesRunScopedSamplerSentinel(t *testing.T) {
	requireDeployScriptTools(t, "awk", "bash", "chmod", "grep", "kill", "mkfifo", "mktemp", "rmdir", "sleep", "unlink")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	helper := filepath.Join(repoRoot, "deploy", "gcp", "deploy-lock.sh")
	fakeBin := t.TempDir()
	sentinel := filepath.Join(t.TempDir(), "subrouter-rss-owner-cleanup-front.running")
	lockLog := filepath.Join(t.TempDir(), "deploy-lock.log")
	if err := os.WriteFile(sentinel, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fakeGcloud := filepath.Join(fakeBin, "gcloud")
	writeExecutableTestFile(t, fakeGcloud, `#!/usr/bin/env bash
set -euo pipefail
remote_command="$*"
cleanup() {
  if [[ "${remote_command}" == *"subrouter-rss-owner-cleanup-*.running"* ]]; then
    unlink "${REMOTE_SAMPLER_SENTINEL}" 2>/dev/null || true
  fi
}
trap cleanup EXIT
printf 'LOCKED\n'
while IFS= read -r heartbeat; do printf 'ACK %s\n' "$heartbeat"; done
`)

	harness := `
set -euo pipefail
source "$1"
subrouter_acquire_deploy_lock "$2" "$3" instance project zone /run/lock/subrouter-deploy.lock owner-cleanup
subrouter_release_deploy_lock
`
	command := exec.Command(mustLookPath(t, "bash"), "-c", harness, "deploy-lock-cleanup-test", helper, lockLog, fakeGcloud)
	command.Env = append(os.Environ(),
		"REMOTE_SAMPLER_SENTINEL="+sentinel,
		"SUBROUTER_DEPLOY_LOCK_HEARTBEAT_INTERVAL_SECONDS=0.05",
		"SUBROUTER_DEPLOY_LOCK_ACK_TIMEOUT_SECONDS=1",
		"SUBROUTER_DEPLOY_LOCK_HEARTBEAT_TIMEOUT_SECONDS=2",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("deploy lock cleanup harness: %v\n%s", err, output)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("run-scoped sampler sentinel survived lock release: %v", err)
	}
}

func TestCreateVMTempFilesSurviveInterruptedAndRepeatedMacOSRuns(t *testing.T) {
	requireDeployScriptTools(t, "bash", "dd", "tr")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	fakeBin := t.TempDir()
	tempDir := t.TempDir()
	artifactDir := t.TempDir()
	for path := range map[string]bool{
		filepath.Join(tempDir, "subrouter-gce-instance.XXXXXX.json"):  true,
		filepath.Join(artifactDir, "vm-release-metadata.XXXXXX.json"): true,
	} {
		if err := os.WriteFile(path, []byte("interrupted-run\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	digest := strings.Repeat("a", 64)
	revision := strings.Repeat("b", 40)
	createdAt := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	assetDir := t.TempDir()
	binaryAsset := "subrouter_1.2.3_linux_amd64"
	manifest := strings.Builder{}
	for _, name := range []string{"SOURCE_PROVENANCE.json", "deployment-contract.py", "install.sh", "install-front-slots.sh", binaryAsset} {
		if err := os.WriteFile(filepath.Join(assetDir, name), []byte(name+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		manifest.WriteString(digest + "  " + name + "\n")
	}
	if err := os.WriteFile(filepath.Join(assetDir, "SHA256SUMS"), []byte(manifest.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	verification := filepath.Join(t.TempDir(), "release-verification.json")
	if err := os.WriteFile(verification, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	writeExecutableTestFile(t, filepath.Join(fakeBin, "sha256sum"), "#!/bin/sh\nprintf '"+digest+"  %s\\n' \"$1\"\n")
	writeExecutableTestFile(t, filepath.Join(fakeBin, "go"), `#!/bin/sh
dd if=/dev/zero bs=1048576 count=2 2>/dev/null | tr '\000' x
printf '\nvcs.revision=`+revision+`\nvcs.modified=false\n'
`)
	writeExecutableTestFile(t, filepath.Join(fakeBin, "gh"), `#!/bin/sh
if [ "$1 $2" = "attestation verify" ]; then
  printf '[{"padding":"'
  dd if=/dev/zero bs=1048576 count=2 2>/dev/null | tr '\000' x
  printf '"}]\n'
else
  printf '{}\n'
fi
`)
	writeExecutableTestFile(t, filepath.Join(fakeBin, "jq"), `#!/bin/sh
case "$*" in
  *"length > 0"*) cat >/dev/null ;;
  *".source_revision"*) printf '%s\n' "$TEST_REVISION" ;;
  *".assets["*) printf '%s\n' "$TEST_DIGEST" ;;
  *"subrouter-release-metadata-sha256"*) printf '%s\n' "$TEST_DIGEST" ;;
  *".creation_timestamp"*) printf '%s\n' "$TEST_CREATED_AT" ;;
  *".id"*) printf '%s\n' 1234567890123456789 ;;
  *"-n"*|*"-c ."*) printf '{}\n' ;;
  *) cat >/dev/null ;;
esac
exit 0
`)
	writeExecutableTestFile(t, filepath.Join(fakeBin, "python3"), `#!/bin/sh
case "$*" in
  *"datetime.now"*) printf '%s\n' "$TEST_CREATED_AT" ;;
  *"hashlib,json,sys"*) printf '%s\n' "$TEST_DIGEST" ;;
  *)
    if [ "$#" -eq 4 ] && [ "$1" = "-" ]; then
      printf '{"creation_timestamp":"%s","id":"1234567890123456789"}\n' "$TEST_CREATED_AT"
    fi
    ;;
esac
exit 0
`)
	writeExecutableTestFile(t, filepath.Join(fakeBin, "gcloud"), `#!/bin/sh
case "$*" in
  "config get-value account") printf '%s\n' operator@example.com ;;
  *"instances describe"*) printf '{}\n' ;;
  *"subrouter-verify-fresh-vm"*) printf '{"kind":"front-slots","state":"prepared"}\n' ;;
esac
exit 0
`)

	evidencePath := filepath.Join(artifactDir, "result.json")
	run := func() ([]byte, error, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, mustLookPath(t, "bash"),
			filepath.Join(repoRoot, "deploy", "gcp", "create-subrouter-vm.sh"),
			"--evidence-json", evidencePath,
		)
		command.Env = append(upsertEnv(os.Environ(), "TMPDIR", tempDir),
			"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"PROJECT_ID=project",
			"SUBROUTER_RELEASE_TAG=v1.2.3",
			"SUBROUTER_RELEASE_ASSET_DIR="+assetDir,
			"SUBROUTER_RELEASE_VERIFICATION_JSON="+verification,
			"SUBROUTER_DEPLOY_ARTIFACT_DIR="+artifactDir,
			"TEST_DIGEST="+digest,
			"TEST_REVISION="+revision,
			"TEST_CREATED_AT="+createdAt,
		)
		output, err := runDeployTestCommand(command)
		return output, err, ctx.Err()
	}
	for invocation := 1; invocation <= 2; invocation++ {
		output, err, contextErr := run()
		if contextErr != nil {
			t.Fatalf("create invocation %d timed out: %v\n%s", invocation, contextErr, output)
		}
		if err != nil {
			t.Fatalf("create invocation %d failed: %v\n%s", invocation, err, output)
		}
	}
	for _, path := range []string{
		filepath.Join(tempDir, "subrouter-gce-instance.XXXXXX.json"),
		filepath.Join(artifactDir, "vm-release-metadata.XXXXXX.json"),
	} {
		body, err := os.ReadFile(path)
		if err != nil || string(body) != "interrupted-run\n" {
			t.Fatalf("legacy collision sentinel changed at %s: %q, %v", path, body, err)
		}
	}
}

func TestPublishFreshVMEmitsAuthenticatedActiveAcceptanceEvidence(t *testing.T) {
	requireDeployScriptTools(t, "bash", "jq", "python3", "sha256sum")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	fakeBin := t.TempDir()
	publishTmp := t.TempDir()
	for _, legacyName := range []string{
		"subrouter-gce-instance.XXXXXX.json",
		"subrouter-vm-bootstrap.XXXXXX.json",
		"subrouter-fresh-topology.XXXXXX.json",
	} {
		if err := os.WriteFile(filepath.Join(publishTmp, legacyName), []byte("occupied\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutableTestFile(t, filepath.Join(fakeBin, "curl"), "#!/bin/sh\nexit 0\n")
	writeExecutableTestFile(t, filepath.Join(fakeBin, "sr"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >>\"$SR_LOG\"\nexit 0\n")
	writeExecutableTestFile(t, filepath.Join(fakeBin, "gcloud"), `#!/bin/sh
case "$*" in
  "config get-value account") printf '%s\n' operator@example.com ;;
  "config get-value project") printf '%s\n' project ;;
  *"instances describe subrouter-team"*) cat "$LIVE_INSTANCE_FIXTURE" ;;
  *"flock -x -w 300"*) printf '%s\n' LOCKED; cat >/dev/null ;;
  *"then echo fresh-prepared"*) printf '%s\n' fresh-prepared ;;
  *"subrouter-verify-fresh-vm"*) cat "$FRESH_TOPOLOGY_FIXTURE" ;;
esac
exit 0
`)

	createdAt := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	bootstrapEmittedAt := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	bootstrap := fmt.Sprintf(`{
  "schema":"subrouter.gcp.deploy-evidence/v1","evidence_type":"vm-provision","mode":"fresh-front-slots","success":true,"mutation_performed":true,
  "run":{"id":"bootstrap-1","project":"project","zone":"us-south1-a","instance":"subrouter-team"},
  "release":{"tag":"v1.2.3","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","source_revision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","tag_on_main":true,"attestation_verified":true,"immutable":true},
  "startup_metadata":{"schema":"subrouter.gcp.vm-release-metadata/v1","sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","verification_evidence_sha256":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"},
  "artifacts":{"SHA256SUMS":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","SOURCE_PROVENANCE.json":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff","deployment-contract.py":"abababababababababababababababababababababababababababababababab","install.sh":"1111111111111111111111111111111111111111111111111111111111111111","install-front-slots.sh":"2222222222222222222222222222222222222222222222222222222222222222","subrouter_1.2.3_linux_amd64":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
  "instance":{"created":true,"id":"1234567890123456789","creation_timestamp":"%s"},
  "topology":{"kind":"front-slots","state":"prepared","release_tag":"v1.2.3","initial_slot":"slot-a","authenticated":false,
    "legacy":{"service_active":false,"service_enabled":false,"socket_active":false,"socket_enabled":false},
    "slot":{"id":"slot-a","service_active":false,"service_enabled":false,"worker_checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","memory_max_bytes":201326592},
    "front":{"service_active":false,"service_enabled":false,"binary_checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","memory_max_bytes":134217728},
    "control":{"binary_checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
    "retained_release":{"binary_checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
  "evidence_emitted_at":"%s"
}`, createdAt, bootstrapEmittedAt)
	bootstrapPath := filepath.Join(t.TempDir(), "bootstrap.json")
	if err := os.WriteFile(bootstrapPath, []byte(bootstrap), 0o600); err != nil {
		t.Fatal(err)
	}
	activeTopology := `{
  "kind":"front-slots","state":"active","release_tag":"v1.2.3","initial_slot":"slot-a","authenticated":true,
  "legacy":{"service_active":false,"service_enabled":false,"socket_active":false,"socket_enabled":false},
  "slot":{"id":"slot-a","service_active":true,"service_enabled":true,"worker_checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","memory_max_bytes":201326592},
  "front":{"service_active":true,"service_enabled":true,"binary_checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","memory_max_bytes":134217728},
  "control":{"binary_checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
  "retained_release":{"binary_checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
`
	topologyPath := filepath.Join(t.TempDir(), "topology.json")
	if err := os.WriteFile(topologyPath, []byte(activeTopology), 0o600); err != nil {
		t.Fatal(err)
	}
	acceptancePath := filepath.Join(t.TempDir(), "acceptance.json")
	liveInstancePath := filepath.Join(t.TempDir(), "instance.json")
	if err := os.WriteFile(liveInstancePath, []byte(fmt.Sprintf(`{"name":"subrouter-team","zone":"https://www.googleapis.com/compute/v1/projects/project/zones/us-south1-a","id":"1234567890123456789","creationTimestamp":%q}`, createdAt)), 0o600); err != nil {
		t.Fatal(err)
	}
	srLog := filepath.Join(t.TempDir(), "sr.log")
	runPublish := func(bootstrapEvidence, acceptanceEvidence string) ([]byte, error, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, mustLookPath(t, "bash"), filepath.Join(repoRoot, "deploy", "gcp", "publish-subrouter.sh"), "v1.2.3")
		command.Env = append(upsertEnv(os.Environ(), "TMPDIR", publishTmp),
			"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"SR_BIN="+filepath.Join(fakeBin, "sr"),
			"SERVER_URL=https://sr.example.com",
			"FRESH_TOPOLOGY_FIXTURE="+topologyPath,
			"LIVE_INSTANCE_FIXTURE="+liveInstancePath,
			"SR_LOG="+srLog,
			"SUBROUTER_FRESH_VM_BOOTSTRAP_EVIDENCE="+bootstrapEvidence,
			"SUBROUTER_FRESH_VM_ACCEPTANCE_EVIDENCE="+acceptanceEvidence,
		)
		output, err := runDeployTestCommand(command)
		return output, err, ctx.Err()
	}
	if output, err, contextErr := runPublish(bootstrapPath, acceptancePath); err != nil {
		t.Fatalf("fresh publish failed: %v\n%s", err, output)
	} else if contextErr != nil {
		t.Fatal(contextErr)
	}
	validator := filepath.Join(repoRoot, "deploy", "gcp", "validate-deploy-evidence.py")
	validate := exec.Command(mustLookPath(t, "python3"), validator, "--expect", "fresh-vm-acceptance", acceptancePath)
	if output, err := validate.CombinedOutput(); err != nil {
		t.Fatalf("post-publish acceptance evidence failed validation: %v\n%s", err, output)
	}
	body, err := os.ReadFile(acceptancePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"state": "active"`, `"authenticated": true`, `"service_active": true`, `"service_enabled": true`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("acceptance evidence missing %s:\n%s", want, body)
		}
	}

	mismatchedBootstrap := strings.Replace(bootstrap, `"id":"1234567890123456789"`, `"id":"9999999999999999999"`, 1)
	mismatchedPath := filepath.Join(t.TempDir(), "bootstrap.json")
	if err := os.WriteFile(mismatchedPath, []byte(mismatchedBootstrap), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err, contextErr := runPublish(mismatchedPath, filepath.Join(t.TempDir(), "acceptance.json")); contextErr != nil {
		t.Fatalf("mismatched publish timed out: %v\n%s", contextErr, output)
	} else if err == nil {
		t.Fatalf("fresh publish accepted a mismatched GCE instance identity:\n%s", output)
	}
	logBody, err := os.ReadFile(srLog)
	if err != nil {
		t.Fatal(err)
	}
	if len(logBody) != 0 {
		t.Fatalf("identity mismatch reached server mutation:\n%s", logBody)
	}
}

func TestGoldenWrapperRejectsHostedURLAndInstanceMismatchBeforeMutation(t *testing.T) {
	requireDeployScriptTools(t, "bash", "jq", "python3")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	fakeBin := t.TempDir()
	externalLog := filepath.Join(t.TempDir(), "external.log")
	writeExecutableTestFile(t, filepath.Join(fakeBin, "uname"), `#!/bin/sh
case "$1" in
  -s) printf '%s\n' Darwin ;;
  -m) printf '%s\n' arm64 ;;
  *) exit 1 ;;
esac
`)
	for _, name := range []string{"gh", "gcloud", "go"} {
		writeExecutableTestFile(t, filepath.Join(fakeBin, name), "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >>\"$EXTERNAL_LOG\"\nexit 99\n")
	}
	wrapper := filepath.Join(repoRoot, "deploy", "gcp", "golden-local-mac-production-continuity.sh")
	run := func(instance, publicURL, hostedURL string) string {
		t.Helper()
		config := filepath.Join(t.TempDir(), "cloud.json")
		if err := os.WriteFile(config, []byte(fmt.Sprintf(`{"hostedUrl":%q}`, hostedURL)), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, mustLookPath(t, "bash"), wrapper, "--cloud-config", config, "--artifact-dir", t.TempDir())
		command.Env = append(os.Environ(),
			"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"EXTERNAL_LOG="+externalLog,
			"SUBROUTER_GCP_PROJECT=project",
			"SUBROUTER_GCP_ZONE=us-south1-a",
			"SUBROUTER_GCP_INSTANCE="+instance,
			"SUBROUTER_PUBLIC_BASE_URL="+publicURL,
		)
		output, err := runDeployTestCommand(command)
		if ctx.Err() != nil {
			t.Fatalf("golden wrapper did not terminate: %v\n%s", ctx.Err(), output)
		}
		if err == nil {
			t.Fatalf("golden wrapper accepted mismatched target:\n%s", output)
		}
		return string(output)
	}

	output := run("subrouter-staging", "https://staging.sr.cmux.com/", "https://sr.cmux.com")
	if !strings.Contains(output, "hostedUrl") || !strings.Contains(output, "SUBROUTER_PUBLIC_BASE_URL") {
		t.Fatalf("hosted/public mismatch was not localized:\n%s", output)
	}
	if body, err := os.ReadFile(externalLog); err == nil && len(body) > 0 {
		t.Fatalf("hosted/public mismatch reached an external operation:\n%s", body)
	}

	output = run("subrouter-team", "https://staging.sr.cmux.com", "https://staging.sr.cmux.com")
	if !strings.Contains(output, "subrouter-team") || !strings.Contains(output, "https://sr.cmux.com") {
		t.Fatalf("instance/public binding mismatch was not localized:\n%s", output)
	}
	if body, err := os.ReadFile(externalLog); err == nil && len(body) > 0 {
		t.Fatalf("instance/public mismatch reached an external operation:\n%s", body)
	}
}

func TestGoldenReleaseHelperCoherenceRejectsDrift(t *testing.T) {
	requireDeployScriptTools(t, "bash")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	verifier := filepath.Join(repoRoot, "deploy", "gcp", "verify-release-helper-coherence.sh")
	checkoutDir := t.TempDir()
	releaseDir := t.TempDir()
	helpers := []string{"deployment-contract.py", "install-front-slots.sh"}
	checkoutPaths := make([]string, 0, len(helpers))
	for _, name := range helpers {
		body := []byte("verified helper " + name + "\n")
		checkoutPath := filepath.Join(checkoutDir, name)
		if err := os.WriteFile(checkoutPath, body, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(releaseDir, name), body, 0o700); err != nil {
			t.Fatal(err)
		}
		checkoutPaths = append(checkoutPaths, checkoutPath)
	}
	run := func() ([]byte, error) {
		t.Helper()
		args := append([]string{verifier, releaseDir}, checkoutPaths...)
		return exec.Command(mustLookPath(t, "bash"), args...).CombinedOutput()
	}
	if output, err := run(); err != nil || len(output) != 0 {
		t.Fatalf("matching release helpers were rejected: %v\n%s", err, output)
	}
	if err := os.WriteFile(filepath.Join(releaseDir, helpers[0]), []byte("stale release helper\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	output, err := run()
	if err == nil {
		t.Fatalf("mismatched release helper was accepted:\n%s", output)
	}
	if !strings.Contains(string(output), helpers[0]) || !strings.Contains(string(output), "cut and pin a new release") {
		t.Fatalf("mismatch was not localized:\n%s", output)
	}
}

func TestReleaseMainVerificationStreamsLargeComparison(t *testing.T) {
	requireDeployScriptTools(t, "bash", "jq", "python3")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	fakeBin := t.TempDir()
	revision := strings.Repeat("a", 40)
	writeExecutableTestFile(t, filepath.Join(fakeBin, "gh"), `#!/bin/sh
case "$*" in
  *'/commits/'*) printf '%s\n' "$TEST_REVISION" ;;
  *'/compare/'*)
    python3 - "$TEST_REVISION" <<'PY'
import json
import sys
json.dump({
    "merge_base_commit": {"sha": sys.argv[1]},
    "status": "ahead",
    "padding": "x" * (2 * 1024 * 1024),
}, sys.stdout)
PY
    ;;
  *) exit 2 ;;
esac
`)
	helper := filepath.Join(repoRoot, "deploy", "gcp", "verify-release-on-main.sh")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(
		ctx,
		mustLookPath(t, "bash"),
		helper,
		"manaflow-ai/subrouter",
		"v0.1.51",
		revision,
	)
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TEST_REVISION="+revision,
	)
	output, err := runDeployTestCommand(command)
	if ctx.Err() != nil {
		t.Fatalf("release verification deadlocked on a large comparison: %v\n%s", ctx.Err(), output)
	}
	if err != nil || strings.TrimSpace(string(output)) != revision {
		t.Fatalf("release verification = %q, %v", output, err)
	}
}

func TestGoReleaseBinaryVerificationUsesFileBackedMetadata(t *testing.T) {
	requireDeployScriptTools(t, "bash", "dd", "tr")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	fakeBin := t.TempDir()
	revision := strings.Repeat("a", 40)
	binary := filepath.Join(t.TempDir(), "release-binary")
	if err := os.WriteFile(binary, []byte("fake release binary\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeExecutableTestFile(t, filepath.Join(fakeBin, "go"), `#!/bin/sh
printf 'path\texample.test/subrouter\n'
dd if=/dev/zero bs=1048576 count=2 2>/dev/null | tr '\000' x
printf '\nbuild\tvcs.revision=%s\nbuild\tvcs.modified=false\n' "$TEST_REVISION"
`)
	helper := filepath.Join(repoRoot, "deploy", "gcp", "verify-go-release-binary.sh")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, mustLookPath(t, "bash"), helper, binary, revision)
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TEST_REVISION="+revision,
	)
	output, err := runDeployTestCommand(command)
	if ctx.Err() != nil {
		t.Fatalf("Go release metadata verification deadlocked: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("Go release metadata verification failed: %v\n%s", err, output)
	}
}

func TestShellValueStreamSupportsNestedLargeJSONQueries(t *testing.T) {
	requireDeployScriptTools(t, "bash", "dd", "jq", "tr")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	helper := filepath.Join(repoRoot, "deploy", "gcp", "stream-shell-value.sh")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, mustLookPath(t, "bash"), "-c", `
set -euo pipefail
source "$1"
padding="$(dd if=/dev/zero bs=1048576 count=2 2>/dev/null | tr '\000' x)"
document="$(printf '{\"padding\":\"%s\",\"active\":{\"id\":\"generation-a\"}}' "$padding")"
active="$(jq -r '.active.id' < <(stream_shell_value "$document"))"
[[ "$active" == generation-a ]]
`, "test-shell-value-stream", helper)
	output, err := runDeployTestCommand(command)
	if ctx.Err() != nil {
		t.Fatalf("nested JSON query deadlocked: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("nested JSON query failed: %v\n%s", err, output)
	}
}

func TestReleaseMainVerificationReportsComparisonTransportFailure(t *testing.T) {
	requireDeployScriptTools(t, "bash", "jq")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	fakeBin := t.TempDir()
	revision := strings.Repeat("a", 40)
	writeExecutableTestFile(t, filepath.Join(fakeBin, "gh"), `#!/bin/sh
case "$*" in
  *'/commits/'*) printf '%s\n' "$TEST_REVISION" ;;
  *'/compare/'*) exit 23 ;;
  *) exit 2 ;;
esac
	`)
	helper := filepath.Join(repoRoot, "deploy", "gcp", "verify-release-on-main.sh")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(
		ctx,
		mustLookPath(t, "bash"),
		helper,
		"manaflow-ai/subrouter",
		"v0.1.51",
		revision,
	)
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TEST_REVISION="+revision,
	)
	output, err := runDeployTestCommand(command)
	if err == nil {
		t.Fatalf("comparison transport failure succeeded: %s", output)
	}
	if !strings.Contains(string(output), "failed to fetch comparison") ||
		strings.Contains(string(output), "is not on main") {
		t.Fatalf("comparison transport failure was misclassified: %v: %s", err, output)
	}
}

func TestDeploymentContractValidatesTargetAndManifest(t *testing.T) {
	requireDeployScriptTools(t, "python3")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	helper := filepath.Join(repoRoot, "deploy", "gcp", "deployment-contract.py")
	digest := strings.Repeat("a", 64)
	manifest := filepath.Join(t.TempDir(), "SHA256SUMS")
	writeManifest := func(body string) {
		t.Helper()
		if err := os.WriteFile(manifest, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run := func() ([]byte, error) {
		command := exec.Command(mustLookPath(t, "python3"), helper, "manifest-sha", manifest, "asset.bin")
		return command.CombinedOutput()
	}
	writeManifest(digest + "  asset.bin\n")
	if output, err := run(); err != nil || strings.TrimSpace(string(output)) != digest {
		t.Fatalf("valid manifest result = %q, %v", output, err)
	}
	writeManifest(digest + "  asset.bin\n" + digest + " *asset.bin\n")
	if output, err := run(); err == nil {
		t.Fatalf("duplicate manifest entry succeeded: %s", output)
	}
	writeManifest("not-a-digest  asset.bin\n")
	if output, err := run(); err == nil {
		t.Fatalf("malformed manifest digest succeeded: %s", output)
	}

	config := filepath.Join(t.TempDir(), "cloud.json")
	if err := os.WriteFile(config, []byte(`{"hostedUrl":"https://staging.sr.cmux.com/"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(mustLookPath(t, "python3"), helper,
		"validate-target", config, "subrouter-staging", "https://STAGING.sr.cmux.com:443")
	if output, err := command.CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != "https://staging.sr.cmux.com" {
		t.Fatalf("valid target result = %q, %v", output, err)
	}
	command = exec.Command(mustLookPath(t, "python3"), helper,
		"validate-target", config, "subrouter-team", "https://staging.sr.cmux.com")
	if output, err := command.CombinedOutput(); err == nil || !strings.Contains(string(output), "subrouter-team") {
		t.Fatalf("mismatched target result = %q, %v", output, err)
	}
}

func TestDeploymentContractValidatesInstanceAndPrivateInputs(t *testing.T) {
	requireDeployScriptTools(t, "python3")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	helper := filepath.Join(repoRoot, "deploy", "gcp", "deployment-contract.py")
	run := func(args ...string) ([]byte, error) {
		t.Helper()
		return exec.Command(mustLookPath(t, "python3"), append([]string{helper}, args...)...).CombinedOutput()
	}

	instance := filepath.Join(t.TempDir(), "instance.json")
	if err := os.WriteFile(instance, []byte(`{"name":"subrouter-team","zone":"https://www.googleapis.com/compute/v1/projects/p/zones/us-south1-a","id":"18446744073709551615","creationTimestamp":"2026-08-03T10:00:00-07:00"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := run("gce-instance-identity", instance, "subrouter-team", "us-south1-a")
	if err != nil || strings.TrimSpace(string(output)) != `{"creation_timestamp":"2026-08-03T17:00:00.000Z","id":"18446744073709551615"}` {
		t.Fatalf("instance identity = %q, %v", output, err)
	}
	if output, err := run("gce-instance-identity", instance, "subrouter-staging", "us-south1-a"); err == nil {
		t.Fatalf("mismatched identity succeeded: %s", output)
	}

	privateFile := filepath.Join(t.TempDir(), "private.json")
	if err := os.WriteFile(privateFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := run("validate-private-file", privateFile); err != nil || len(output) != 0 {
		t.Fatalf("private file result = %q, %v", output, err)
	}
	if err := os.Chmod(privateFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if output, err := run("validate-private-file", privateFile); err == nil {
		t.Fatalf("public private-file input succeeded: %s", output)
	}

	bootstrap := filepath.Join(t.TempDir(), "bootstrap.json")
	if err := os.WriteFile(bootstrap, []byte(`{"instance":{"id":"123","creation_timestamp":"2026-08-03T10:00:00.000Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	live := `{"creation_timestamp":"2026-08-03T10:00:00.000Z","id":"123"}`
	if output, err := run("validate-instance-binding", bootstrap, live, "--now", "2026-08-03T11:59:59.999Z"); err != nil || len(output) != 0 {
		t.Fatalf("instance binding result = %q, %v", output, err)
	}
	if output, err := run("validate-instance-binding", bootstrap, live, "--now", "2026-08-03T12:00:00.000Z"); err == nil {
		t.Fatalf("two-hour-old instance binding succeeded: %s", output)
	}
	futureLive := `{"creation_timestamp":"2026-08-03T10:05:00.001Z","id":"123"}`
	if err := os.WriteFile(bootstrap, []byte(`{"instance":{"id":"123","creation_timestamp":"2026-08-03T10:05:00.001Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := run("validate-instance-binding", bootstrap, futureLive, "--now", "2026-08-03T10:00:00.000Z"); err == nil {
		t.Fatalf("future-skewed instance binding succeeded: %s", output)
	}
}

func TestDeploymentContractAcceptsPreLifecycleLegacySupervisorStatus(t *testing.T) {
	requireDeployScriptTools(t, "python3")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	helper := filepath.Join(repoRoot, "deploy", "gcp", "deployment-contract.py")
	status := filepath.Join(t.TempDir(), "supervisor-status.json")
	run := func(body string) ([]byte, error) {
		t.Helper()
		if err := os.WriteFile(status, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return exec.Command(
			mustLookPath(t, "python3"), helper, "validate-legacy-supervisor-status", status,
		).CombinedOutput()
	}

	preLifecycle := `{"active":{"id":"generation-a"},"backends":[{"id":"generation-a","active":true,"connections":2}]}`
	if output, err := run(preLifecycle); err != nil || len(output) != 0 {
		t.Fatalf("pre-lifecycle legacy status result = %q, %v", output, err)
	}
	current := `{"accepting":true,"retiring":false,"active":{"id":"generation-a"},"backends":[{"id":"generation-a","active":true,"connections":0}]}`
	if output, err := run(current); err != nil || len(output) != 0 {
		t.Fatalf("current legacy status result = %q, %v", output, err)
	}
	for _, invalid := range []string{
		`{"retiring":false,"active":{"id":"generation-a"},"backends":[{"id":"generation-a","active":true,"connections":0}]}`,
		`{"accepting":true,"active":{"id":"generation-a"},"backends":[{"id":"generation-a","active":true,"connections":0}]}`,
		`{"accepting":false,"retiring":false,"active":{"id":"generation-a"},"backends":[{"id":"generation-a","active":true,"connections":0}]}`,
		`{"accepting":true,"retiring":true,"active":{"id":"generation-a"},"backends":[{"id":"generation-a","active":true,"connections":0}]}`,
		`{"active":{"id":"generation-a"},"backends":[{"id":"generation-b","active":true,"connections":0}]}`,
		`{"active":{"id":"generation-a"},"backends":[{"id":"generation-a","active":true,"connections":-1}]}`,
	} {
		if output, err := run(invalid); err == nil {
			t.Fatalf("invalid legacy status succeeded: %s\n%s", invalid, output)
		}
	}
}

func TestDeploymentContractValidatesAuthenticationAndURLMapTransitions(t *testing.T) {
	requireDeployScriptTools(t, "python3")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	helper := filepath.Join(repoRoot, "deploy", "gcp", "deployment-contract.py")
	run := func(args ...string) ([]byte, error) {
		t.Helper()
		return exec.Command(mustLookPath(t, "python3"), append([]string{helper}, args...)...).CombinedOutput()
	}

	defaults := filepath.Join(t.TempDir(), "defaults")
	if err := os.WriteFile(defaults, []byte("SUBROUTER_ADMIN_TOKEN=admin-secret\nSUBROUTER_ACCOUNT_IMPORT_TOKEN=import-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := run("validate-auth-defaults", defaults); err != nil || len(output) != 0 {
		t.Fatalf("authenticated defaults result = %q, %v", output, err)
	}
	if err := os.WriteFile(defaults, []byte("SUBROUTER_ADMIN_TOKEN=same\nSUBROUTER_ACCOUNT_IMPORT_TOKEN=same\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := run("validate-auth-defaults", defaults); err == nil || strings.Contains(string(output), "same") {
		t.Fatalf("equal secret result leaked or succeeded: %q, %v", output, err)
	}

	legacyURL := "https://www.googleapis.com/legacy"
	frontURL := "https://www.googleapis.com/front"
	before := filepath.Join(t.TempDir(), "before.yaml")
	candidate := filepath.Join(t.TempDir(), "candidate.yaml")
	if err := os.WriteFile(before, []byte("defaultService: "+legacyURL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := run("classify-url-map", before, legacyURL, frontURL); err != nil || strings.TrimSpace(string(output)) != "legacy" {
		t.Fatalf("URL-map classification = %q, %v", output, err)
	}
	if output, err := run("assert-url-map", before, legacyURL, "1", frontURL, "0"); err != nil || len(output) != 0 {
		t.Fatalf("URL-map assertion = %q, %v", output, err)
	}
	if output, err := run("rewrite-url-map", before, candidate, legacyURL, frontURL); err != nil || len(output) != 0 {
		t.Fatalf("URL-map rewrite = %q, %v", output, err)
	}
	if output, err := run("assert-url-map", candidate, legacyURL, "0", frontURL, "1"); err != nil || len(output) != 0 {
		t.Fatalf("rewritten URL-map assertion = %q, %v", output, err)
	}
	if output, err := run("rewrite-url-map", candidate, filepath.Join(t.TempDir(), "bad.yaml"), legacyURL, frontURL); err == nil {
		t.Fatalf("ambiguous URL-map rewrite succeeded: %s", output)
	}
}

func TestDeploymentContractValidatesGoldenTransitionProofs(t *testing.T) {
	requireDeployScriptTools(t, "python3")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	helper := filepath.Join(repoRoot, "deploy", "gcp", "deployment-contract.py")
	run := func(args ...string) ([]byte, error) {
		t.Helper()
		return exec.Command(mustLookPath(t, "python3"), append([]string{helper}, args...)...).CombinedOutput()
	}

	ack := filepath.Join(t.TempDir(), "ack.json")
	ackBody := `{"schema":"subrouter.gcp.slot-activation-ack/v1","challenge":"challenge","candidate_slot":"slot-b","candidate_generation":"generation-b","configured_original_clients":4,"original_streams_crossed":4,"direct_original_connections_verified":2,"local_egress_clients_verified":2,"all_original_streams_crossed_activation":true,"processes_stable":true,"sockets_stable":true,"local_egress_verified":true,"fresh_candidate_direct_connection":true,"fresh_candidate_connection_id":"connection","activated_at":"2026-08-03T10:00:29.999Z"}`
	if err := os.WriteFile(ack, []byte(ackBody), 0o600); err != nil {
		t.Fatal(err)
	}
	ackArgs := []string{"validate-activation-ack", ack, "challenge", "slot-b", "generation-b", "2026-08-03T10:00:00Z", "2026-08-03T10:00:00.500Z", "2026-08-03T10:00:29.999Z"}
	if output, err := run(ackArgs...); err != nil || len(output) != 0 {
		t.Fatalf("activation ack result = %q, %v", output, err)
	}
	lateAck := strings.Replace(ackBody, "10:00:29.999Z", "10:00:30.000Z", 1)
	if err := os.WriteFile(ack, []byte(lateAck), 0o600); err != nil {
		t.Fatal(err)
	}
	ackArgs[len(ackArgs)-1] = "2026-08-03T10:00:30.000Z"
	if output, err := run(ackArgs...); err == nil {
		t.Fatalf("late activation ack succeeded: %s", output)
	}

	proof := filepath.Join(t.TempDir(), "proof.json")
	proofBody := `{"schema":"subrouter.gcp.destination-proof/v1","challenge":"challenge","operation":"final-cutover","destination":"front","destination_generation":"generation-b","source":"legacy","source_generation":"generation-a","source_snapshot_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","expected_source_connections":2,"original_continuity_verified":true,"fresh_public_connection":true,"connection_id":"connection","session_id":"session-id","observed_at":"2026-08-03T10:00:29.999Z"}`
	if err := os.WriteFile(proof, []byte(proofBody), 0o600); err != nil {
		t.Fatal(err)
	}
	proofArgs := []string{"validate-destination-proof", proof, "challenge", "final-cutover", "front", "generation-b", "legacy", "generation-a", strings.Repeat("a", 64), "2", "2026-08-03T10:00:00Z", "2026-08-03T10:00:29.999Z"}
	if output, err := run(proofArgs...); err != nil || len(output) != 0 {
		t.Fatalf("destination proof result = %q, %v", output, err)
	}
	slowProof := strings.Replace(proofBody, "10:00:29.999Z", "10:01:59.999Z", 1)
	if err := os.WriteFile(proof, []byte(slowProof), 0o600); err != nil {
		t.Fatal(err)
	}
	proofArgs[len(proofArgs)-1] = "2026-08-03T10:01:59.999Z"
	if output, err := run(proofArgs...); err != nil || len(output) != 0 {
		t.Fatalf("sub-two-minute destination proof result = %q, %v", output, err)
	}
	lateProof := strings.Replace(proofBody, "10:00:29.999Z", "10:02:00.000Z", 1)
	if err := os.WriteFile(proof, []byte(lateProof), 0o600); err != nil {
		t.Fatal(err)
	}
	proofArgs[len(proofArgs)-1] = "2026-08-03T10:02:00.000Z"
	if output, err := run(proofArgs...); err == nil {
		t.Fatalf("two-minute destination proof boundary succeeded: %s", output)
	}
	proofArgs[len(proofArgs)-1] = "2026-08-03T10:00:29.999Z"
	if err := os.WriteFile(proof, []byte(strings.Replace(proofBody, `"connection_id":"connection"`, `"connection_id":""`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := run(proofArgs...); err == nil {
		t.Fatalf("empty destination connection succeeded: %s", output)
	}
	if err := os.WriteFile(proof, []byte(strings.Replace(proofBody, `"session_id":"session-id"`, `"session_id":""`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := run(proofArgs...); err == nil {
		t.Fatalf("empty destination session succeeded: %s", output)
	}
}

func TestDeploymentContractProbesSlotEndpoint(t *testing.T) {
	requireDeployScriptTools(t, "python3")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	helper := filepath.Join(repoRoot, "deploy", "gcp", "deployment-contract.py")
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	request := make(chan string, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			request <- "accept: " + acceptErr.Error()
			return
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
		reader := bufio.NewReaderSize(connection, 4096)
		var body strings.Builder
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				request <- "read: " + readErr.Error()
				return
			}
			body.WriteString(line)
			if line == "\r\n" {
				break
			}
		}
		request <- body.String()
		_, _ = connection.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"))
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, mustLookPath(t, "python3"), helper, "probe-slot-endpoint", fmt.Sprint(port), "/_subrouter/ready")
	output, err := runDeployTestCommand(command)
	if ctx.Err() != nil {
		t.Fatalf("slot probe timed out: %v\n%s", ctx.Err(), output)
	}
	if err != nil || len(output) != 0 {
		t.Fatalf("slot probe result = %q, %v", output, err)
	}
	got := <-request
	for _, want := range []string{
		fmt.Sprintf("PROXY TCP4 127.0.0.1 127.0.0.1 12345 %d\r\n", port),
		"GET /_subrouter/ready HTTP/1.1\r\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("slot probe request missing %q: %q", want, got)
		}
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
  "mode":"activation",
  "intent":"rehearsal",
  "success":true,
  "run":{"id":"run-1","project":"project","zone":"zone","instance":"instance"},
  "release":{"tag":"v1.2.3","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","source_revision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","tag_on_main":true,"attestation_verified":true,"immutable":true},
  "slots":{"before":"slot-a","candidate":"slot-b","final":"slot-b","old_generation":"old-generation","candidate_generation":"new-generation"},
  "checksums":{"installed_before":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","candidate_installed":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","installed_after":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
  "timestamps":{"upgrade_requested_at":"2026-08-02T10:00:00Z","provisional_switch_at":"2026-08-02T10:00:00.500Z","activated_at":"2026-08-02T10:00:01Z","golden_ack_received_at":"2026-08-02T10:00:01.500Z","evidence_emitted_at":"2026-08-02T10:00:03Z"},
  "golden_ack":{"sha256":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","challenge":"0123456789abcdef0123456789abcdef","fresh_candidate_connection_id":"connection-hash","configured_original_clients":4,"original_streams_crossed":4,"direct_original_connections_verified":2,"local_egress_clients_verified":2,"all_original_streams_crossed_activation":true,"processes_stable":true,"sockets_stable":true,"local_egress_verified":true,"fresh_candidate_direct_connection":true,"activated_at":"2026-08-02T10:00:01Z","received_at":"2026-08-02T10:00:01.500Z"},
  "front":{"active_before":{"id":"slot-a","network":"tcp","address":"127.0.0.1:31417"},"active_after":{"id":"slot-b","network":"tcp","address":"127.0.0.1:31418"},"active_final":{"id":"slot-b","network":"tcp","address":"127.0.0.1:31418"}},
  "old_slot":{"before":{"accepting":true,"retiring":false,"front_active":true,"active_generation":"old-generation","active_connections":4,"inactive_connections":0,"service_active":true},"after":{"accepting":true,"retiring":false,"front_active":false,"active_generation":"old-generation","active_connections":4,"inactive_connections":0,"service_active":true}},
  "metrics":{"old_slot":{"nrestarts":{"before":0,"after":0},"oom_kill":{"before":0,"after":0},"run_scoped_peak_rss_bytes":150000000,"memory_max_bytes":201326592},"candidate_slot":{"nrestarts":{"before":0,"after":0},"oom_kill":{"before":0,"after":0},"run_scoped_peak_rss_bytes":180000000,"memory_max_bytes":201326592},"front":{"nrestarts":{"before":0,"after":0},"oom_kill":{"before":0,"after":0},"run_scoped_peak_rss_bytes":100000000,"memory_max_bytes":134217728}},
  "continuity":{"configured_original_clients":4,"expected_original_slot_connections":2,"pinned_original_connections_at_switch":2,"expected_candidate_connections_for_rollback":1,"candidate_connections_before":0,"candidate_connections_after_ack":1,"candidate_connection_count_delta":1,"all_expected_slot_connections_pinned":true,"transports":[],"resumed_contexts":0,"resume_nonce_verified":false,"ci_evidence_role":"supplemental","golden_gate_role":"external-required"},
  "rollback":{"performed":false,"requested_at":null,"activated_at":null,"from":null,"to":null},
  "retirement":{"target":"slot-a","requested_at":null,"state":"not-requested","evidence_file_required":true}
}`
	if output, err := run("slot-activation", activation); err != nil {
		t.Fatalf("valid activation evidence was rejected: %v\n%s", err, output)
	}
	activationAt := func(activatedAt, emittedAt string) string {
		t.Helper()
		fixture := strings.ReplaceAll(activation, `"activated_at":"2026-08-02T10:00:01Z"`, `"activated_at":"`+activatedAt+`"`)
		fixture = strings.ReplaceAll(fixture, `"golden_ack_received_at":"2026-08-02T10:00:01.500Z"`, `"golden_ack_received_at":"`+activatedAt+`"`)
		fixture = strings.ReplaceAll(fixture, `"received_at":"2026-08-02T10:00:01.500Z"`, `"received_at":"`+activatedAt+`"`)
		return strings.Replace(fixture, `"evidence_emitted_at":"2026-08-02T10:00:03Z"`, `"evidence_emitted_at":"`+emittedAt+`"`, 1)
	}
	if output, err := run("slot-activation", activationAt("2026-08-02T10:00:29.999Z", "2026-08-02T10:00:30.500Z")); err != nil {
		t.Fatalf("29.999-second activation was rejected: %v\n%s", err, output)
	}
	for _, elapsed := range []string{"2026-08-02T10:00:30.000Z", "2026-08-02T10:00:30.001Z"} {
		if output, err := run("slot-activation", activationAt(elapsed, "2026-08-02T10:00:31.000Z")); err == nil {
			t.Fatalf("activation boundary %s was accepted:\n%s", elapsed, output)
		}
	}

	wrongFinal := strings.Replace(activation, `"final":"slot-b"`, `"final":"slot-a"`, 1)
	if output, err := run("slot-activation", wrongFinal); err == nil {
		t.Fatalf("candidate-inactive deploy evidence was accepted:\n%s", output)
	}
	missingOriginal := strings.Replace(activation, `"pinned_original_connections_at_switch":2`, `"pinned_original_connections_at_switch":1`, 1)
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
  "transition_evidence_type":"slot-activation",
  "transition_evidence_sha256":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
  "run":{"id":"run-1","project":"project","zone":"zone","instance":"instance"},
  "slots":{"retired":"slot-a","active":"slot-b","retired_generation":"old-generation"},
  "front":{"active":{"id":"slot-b","network":"tcp","address":"127.0.0.1:31418"},"retired_connections_after":0},
  "retirement":{"requested_at":"2026-08-02T10:00:02Z","last_connection_closed_at":"2026-08-02T10:01:00Z","absent_at":"2026-08-02T10:01:01Z","absence_latency_ms":1000,"service_active_after":false,"control_socket_present_after":false,"enabled_after":false,"service_result":"success"},
  "metrics":{"old_slot":{"nrestarts":{"before":0,"after":0},"oom_kill":{"before":0,"after":0},"run_scoped_peak_rss_bytes":150000000,"memory_max_bytes":201326592}},
  "evidence_emitted_at":"2026-08-02T10:01:02Z"
}`
	if output, err := run("slot-retirement", retirement); err != nil {
		t.Fatalf("valid retirement evidence was rejected: %v\n%s", err, output)
	}
	lateAbsence := strings.Replace(retirement, `"absence_latency_ms":1000`, `"absence_latency_ms":30001`, 1)
	if output, err := run("slot-retirement", lateAbsence); err == nil {
		t.Fatalf("late old-slot absence evidence was accepted:\n%s", output)
	}

	alreadyNormalized := `{
  "schema":"subrouter.gcp.deploy-evidence/v1","evidence_type":"staging-predecessor-normalization","mode":"staging-only","success":true,
  "normalization_performed":false,"normalization_result":"already-normalized",
  "run":{"id":"run-1","project":"project","zone":"zone","instance":"subrouter-staging"},
  "predecessor":{"tag":"v0.1.51","sha256":"99fcd10d912184c160370eb228b382795101f2b5b2467244f995aa2d10b0c323","source_revision":"5eacb5411c0bd4a24f4e422d6366fa7bfd1843c8","tag_on_main":true,"hard_pin_verified":true,"sha256sums_match":true,"embedded_revision_verified":true,"live_worker_checksum_match":true},
  "checksums":{"before":"99fcd10d912184c160370eb228b382795101f2b5b2467244f995aa2d10b0c323","after":"99fcd10d912184c160370eb228b382795101f2b5b2467244f995aa2d10b0c323"},
  "generations":{"before":"generation-1","after":"generation-1"},
  "connections":{"active_generation_before":2,"active_generation_after":2,"inactive_after":0},
  "public":{"health":true,"ready":true},
  "timestamps":{"verified_at":"2026-08-02T10:00:00Z","evidence_emitted_at":"2026-08-02T10:00:01Z"},
  "metrics":{"nrestarts":{"before":0,"after":0},"oom_kill":{"before":0,"after":0}}
}`
	if output, err := run("staging-predecessor-normalization", alreadyNormalized); err != nil {
		t.Fatalf("already-normalized staging evidence was rejected: %v\n%s", err, output)
	}
	falseMutation := strings.Replace(alreadyNormalized, `"normalization_performed":false`, `"normalization_performed":true`, 1)
	if output, err := run("staging-predecessor-normalization", falseMutation); err == nil {
		t.Fatalf("same-generation performed normalization was accepted:\n%s", output)
	}

	vmProvision := `{
  "schema":"subrouter.gcp.deploy-evidence/v1","evidence_type":"vm-provision","mode":"fresh-front-slots","success":true,"mutation_performed":true,
  "run":{"id":"run-1","project":"project","zone":"zone","instance":"instance"},
  "release":{"tag":"v1.2.3","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","source_revision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","tag_on_main":true,"attestation_verified":true,"immutable":true},
  "startup_metadata":{"schema":"subrouter.gcp.vm-release-metadata/v1","sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","verification_evidence_sha256":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"},
  "artifacts":{"SHA256SUMS":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","SOURCE_PROVENANCE.json":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff","deployment-contract.py":"abababababababababababababababababababababababababababababababab","install.sh":"1111111111111111111111111111111111111111111111111111111111111111","install-front-slots.sh":"2222222222222222222222222222222222222222222222222222222222222222","subrouter_1.2.3_linux_amd64":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
  "instance":{"created":true,"id":"1234567890123456789","creation_timestamp":"2026-08-03T09:55:00Z"},
  "topology":{"kind":"front-slots","state":"prepared","release_tag":"v1.2.3","initial_slot":"slot-a","authenticated":false,
    "legacy":{"service_active":false,"service_enabled":false,"socket_active":false,"socket_enabled":false},
    "slot":{"id":"slot-a","service_active":false,"service_enabled":false,"worker_checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","memory_max_bytes":201326592},
    "front":{"service_active":false,"service_enabled":false,"binary_checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","memory_max_bytes":134217728},
    "control":{"binary_checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
    "retained_release":{"binary_checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
  "evidence_emitted_at":"2026-08-03T10:00:02Z"
}`
	if output, err := run("vm-provision", vmProvision); err != nil {
		t.Fatalf("valid fresh VM evidence was rejected: %v\n%s", err, output)
	}
	legacyStarted := strings.Replace(vmProvision, `"service_active":false`, `"service_active":true`, 1)
	if output, err := run("vm-provision", legacyStarted); err == nil {
		t.Fatalf("fresh VM evidence with active legacy service was accepted:\n%s", output)
	}

	freshVMAcceptance := `{
  "schema":"subrouter.gcp.deploy-evidence/v1","evidence_type":"fresh-vm-acceptance","mode":"post-publish","success":true,
  "run":{"id":"publish-1","project":"project","zone":"zone","instance":"instance"},
  "release":{"tag":"v1.2.3","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","source_revision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","tag_on_main":true,"attestation_verified":true,"immutable":true},
  "bootstrap_evidence":{"sha256":"3333333333333333333333333333333333333333333333333333333333333333","evidence_type":"vm-provision","topology_state":"prepared","evidence_emitted_at":"2026-08-03T10:00:00Z"},
  "instance":{"created":true,"id":"1234567890123456789","creation_timestamp":"2026-08-03T09:55:00Z","bootstrap_run_id":"bootstrap-1"},
  "startup_metadata":{"schema":"subrouter.gcp.vm-release-metadata/v1","sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","verification_evidence_sha256":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"},
  "artifacts":{"SHA256SUMS":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","SOURCE_PROVENANCE.json":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff","deployment-contract.py":"abababababababababababababababababababababababababababababababab","install.sh":"1111111111111111111111111111111111111111111111111111111111111111","install-front-slots.sh":"2222222222222222222222222222222222222222222222222222222222222222","subrouter_1.2.3_linux_amd64":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
  "public":{"base_url":"https://sr.example.com","health":true,"ready":true},
  "topology":{"kind":"front-slots","state":"active","release_tag":"v1.2.3","initial_slot":"slot-a","authenticated":true,
    "legacy":{"service_active":false,"service_enabled":false,"socket_active":false,"socket_enabled":false},
    "slot":{"id":"slot-a","service_active":true,"service_enabled":true,"worker_checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","memory_max_bytes":201326592},
    "front":{"service_active":true,"service_enabled":true,"binary_checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","memory_max_bytes":134217728},
    "control":{"binary_checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
    "retained_release":{"binary_checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
  "evidence_emitted_at":"2026-08-03T10:00:02Z"
}`
	if output, err := run("fresh-vm-acceptance", freshVMAcceptance); err != nil {
		t.Fatalf("valid post-publish VM acceptance was rejected: %v\n%s", err, output)
	}
	preparedAcceptance := strings.Replace(freshVMAcceptance, `"state":"active"`, `"state":"prepared"`, 1)
	if output, err := run("fresh-vm-acceptance", preparedAcceptance); err == nil {
		t.Fatalf("prepared topology passed final VM acceptance:\n%s", output)
	}
	unauthenticatedAcceptance := strings.Replace(freshVMAcceptance, `"authenticated":true`, `"authenticated":false`, 1)
	if output, err := run("fresh-vm-acceptance", unauthenticatedAcceptance); err == nil {
		t.Fatalf("unauthenticated topology passed final VM acceptance:\n%s", output)
	}
	disabledFrontAcceptance := strings.Replace(freshVMAcceptance, `"front":{"service_active":true,"service_enabled":true`, `"front":{"service_active":true,"service_enabled":false`, 1)
	if output, err := run("fresh-vm-acceptance", disabledFrontAcceptance); err == nil {
		t.Fatalf("disabled front passed final VM acceptance:\n%s", output)
	}
	reusedInstanceAcceptance := strings.Replace(freshVMAcceptance, `"created":true`, `"created":false`, 1)
	if output, err := run("fresh-vm-acceptance", reusedInstanceAcceptance); err == nil {
		t.Fatalf("reused VM passed fresh-instance acceptance:\n%s", output)
	}
	missingIdentityAcceptance := strings.Replace(freshVMAcceptance, `,"id":"1234567890123456789","creation_timestamp":"2026-08-03T09:55:00Z"`, ``, 1)
	if output, err := run("fresh-vm-acceptance", missingIdentityAcceptance); err == nil {
		t.Fatalf("identity-free VM passed fresh-instance acceptance:\n%s", output)
	}
	staleAcceptance := strings.Replace(freshVMAcceptance, `"creation_timestamp":"2026-08-03T09:55:00Z"`, `"creation_timestamp":"2026-08-02T09:55:00Z"`, 1)
	if output, err := run("fresh-vm-acceptance", staleAcceptance); err == nil {
		t.Fatalf("stale VM bootstrap passed fresh-instance acceptance:\n%s", output)
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
			"SUBROUTER_DEPLOYMENT_CONTRACT="+filepath.Join(repoRoot, "deploy", "gcp", "deployment-contract.py"),
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

func TestFrontSlotInstallerSafelyBeginsDormantStaleMigrationReconciliation(t *testing.T) {
	requireDeployScriptTools(t, "bash", "curl", "jq", "python3", "sha256sum")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	stateDir, err := os.MkdirTemp("/tmp", "subrouter-front-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	fakeBin := filepath.Join(stateDir, "bin")
	serviceState := filepath.Join(stateDir, "services")
	serviceHome := filepath.Join(stateDir, "home")
	releaseRoot := filepath.Join(stateDir, "releases")
	slotRoot := filepath.Join(stateDir, "slots")
	frontRoot := filepath.Join(stateDir, "front")
	controlRoot := filepath.Join(stateDir, "control")
	unitRoot := filepath.Join(stateDir, "units")
	logDir := filepath.Join(stateDir, "log")
	frontEnv := filepath.Join(stateDir, "subrouter-front")
	frontSocket := filepath.Join(stateDir, "front.sock")
	systemctlLog := filepath.Join(stateDir, "systemctl.log")
	for _, directory := range []string{fakeBin, serviceState, serviceHome, releaseRoot, slotRoot, frontRoot, controlRoot, unitRoot, logDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	serviceUser := os.Getenv("USER")
	if serviceUser == "" {
		serviceUser = "nobody"
	}
	serviceGroupOutput, err := exec.Command("id", "-gn").Output()
	if err != nil {
		t.Fatal(err)
	}
	serviceGroup := strings.TrimSpace(string(serviceGroupOutput))

	writeExecutableTestFile(t, filepath.Join(fakeBin, "id"), "#!/bin/sh\nprintf '0\\n'\n")
	writeExecutableTestFile(t, filepath.Join(fakeBin, "curl"), `#!/bin/sh
case "$*" in
  *"/_subrouter/front-status"*)
    printf '%s\n' "$FRONT_STATUS"
    if [ "${FRONT_CURL_EXIT:-0}" != 0 ]; then
      exit "$FRONT_CURL_EXIT"
    fi
    case "${FRONT_HTTP_ERROR:-0}:$1" in
      1:*f*) exit 22 ;;
    esac
    ;;
esac
exit 0
`)
	writeExecutableTestFile(t, filepath.Join(fakeBin, "python3"), "#!/bin/sh\nexit 0\n")
	writeExecutableTestFile(t, filepath.Join(fakeBin, "systemctl"), `#!/bin/sh
printf '%s\n' "$*" >>"$SYSTEMCTL_LOG"
case "$1" in
  show)
    case "$*" in
      *"-p User --value") printf '%s\n' "$SERVICE_USER" ;;
      *"-p Group --value") printf '%s\n' "$SERVICE_GROUP" ;;
      *"-p Environment --value") printf 'HOME=%s\n' "$SERVICE_HOME" ;;
    esac
    ;;
  is-active)
    unit="${3:-}"
    if [ "$unit" = "subrouter.service" ]; then
      exit 0
    fi
    test -e "$SERVICE_STATE/$unit.active"
    exit $?
    ;;
  is-enabled)
    test -e "$SERVICE_STATE/${3:-}.enabled"
    exit $?
    ;;
  disable)
    if [ "${2:-}" = "--now" ]; then
      shift 2
      for unit in "$@"; do
        rm -f "$SERVICE_STATE/$unit.active" "$SERVICE_STATE/$unit.enabled"
      done
    else
      shift
      for unit in "$@"; do
        rm -f "$SERVICE_STATE/$unit.enabled"
      done
    fi
    ;;
  enable)
    if [ "${2:-}" = "--now" ]; then
      shift 2
      for unit in "$@"; do
        : >"$SERVICE_STATE/$unit.active"
        : >"$SERVICE_STATE/$unit.enabled"
      done
    fi
    ;;
esac
exit 0
`)

	for _, unit := range []string{"subrouter-front.service", "subrouter-slot@slot-a.service"} {
		if err := os.WriteFile(filepath.Join(serviceState, unit+".active"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(serviceState, unit+".enabled"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("unix", frontSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	writeRelease := func(tag, marker string) string {
		directory := filepath.Join(releaseRoot, tag)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, "subrouter")
		writeExecutableTestFile(t, path, "#!/bin/sh\nprintf '"+marker+"\\n'\n")
		return path
	}
	controlRelease := writeRelease("v9.9.9", "unsupported-control")
	workerRelease := writeRelease("v9.9.8", "worker")
	for _, path := range []string{
		filepath.Join(frontRoot, "subrouter"),
		filepath.Join(controlRoot, "subrouter"),
		filepath.Join(slotRoot, "slot-a", "worker"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		writeExecutableTestFile(t, path, "#!/bin/sh\nprintf 'stale\\n'\n")
	}

	commandEnv := append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SYSTEMCTL_LOG="+systemctlLog,
		"SERVICE_STATE="+serviceState,
		"SERVICE_USER="+serviceUser,
		"SERVICE_GROUP="+serviceGroup,
		"SERVICE_HOME="+serviceHome,
		"SUBROUTER_RELEASE_ROOT="+releaseRoot,
		"SUBROUTER_SLOT_ROOT="+slotRoot,
		"SUBROUTER_FRONT_ROOT="+frontRoot,
		"SUBROUTER_CONTROL_ROOT="+controlRoot,
		"SUBROUTER_STATE_DIR="+stateDir,
		"SUBROUTER_FRONT_CONTROL_SOCKET="+frontSocket,
		"SUBROUTER_FRONT_ENV="+frontEnv,
		"SUBROUTER_SLOT_ENV_DIR="+filepath.Join(stateDir, "slot-env"),
		"SUBROUTER_LOG_DIR="+logDir,
		"SUBROUTER_LEGACY_SERVICE=subrouter.service",
		"SUBROUTER_SLOT_UNIT="+filepath.Join(unitRoot, "subrouter-slot@.service"),
		"SUBROUTER_FRONT_UNIT="+filepath.Join(unitRoot, "subrouter-front.service"),
		"SUBROUTER_VERIFY_UNIT="+filepath.Join(unitRoot, "subrouter-verify.service"),
		"SUBROUTER_VERIFY_DROPIN_DIR="+filepath.Join(unitRoot, "subrouter-verify.service.d"),
		"SUBROUTER_DEPLOYMENT_CONTRACT="+filepath.Join(repoRoot, "deploy", "gcp", "deployment-contract.py"),
	)
	run := func(httpError, curlExit, frontStatus string) ([]byte, error, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		command := exec.CommandContext(ctx, mustLookPath(t, "bash"),
			filepath.Join(repoRoot, "deploy", "gcp", "install-front-slots.sh"),
			"ensure-migration-topology", "v9.9.9", "v9.9.8", "slot-a")
		command.Env = append(commandEnv,
			"FRONT_HTTP_ERROR="+httpError,
			"FRONT_CURL_EXIT="+curlExit,
			"FRONT_STATUS="+frontStatus,
		)
		output, runErr := runDeployTestCommand(command)
		contextErr := ctx.Err()
		cancel()
		return output, runErr, contextErr
	}
	validFrontStatus := `{"active":{"id":"slot-a"},"backends":[{"id":"slot-a","connections":0}]}`
	missingActiveBackendStatus := `{"active":{"id":"slot-a"},"backends":[]}`
	output, runErr, contextErr := run("0", "0", missingActiveBackendStatus)
	if runErr == nil || contextErr != nil {
		t.Fatalf("incomplete front status was not rejected promptly: err=%v context=%v\n%s", runErr, contextErr, output)
	}
	if !strings.Contains(string(output), "front returned invalid topology status") {
		t.Fatalf("incomplete front status was treated as zero connections:\n%s", output)
	}
	logBody, err := os.ReadFile(systemctlLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logBody), "disable --now") {
		t.Fatalf("incomplete front status allowed a service stop:\n%s", logBody)
	}
	if err := os.WriteFile(systemctlLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	frontActiveMarker := filepath.Join(serviceState, "subrouter-front.service.active")
	if err := os.Remove(frontActiveMarker); err != nil {
		t.Fatal(err)
	}
	output, runErr, contextErr = run("1", "0", validFrontStatus)
	if runErr == nil || contextErr != nil {
		t.Fatalf("unmanaged erroring front was not rejected promptly: err=%v context=%v\n%s", runErr, contextErr, output)
	}
	if !strings.Contains(string(output), "front control socket is live outside subrouter-front.service") {
		t.Fatalf("unmanaged erroring front was mistaken for a stale socket:\n%s", output)
	}
	logBody, err = os.ReadFile(systemctlLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logBody), "disable --now") {
		t.Fatalf("unmanaged erroring front was stopped:\n%s", logBody)
	}
	output, runErr, contextErr = run("0", "52", validFrontStatus)
	if runErr == nil || contextErr != nil {
		t.Fatalf("unmanaged transport-error front was not rejected promptly: err=%v context=%v\n%s", runErr, contextErr, output)
	}
	if !strings.Contains(string(output), "front control socket is live outside subrouter-front.service") {
		t.Fatalf("unmanaged transport-error front was mistaken for a stale socket:\n%s", output)
	}
	logBody, err = os.ReadFile(systemctlLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logBody), "disable --now") {
		t.Fatalf("unmanaged transport-error front was stopped:\n%s", logBody)
	}
	if err := os.WriteFile(frontActiveMarker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for path, target := range map[string]string{
		filepath.Join(frontRoot, "subrouter"):       controlRelease,
		filepath.Join(controlRoot, "subrouter"):     controlRelease,
		filepath.Join(slotRoot, "slot-a", "worker"): workerRelease,
	} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(systemctlLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	output, runErr, contextErr = run("0", "0", validFrontStatus)
	if runErr == nil || contextErr != nil {
		t.Fatalf("matching on-disk topology did not reach bounded reinstall validation: err=%v context=%v\n%s", runErr, contextErr, output)
	}
	if !strings.Contains(string(output), "v9.9.9 does not support subrouter front") {
		t.Fatalf("stale topology failed before attempting the verified reinstall:\n%s", output)
	}
	logBody, err = os.ReadFile(systemctlLog)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logBody)
	frontStopped := strings.Index(logText, "disable --now subrouter-front.service")
	slotStopped := strings.Index(logText, "disable --now subrouter-slot@slot-a.service subrouter-slot@slot-b.service")
	if frontStopped < 0 || slotStopped <= frontStopped {
		t.Fatalf("dormant topology reconciliation order is unsafe:\n%s", logText)
	}
	if strings.Contains(logText, "enable --now") {
		t.Fatalf("rejected replacement started a serving unit:\n%s", logText)
	}
	if strings.Contains(logText, "disable --now subrouter.service") || strings.Contains(logText, "restart subrouter.service") {
		t.Fatalf("legacy serving process was interrupted:\n%s", logText)
	}
}

func TestFreshFrontTopologyStartsOnlyAfterDistinctTokensExist(t *testing.T) {
	requireDeployScriptTools(t, "bash", "curl", "jq", "python3", "sha256sum")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	fakeBin := t.TempDir()
	realPython := mustLookPath(t, "python3")
	writeExecutableTestFile(t, filepath.Join(fakeBin, "id"), "#!/bin/sh\nprintf '0\\n'\n")
	writeExecutableTestFile(t, filepath.Join(fakeBin, "curl"), "#!/bin/sh\nexit 0\n")
	writeExecutableTestFile(t, filepath.Join(fakeBin, "python3"), `#!/bin/sh
if [ "${2:-}" = "validate-auth-defaults" ]; then
  exec "$REAL_PYTHON" "$@"
fi
exit 0
`)
	writeExecutableTestFile(t, filepath.Join(fakeBin, "systemctl"), `#!/bin/sh
printf '%s\n' "$*" >>"$SYSTEMCTL_LOG"
case "$*" in
  "is-active --quiet subrouter.service"|"is-active --quiet subrouter.socket") exit 3 ;;
esac
exit 0
`)

	stateDir := t.TempDir()
	marker := filepath.Join(stateDir, "front-topology-prepared")
	defaults := filepath.Join(t.TempDir(), "subrouter")
	logPath := filepath.Join(t.TempDir(), "systemctl.log")
	if err := os.WriteFile(marker, []byte("slot-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(defaults, []byte("SUBROUTER_ADMIN_TOKEN=admin-secret\nSUBROUTER_ACCOUNT_IMPORT_TOKEN=import-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func() ([]byte, error, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, mustLookPath(t, "bash"), filepath.Join(repoRoot, "deploy", "gcp", "install-front-slots.sh"), "activate-fresh-topology", "slot-a")
		command.Env = append(os.Environ(),
			"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"REAL_PYTHON="+realPython,
			"SYSTEMCTL_LOG="+logPath,
			"SUBROUTER_STATE_DIR="+stateDir,
			"SUBROUTER_FRESH_TOPOLOGY_MARKER="+marker,
			"SUBROUTER_DEFAULTS_FILE="+defaults,
			"SUBROUTER_DEPLOYMENT_CONTRACT="+filepath.Join(repoRoot, "deploy", "gcp", "deployment-contract.py"),
		)
		output, err := runDeployTestCommand(command)
		return output, err, ctx.Err() != nil
	}
	if output, err, timedOut := run(); err != nil || timedOut {
		t.Fatalf("activate fresh topology: %v\n%s", err, output)
	}
	if _, err := os.Stat(marker + ".active"); err != nil {
		t.Fatalf("active marker: %v", err)
	}
	logBody, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logBody)
	legacyDisabled := strings.Index(logText, "disable --now subrouter.service")
	slotEnabled := strings.Index(logText, "enable --now subrouter-slot@slot-a.service")
	frontEnabled := strings.Index(logText, "enable --now subrouter-front.service")
	if legacyDisabled < 0 || slotEnabled <= legacyDisabled || frontEnabled <= slotEnabled {
		t.Fatalf("fresh activation order is unsafe:\n%s", logText)
	}
	if strings.Contains(logText, "enable --now subrouter.service") || strings.Contains(logText, "restart subrouter.service") {
		t.Fatalf("fresh activation started the legacy topology:\n%s", logText)
	}

	if err := os.Rename(marker+".active", marker); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(defaults, []byte("SUBROUTER_ADMIN_TOKEN=admin-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err, timedOut := run(); timedOut {
		t.Fatalf("rejected activation did not return after rollback cleanup:\n%s", output)
	} else if err == nil {
		t.Fatalf("fresh topology activated without an import token:\n%s", output)
	}
	logBody, err = os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logBody), "enable --now") {
		t.Fatalf("unauthenticated activation enabled a serving unit:\n%s", logBody)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("retry marker was not preserved after rejected activation: %v", err)
	}
}

func TestReleaseInstallerResolverBindsAttestedOverride(t *testing.T) {
	requireDeployScriptTools(t, "bash", "jq", "sha256sum")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	resolver := filepath.Join(repoRoot, "deploy", "gcp", "resolve-release-installer.sh")
	defaultInstaller := filepath.Join(t.TempDir(), "install-front-slots.sh")
	overrideInstaller := filepath.Join(t.TempDir(), "install-front-slots.sh")
	if err := os.WriteFile(defaultInstaller, []byte("default installer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	overrideBody := []byte("attested release installer\n")
	if err := os.WriteFile(overrideInstaller, overrideBody, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(overrideBody)
	defaultContract := filepath.Join(t.TempDir(), "deployment-contract.py")
	overrideContract := filepath.Join(t.TempDir(), "deployment-contract.py")
	if err := os.WriteFile(defaultContract, []byte("default contract\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	contractBody := []byte("attested deployment contract\n")
	if err := os.WriteFile(overrideContract, contractBody, 0o600); err != nil {
		t.Fatal(err)
	}
	contractDigest := sha256.Sum256(contractBody)
	verification := filepath.Join(t.TempDir(), "release-verification.json")
	verificationBody := fmt.Sprintf(`{
		"schema":"subrouter.release-verification/v1",
		"release_published":true,
		"release_immutable":true,
		"asset_digest_verified":true,
		"strict_build_attestation_verified":true,
		"assets":{"install-front-slots.sh":"%x","deployment-contract.py":"%x"}
	}`, digest, contractDigest)
	if err := os.WriteFile(verification, []byte(verificationBody), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(installer, evidence string) ([]byte, error) {
		command := exec.Command(mustLookPath(t, "bash"), resolver, defaultInstaller)
		command.Env = append(
			os.Environ(),
			"SUBROUTER_INSTALL_FRONT_SLOTS="+installer,
			"SUBROUTER_RELEASE_VERIFICATION_JSON="+evidence,
		)
		return command.CombinedOutput()
	}
	if output, err := run("", ""); err != nil || strings.TrimSpace(string(output)) != defaultInstaller {
		t.Fatalf("default installer = %q, %v", output, err)
	}
	if output, err := run(overrideInstaller, verification); err != nil || strings.TrimSpace(string(output)) != overrideInstaller {
		t.Fatalf("verified installer = %q, %v", output, err)
	}
	if output, err := run(overrideInstaller, ""); err == nil {
		t.Fatalf("override without verification succeeded: %s", output)
	}
	badVerification := filepath.Join(t.TempDir(), "release-verification.json")
	if err := os.WriteFile(badVerification, []byte(strings.Replace(verificationBody, fmt.Sprintf("%x", digest), strings.Repeat("0", 64), 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := run(overrideInstaller, badVerification); err == nil {
		t.Fatalf("checksum mismatch succeeded: %s", output)
	}
	symlink := filepath.Join(t.TempDir(), "install-front-slots.sh")
	if err := os.Symlink(overrideInstaller, symlink); err != nil {
		t.Fatal(err)
	}
	if output, err := run(symlink, verification); err == nil {
		t.Fatalf("symlink override succeeded: %s", output)
	}

	contractResolver := filepath.Join(repoRoot, "deploy", "gcp", "resolve-release-contract.sh")
	runContract := func(contract, evidence string) ([]byte, error) {
		command := exec.Command(mustLookPath(t, "bash"), contractResolver, defaultContract)
		command.Env = append(
			os.Environ(),
			"SUBROUTER_DEPLOYMENT_CONTRACT="+contract,
			"SUBROUTER_RELEASE_VERIFICATION_JSON="+evidence,
		)
		return command.CombinedOutput()
	}
	if output, err := runContract("", ""); err != nil || strings.TrimSpace(string(output)) != defaultContract {
		t.Fatalf("default contract = %q, %v", output, err)
	}
	if output, err := runContract(overrideContract, verification); err != nil || strings.TrimSpace(string(output)) != overrideContract {
		t.Fatalf("verified contract = %q, %v", output, err)
	}
	badContractVerification := filepath.Join(t.TempDir(), "release-verification.json")
	if err := os.WriteFile(badContractVerification, []byte(strings.Replace(verificationBody, fmt.Sprintf("%x", contractDigest), strings.Repeat("0", 64), 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := runContract(overrideContract, badContractVerification); err == nil {
		t.Fatalf("contract override with mismatched verification succeeded: %s", output)
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
