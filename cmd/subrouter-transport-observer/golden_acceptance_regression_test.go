package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGoldenSummaryRejectsActivationAtOrAboveThirtySeconds(t *testing.T) {
	summary := validGoldenAcceptanceSummary()
	summary.Activation.StartedAt = "2026-08-02T00:00:00Z"
	summary.Activation.FinishedAt = "2026-08-02T00:00:30Z"
	if err := validateGoldenSummary(summary, false); err == nil {
		t.Fatal("accepted an activation that did not finish in under 30 seconds")
	}
}

func TestGoldenSummaryRejectsStalledOriginalStream(t *testing.T) {
	summary := validGoldenAcceptanceSummary()
	for index := range summary.Sessions {
		if summary.Sessions[index].Label == "direct-websocket" {
			summary.Sessions[index].MaxChunkGapMillis = 5_001
		}
	}
	if err := validateGoldenSummary(summary, false); err == nil {
		t.Fatal("accepted an original stream stalled beyond the five-second floor")
	}
}

func TestGoldenActionRejectsSleepOnlySuccess(t *testing.T) {
	result := runGoldenAcceptanceAction(t, "sleep-only", "sleep 0.01")
	if result.ExitCode == 0 {
		t.Fatal("accepted a successful sleep without structured transition evidence")
	}
}

func TestGoldenActionRejectsRestartAndOOMDeltas(t *testing.T) {
	for _, test := range []struct {
		name         string
		restartDelta int
		oomDelta     int
	}{
		{name: "restart", restartDelta: 1},
		{name: "oom", oomDelta: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := goldenTransitionPayload("generation-a", "generation-b", strings.Repeat("a", 64), strings.Repeat("b", 64), test.restartDelta, test.oomDelta)
			result := runGoldenAcceptanceAction(t, "activation", "printf '%s\\n' '"+payload+"'")
			if result.ExitCode == 0 {
				t.Fatalf("accepted %s delta", test.name)
			}
		})
	}
}

func TestGoldenSummaryRejectsRollbackThatDoesNotReverseActivation(t *testing.T) {
	summary := validGoldenAcceptanceSummary()
	activation := goldenTransitionPayload("generation-a", "generation-b", strings.Repeat("a", 64), strings.Repeat("b", 64), 0, 0)
	rollback := goldenTransitionPayload("generation-c", "generation-a", strings.Repeat("c", 64), strings.Repeat("a", 64), 0, 0)
	summary.Activation = runGoldenAcceptanceAction(t, "activation", "printf '%s\\n' '"+activation+"'")
	summary.Rollback = runGoldenAcceptanceAction(t, "rollback", "printf '%s\\n' '"+rollback+"'")
	if err := validateGoldenSummary(summary, false); err == nil {
		t.Fatal("accepted a rollback that did not exactly reverse the activated generation")
	}
}

func TestGoldenOldGenerationCheckRejectsMissingRetirement(t *testing.T) {
	payload := `{"old_generation_id":"generation-b","active":false,"accepting":false,"connections":0,"server_rss_bytes":1048576}`
	result := runGoldenOpaqueEvidence(t, payload)
	if result.ExitCode == 0 {
		t.Fatal("accepted old-generation evidence without bounded retirement")
	}
}

func TestGoldenOldGenerationCheckRejectsMissingServerRSS(t *testing.T) {
	payload := `{"old_generation_id":"generation-b","active":false,"accepting":false,"connections":0,"retired_within_ms":1000}`
	result := runGoldenOpaqueEvidence(t, payload)
	if result.ExitCode == 0 {
		t.Fatal("accepted old-generation evidence without server RSS")
	}
}

