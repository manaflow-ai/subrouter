package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitHubReleasePublisherRecoversDraftAndVerifiesImmutableRerun(t *testing.T) {
	requireDeployScriptTools(t, "bash", "jq", "sha256sum")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	script := filepath.Join(repoRoot, "scripts", "publish-github-release.sh")

	for _, test := range []struct {
		name               string
		initialState       string
		verifyFailures     int
		wantVerifyAttempts int
		wantMutations      map[string]bool
	}{
		{
			name: "publish missing release", initialState: "missing",
			wantVerifyAttempts: 2,
			wantMutations:      map[string]bool{"release create": true, "release upload": true, "release edit": true},
		},
		{
			name: "replace incomplete draft", initialState: "draft",
			wantVerifyAttempts: 2,
			wantMutations:      map[string]bool{"release delete": true, "release create": true, "release upload": true, "release edit": true},
		},
		{name: "verify completed immutable release", initialState: "immutable", wantVerifyAttempts: 2},
		{
			name: "retry verification while attestations propagate", initialState: "missing",
			verifyFailures: 2, wantVerifyAttempts: 4,
			wantMutations: map[string]bool{"release create": true, "release upload": true, "release edit": true},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assetDir := t.TempDir()
			assetBody := []byte("release bytes\n")
			if err := os.WriteFile(filepath.Join(assetDir, "asset.bin"), assetBody, 0o600); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(assetBody)
			if err := os.WriteFile(
				filepath.Join(assetDir, "SHA256SUMS"),
				[]byte(fmt.Sprintf("%x  asset.bin\n", digest)),
				0o600,
			); err != nil {
				t.Fatal(err)
			}

			fakeBin := t.TempDir()
			statePath := filepath.Join(t.TempDir(), "release-state")
			if err := os.WriteFile(statePath, []byte(test.initialState+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(t.TempDir(), "gh.log")
			verifyCountPath := filepath.Join(t.TempDir(), "verify-count")
			if err := os.WriteFile(verifyCountPath, []byte("0\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			writeExecutableTestFile(t, filepath.Join(fakeBin, "gh"), `#!/bin/sh
set -eu
printf '%s\n' "$*" >>"$FAKE_GH_LOG"
operation="$1 $2"
shift 2
state="$(cat "$FAKE_RELEASE_STATE")"
case "$operation" in
  "release view")
    case "$*" in
      *"isDraft,isImmutable,assets"*)
        [ "$state" != missing ] || exit 1
        find "$FAKE_ASSET_DIR" -maxdepth 1 -type f -print0 \
          | sort -z \
          | xargs -0 sha256sum \
          | awk '{name=$2; sub(/^.*\//,"",name); printf "%s\tsha256:%s\n", name, $1}' \
          | LC_ALL=C sort
        ;;
      *"--jq"*)
        [ "$state" = immutable ] || exit 1
        printf 'true\n'
        ;;
      *)
        case "$state" in
          missing) printf 'release not found\n' >&2; exit 1 ;;
          draft) printf '{"isDraft":true,"isImmutable":false}\n' ;;
          immutable) printf '{"isDraft":false,"isImmutable":true}\n' ;;
          mutable) printf '{"isDraft":false,"isImmutable":false}\n' ;;
          *) exit 2 ;;
        esac
        ;;
    esac
    ;;
  "release delete")
    [ "$state" = draft ] || exit 1
    printf 'missing\n' >"$FAKE_RELEASE_STATE"
    ;;
  "release create")
    [ "$state" = missing ] || exit 1
    printf 'draft\n' >"$FAKE_RELEASE_STATE"
    ;;
  "release upload")
    [ "$state" = draft ] || exit 1
    ;;
  "release edit")
    [ "$state" = draft ] || exit 1
    printf 'immutable\n' >"$FAKE_RELEASE_STATE"
    ;;
  "release verify-asset")
    [ "$state" = immutable ] || exit 1
		count="$(cat "$FAKE_VERIFY_COUNT")"
		count=$((count + 1))
		printf '%s\n' "$count" >"$FAKE_VERIFY_COUNT"
		if [ "$count" -le "$FAKE_VERIFY_FAILURES" ]; then
			printf 'attestation index has not propagated\n' >&2
			exit 1
		fi
    printf '{}\n'
    ;;
  "attestation verify")
    printf '[{"verificationResult":{"statement":{"subject":[{"name":"asset","digest":{"sha256":"%s"}}]}}}]\n' "$FAKE_DIGEST"
    ;;
  *) exit 2 ;;
esac
`)

			command := exec.Command(mustLookPath(t, "bash"), script, assetDir)
			command.Env = append(os.Environ(),
				"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"GH_REPO=manaflow-ai/subrouter",
				"TAG_NAME=v0.1.52",
				"SOURCE_COMMIT="+strings.Repeat("b", 40),
				"FAKE_ASSET_DIR="+assetDir,
				"FAKE_RELEASE_STATE="+statePath,
				"FAKE_GH_LOG="+logPath,
				"FAKE_DIGEST="+fmt.Sprintf("%x", digest),
				"FAKE_VERIFY_COUNT="+verifyCountPath,
				fmt.Sprintf("FAKE_VERIFY_FAILURES=%d", test.verifyFailures),
			)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("publish release: %v\n%s", err, output)
			}
			calls, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			callLog := string(calls)
			for _, operation := range []string{"release delete", "release create", "release upload", "release edit"} {
				want := test.wantMutations[operation]
				if strings.Contains(callLog, operation) != want {
					t.Fatalf("%s mutation presence = %t, want %t\n%s", operation, strings.Contains(callLog, operation), want, callLog)
				}
			}
			if strings.Contains(callLog, "--clobber") {
				t.Fatalf("publisher mutates release assets with --clobber:\n%s", callLog)
			}
			if !strings.Contains(callLog, "release verify-asset") {
				t.Fatalf("publisher did not verify immutable release assets:\n%s", callLog)
			}
			if attempts := strings.Count(callLog, "release verify-asset"); attempts != test.wantVerifyAttempts {
				t.Fatalf("immutable verification attempts = %d, want %d:\n%s", attempts, test.wantVerifyAttempts, callLog)
			}
		})
	}
}

func TestGitHubReleasePublisherRejectsPublishedMutableRelease(t *testing.T) {
	requireDeployScriptTools(t, "bash", "jq", "sha256sum")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	script := filepath.Join(repoRoot, "scripts", "publish-github-release.sh")
	assetDir := t.TempDir()
	body := []byte("release bytes\n")
	if err := os.WriteFile(filepath.Join(assetDir, "asset.bin"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	if err := os.WriteFile(filepath.Join(assetDir, "SHA256SUMS"), []byte(fmt.Sprintf("%x  asset.bin\n", digest)), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	writeExecutableTestFile(t, filepath.Join(fakeBin, "gh"), "#!/bin/sh\nprintf '{\"isDraft\":false,\"isImmutable\":false}\\n'\n")
	command := exec.Command(mustLookPath(t, "bash"), script, assetDir)
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GH_REPO=manaflow-ai/subrouter",
		"TAG_NAME=v0.1.52",
		"SOURCE_COMMIT="+strings.Repeat("b", 40),
	)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("publisher accepted a mutable published release:\n%s", output)
	}
}

func TestGitHubReleasePublisherRejectsAmbiguousLookupFailureWithoutMutation(t *testing.T) {
	requireDeployScriptTools(t, "bash", "jq", "sha256sum")
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	script := filepath.Join(repoRoot, "scripts", "publish-github-release.sh")
	assetDir := t.TempDir()
	body := []byte("release bytes\n")
	if err := os.WriteFile(filepath.Join(assetDir, "asset.bin"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	if err := os.WriteFile(filepath.Join(assetDir, "SHA256SUMS"), []byte(fmt.Sprintf("%x  asset.bin\n", digest)), 0o600); err != nil {
		t.Fatal(err)
	}

	fakeBin := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "gh.log")
	writeExecutableTestFile(t, filepath.Join(fakeBin, "gh"), `#!/bin/sh
set -eu
printf '%s\n' "$*" >>"$FAKE_GH_LOG"
if [ "$1 $2" = "release view" ]; then
  printf 'HTTP 404: Not Found (https://api.github.com/repos/no-access/private-repo/releases/tags/v0.1.52)\n' >&2
  exit 1
fi
exit 97
`)
	command := exec.Command(mustLookPath(t, "bash"), script, assetDir)
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GH_REPO=manaflow-ai/subrouter",
		"TAG_NAME=v0.1.52",
		"SOURCE_COMMIT="+strings.Repeat("b", 40),
		"FAKE_GH_LOG="+logPath,
	)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("publisher accepted ambiguous release lookup failure:\n%s", output)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	callLog := string(calls)
	for _, operation := range []string{"release delete", "release create", "release upload", "release edit"} {
		if strings.Contains(callLog, operation) {
			t.Fatalf("publisher attempted %s after ambiguous lookup failure:\n%s", operation, callLog)
		}
	}
}