func TestGoldenProcessEvidenceRejectsProcessTreeAbove192MiB(t *testing.T) {
	bin := t.TempDir()
	writeGoldenExecutable(t, filepath.Join(bin, "pgrep"), "#!/bin/sh\nexit 1\n")
	writeGoldenExecutable(t, filepath.Join(bin, "lsof"), "#!/bin/sh\nprintf 'n127.0.0.1:41000->203.0.113.10:443\\n'\n")
	writeGoldenExecutable(t, filepath.Join(bin, "ps"), `#!/bin/sh
case "$*" in
  *state=*) printf 'S\n' ;;
  *rss=*) printf '200000\n' ;;
  *) exit 1 ;;
esac
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := captureProcessEvidence("before-activation", "direct-websocket", os.Getpid()); err == nil {
		t.Fatal("accepted a process tree above 192 MiB RSS")
	}
}

func TestGoldenStableSocketMustBeResponseTransportSocket(t *testing.T) {
	actual := strings.Repeat("a", 64)
	unrelated := strings.Repeat("b", 64)
	stats := newObserverStats()
	stats.observe(transportEvent{
		Kind: "request_started", Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Method: "POST", Path: "/v1/responses", RequestID: "request-1", ConnectionID: actual,
	})
	session := &goldenSession{label: "direct-websocket", observer: &runningGoldenObserver{stats: stats}}
	before := map[string]goldenProcessEvidence{
		session.label: {Label: session.label, SocketIDs: []string{unrelated}},
	}
	after := map[string]goldenProcessEvidence{
		session.label: {Label: session.label, SocketIDs: []string{unrelated}},
	}
	if err := requireStableSessionSockets([]*goldenSession{session}, before, after); err == nil {
		t.Fatal("accepted overlap from an unrelated descendant socket")
	}
}

func runGoldenAcceptanceAction(t *testing.T, label, body string) goldenActionSummary {
	t.Helper()
	script := filepath.Join(t.TempDir(), label+".sh")
	writeGoldenExecutable(t, script, "#!/bin/sh\nset -eu\n"+body+"\n")
	runner := &goldenRunner{evidence: &jsonlRecorder{writer: io.Discard}}
	action, err := runner.startAction(context.Background(), label, []string{script})
	if err != nil {
		t.Fatal(err)
	}
	return waitAction(action)
}

func runGoldenOpaqueEvidence(t *testing.T, payload string) goldenActionSummary {
	t.Helper()
	script := filepath.Join(t.TempDir(), "old-generation.sh")
	writeGoldenExecutable(t, script, "#!/bin/sh\nprintf '%s\\n' '"+payload+"'\n")
	return runOpaqueAction(context.Background(), []string{script})
}

func writeGoldenExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}

func goldenTransitionPayload(fromGeneration, toGeneration, fromRelease, toRelease string, restartDelta, oomDelta int) string {
	return `{"from_generation_id":"` + fromGeneration + `","to_generation_id":"` + toGeneration +
		`","from_release_sha256":"` + fromRelease + `","to_release_sha256":"` + toRelease +
		`","active_generation_id":"` + toGeneration + `","restart_delta":` + string(rune('0'+restartDelta)) +
		`,"oom_delta":` + string(rune('0'+oomDelta)) + `}`
}

func validGoldenAcceptanceSummary() goldenSummary {
	stamp := "2026-08-02T00:00:00Z"
	action := goldenActionSummary{StartedAt: stamp, FinishedAt: "2026-08-02T00:00:01Z", ExitCode: 0}
	hash := strings.Repeat("a", 64)
	labels := []struct {
		label, route, transport string
	}{
		{label: "direct-websocket", route: "direct-hosted", transport: "websocket"},
		{label: "direct-http", route: "direct-hosted", transport: "http"},
		{label: "local-websocket", route: "local-egress", transport: "websocket"},
		{label: "local-http", route: "local-egress", transport: "http"},
		{label: "activation-direct", route: "direct-hosted", transport: "websocket"},
		{label: "activation-local", route: "local-egress", transport: "websocket"},
	}
	summary := goldenSummary{
		ReleasedVersion: "1.2.3", ReleasedSHA256: hash, ReleaseChecksumVerified: true,
		ReleasePlatform: "darwin/arm64", Activation: action, Rollback: action,
		OldGenerationCleanup: action, ProbeFrequencyHz: 10, FreshLocalLeaseObserved: true,
	}
	for _, label := range []string{"public-health", "public-ready", "local-health", "local-ready"} {
		summary.Health = append(summary.Health, goldenProbeSummary{Label: label, Samples: 10, MaxStartGapMillis: 100})
	}
	for index, item := range labels {
		session := goldenSessionSummary{
			Label: item.label, Route: item.route, Transport: item.transport, ProcessID: index + 1,
			ThreadIDHash: hash, NonceHash: hash, ResponseRequests: 1, ResponseConnections: 1,
			ResponseBytes: 1, MaxChunkGapMillis: 1, MarkerCount: 1,
		}
		if !strings.HasPrefix(item.label, "activation-") {
			session.ResumeMarkerCount = 1
			session.ResumeNonceCount = 1
			session.SocketIDsBefore = []string{hash}
			session.SocketIDsAfterRollback = []string{hash}
		}
		summary.Sessions = append(summary.Sessions, session)
	}
	initial := []string{"direct-websocket", "direct-http", "local-websocket", "local-http", "local-daemon"}
	observers := []string{"observer-local-lease", "observer-direct-websocket", "observer-direct-http", "observer-local-websocket", "observer-local-http"}
	for _, phase := range []string{"before-activation", "after-activation", "after-rollback"} {
		for _, label := range initial {
			evidence := goldenProcessEvidence{
				Timestamp: stamp, Phase: phase, Label: label, ProcessID: 1,
				DescendantPIDs: []int{1}, ProcessStates: []string{"S"}, SocketIDs: []string{hash},
			}
			if label == "local-daemon" {
				evidence.RemoteSocketIDs = []string{hash}
			}
			summary.ProcessSnapshots = append(summary.ProcessSnapshots, evidence)
		}
		phaseObservers := append([]string(nil), observers...)
		if phase != "before-activation" {
			phaseObservers = append(phaseObservers, "observer-activation-direct", "observer-activation-local")
		}
		for _, label := range phaseObservers {
			summary.ProcessSnapshots = append(summary.ProcessSnapshots, goldenProcessEvidence{
				Timestamp: stamp, Phase: phase, Label: label, ProcessID: 1, ProcessStates: []string{"S"},
			})
		}
	}
	return summary
}
