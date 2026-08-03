package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type goldenSlowWriter struct {
	delay  time.Duration
	writes atomic.Int32
}

func (writer *goldenSlowWriter) Write(data []byte) (int, error) {
	time.Sleep(writer.delay)
	writer.writes.Add(1)
	return len(data), nil
}

func TestGoldenInitialReadyDoesNotSpinAfterThreadBecomesAvailable(t *testing.T) {
	threadAvailable := make(chan struct{})
	close(threadAvailable)
	session := &goldenSession{
		observer:        &runningGoldenObserver{stats: newObserverStats()},
		done:            make(chan struct{}),
		threadAvailable: threadAvailable,
	}
	allocations := testing.AllocsPerRun(3, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		err := waitGoldenInitialReady(ctx, session)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("wait error = %v, want context deadline", err)
		}
	})
	if allocations > 64 {
		t.Fatalf("allocations while waiting after thread readiness = %.0f, want at most 64", allocations)
	}
}

func TestGoldenSamplingEvidenceSlowWriterDoesNotCreateSamplingGap(t *testing.T) {
	previous := goldenTestHooks
	goldenTestHooks.enabled = true
	goldenTestHooks.processTable = func(pids []int) (goldenProcessTable, error) {
		processes := make(map[int]goldenProcessSample, len(pids))
		for _, pid := range pids {
			processes[pid] = goldenProcessSample{parent: 1, state: "S", rss: 1 << 20}
		}
		return goldenProcessTable{processes: processes, children: make(map[int][]int)}, nil
	}
	t.Cleanup(func() { goldenTestHooks = previous })

	slow := &goldenSlowWriter{delay: 150 * time.Millisecond}
	runner := &goldenRunner{evidence: &jsonlRecorder{writer: slow}}
	runner.samplingEvidence = newGoldenSamplingEvidenceWriter(runner.evidence, 32)
	for range 6 {
		runner.recordGoldenProcessSample(42)
		time.Sleep(goldenProcessSampleInterval)
	}
	if err := runner.stopSamplingEvidenceWriter(); err != nil {
		t.Fatal(err)
	}
	if runner.localRSSSamples != 6 {
		t.Fatalf("samples = %d, want 6", runner.localRSSSamples)
	}
	if runner.localMaxSampleGap > goldenProcessSampleMaxGap {
		t.Fatalf("sampling gap = %s, limit %s", runner.localMaxSampleGap, goldenProcessSampleMaxGap)
	}
	if got := slow.writes.Load(); got != 6 {
		t.Fatalf("evidence writes = %d, want 6", got)
	}
}

type goldenFailingWriter struct{}

func (goldenFailingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestGoldenSamplingEvidenceWriteFailureFailsGate(t *testing.T) {
	runner := &goldenRunner{evidence: &jsonlRecorder{writer: goldenFailingWriter{}}}
	runner.samplingEvidence = newGoldenSamplingEvidenceWriter(runner.evidence, 1)
	runner.recordSamplingEvidence(map[string]any{"kind": "process_sample"})
	if got := fixedGoldenFailure(runner.stopSamplingEvidenceWriter()); got != "evidence_write_failed" {
		t.Fatalf("failure = %q, want evidence_write_failed", got)
	}
}

type goldenBlockingWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (writer *goldenBlockingWriter) Write(data []byte) (int, error) {
	writer.once.Do(func() { close(writer.started) })
	<-writer.release
	return len(data), nil
}

func TestGoldenSamplingEvidenceOverflowFailsGate(t *testing.T) {
	blocking := &goldenBlockingWriter{started: make(chan struct{}), release: make(chan struct{})}
	runner := &goldenRunner{evidence: &jsonlRecorder{writer: blocking}}
	runner.samplingEvidence = newGoldenSamplingEvidenceWriter(runner.evidence, 1)
	runner.recordSamplingEvidence(map[string]any{"sample": 1})
	<-blocking.started
	runner.recordSamplingEvidence(map[string]any{"sample": 2})
	runner.recordSamplingEvidence(map[string]any{"sample": 3})
	close(blocking.release)
	if got := fixedGoldenFailure(runner.stopSamplingEvidenceWriter()); got != "evidence_write_failed" {
		t.Fatalf("failure = %q, want evidence_write_failed", got)
	}
}

func TestGoldenProcessSamplingMissingRootUsesProcessLiveness(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows golden command is compile-only and has no process liveness probe")
	}

	previous := goldenTestHooks
	t.Cleanup(func() { goldenTestHooks = previous })
	localPID := os.Getpid() + 1_000_000
	goldenTestHooks.enabled = true
	goldenTestHooks.processTable = func([]int) (goldenProcessTable, error) {
		return goldenProcessTable{
			processes: map[int]goldenProcessSample{
				localPID: {parent: 1, state: "S", rss: 1 << 20},
			},
			children: make(map[int][]int),
		}, nil
	}

	t.Run("live root is a sampling failure", func(t *testing.T) {
		process, err := os.FindProcess(os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		if !processAlive(process) {
			t.Fatal("test process is not alive")
		}
		session := &goldenSession{
			command: &exec.Cmd{Process: process},
			done:    make(chan struct{}),
			issues:  make(map[string]int),
		}
		runner := &goldenRunner{sessions: []*goldenSession{session}}
		runner.recordGoldenProcessSample(localPID)

		session.mu.Lock()
		failures := session.processSampleFailures
		session.mu.Unlock()
		if failures != 1 {
			t.Fatalf("process sample failures = %d, want 1", failures)
		}
	})

	t.Run("exited root is ignored but session validation still fails", func(t *testing.T) {
		command := exec.Command(os.Args[0], "-test.run=^$")
		if err := command.Run(); err != nil {
			t.Fatal(err)
		}
		if processAlive(command.Process) {
			t.Fatal("completed test process is still alive")
		}
		session := &goldenSession{
			command: command,
			done:    make(chan struct{}),
			issues:  make(map[string]int),
		}
		runner := &goldenRunner{sessions: []*goldenSession{session}}
		runner.recordGoldenProcessSample(localPID)

		session.mu.Lock()
		failures := session.processSampleFailures
		session.mu.Unlock()
		if failures != 0 {
			t.Fatalf("process sample failures = %d, want 0", failures)
		}
		if got := fixedGoldenFailure(validateGoldenSessions([]*goldenSession{session}, false)); got != "completion_marker_missing" {
			t.Fatalf("session validation failure = %q, want completion_marker_missing", got)
		}
	})
}

func TestGoldenLaunchSessionWaitsForProcessReadinessBeforeSamplerVisibility(t *testing.T) {
	previous := goldenTestHooks
	t.Cleanup(func() { goldenTestHooks = previous })

	root := t.TempDir()
	client := filepath.Join(root, "fake-client")
	writeGoldenExecutable(t, client, "#!/bin/sh\nexec /bin/sleep 30\n")
	runner := &goldenRunner{
		privateRoot: root,
		options:     goldenOptions{model: "test", codexBinary: client},
		evidence:    &jsonlRecorder{writer: io.Discard},
	}
	session := &goldenSession{
		label: "readiness", route: "direct-hosted", transport: "websocket",
		home: root, codexHome: root, streamReleaseToken: "11111111111111111111111111111111", issues: make(map[string]int),
		done: make(chan struct{}), threadAvailable: make(chan struct{}),
	}
	readyEntered := make(chan int, 1)
	readyRelease := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(readyRelease) }) }
	goldenTestHooks.enabled = true
	goldenTestHooks.sessionProcessReady = func(_ context.Context, process *os.Process) error {
		readyEntered <- process.Pid
		<-readyRelease
		return nil
	}
	t.Cleanup(func() {
		release()
		runner.stopAll()
	})

	launchResult := make(chan error, 1)
	go func() {
		launchResult <- runner.launchSession(context.Background(), client, session, "", "test")
	}()
	var startedPID int
	select {
	case startedPID = <-readyEntered:
	case err := <-launchResult:
		t.Fatalf("launch returned before readiness hook: %v", err)
	case <-time.After(time.Second):
		t.Fatal("readiness hook was not called")
	}
	runner.mu.Lock()
	visibleBeforeReady := len(runner.sessions)
	runner.mu.Unlock()
	if visibleBeforeReady != 0 {
		t.Fatalf("sampler-visible sessions before readiness = %d, want 0", visibleBeforeReady)
	}
	release()
	select {
	case err := <-launchResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("launch did not finish after process became ready")
	}
	runner.mu.Lock()
	visibleAfterReady := len(runner.sessions)
	runner.mu.Unlock()
	if visibleAfterReady != 1 || session.command == nil || session.command.Process.Pid != startedPID {
		t.Fatalf("ready session was not registered: visible=%d command=%v pid=%d", visibleAfterReady, session.command, startedPID)
	}
}

func TestGoldenSessionRegistrationSurvivesTeardownSampling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows golden command is compile-only and has no process liveness probe")
	}

	previous := goldenTestHooks
	t.Cleanup(func() { goldenTestHooks = previous })
	root := t.TempDir()
	processState := filepath.Join(root, "process-state")
	if err := os.Mkdir(processState, 0o700); err != nil {
		t.Fatal(err)
	}
	localPID := os.Getpid()
	localRecord := fmt.Sprintf("%d %d S 1024\n", localPID, os.Getppid())
	if err := os.WriteFile(filepath.Join(processState, strconv.Itoa(localPID)), []byte(localRecord), 0o600); err != nil {
		t.Fatal(err)
	}
	client := filepath.Join(root, "registered-client")
	writeGoldenExecutable(t, client, `#!/bin/sh
set -eu
record="$SUBROUTER_GOLDEN_FAKE_PROCESS_STATE/$$"
temporary="$SUBROUTER_GOLDEN_FAKE_PROCESS_STATE/.process-$$"
printf '%s %s S 1024\n' "$$" "$PPID" >"$temporary"
mv "$temporary" "$record"
/bin/sleep 0.005
`)
	t.Setenv("SUBROUTER_GOLDEN_FAKE_PROCESS_STATE", processState)

	finished := make(chan int, 32)
	finishErrors := make(chan error, 32)
	goldenTestHooks.enabled = true
	goldenTestHooks.processTable = func(pids []int) (goldenProcessTable, error) {
		return loadGoldenFakeProcessTable(processState, pids)
	}
	goldenTestHooks.sessionProcessReady = func(ctx context.Context, process *os.Process) error {
		return waitGoldenFakeProcessRegistration(ctx, processState, process)
	}
	goldenTestHooks.sessionProcessDone = func(process *os.Process) {
		path := filepath.Join(processState, strconv.Itoa(process.Pid))
		if _, err := os.Stat(path); err != nil {
			finishErrors <- fmt.Errorf("registration %d was not retained through Wait: %w", process.Pid, err)
		} else if err := os.Remove(path); err != nil {
			finishErrors <- err
		}
		finished <- process.Pid
	}
	runner := &goldenRunner{
		privateRoot: root,
		options:     goldenOptions{model: "test", codexBinary: client},
		evidence:    &jsonlRecorder{writer: io.Discard},
	}
	t.Cleanup(func() { _ = runner.stopAll() })

	sampleCtx, cancelSampling := context.WithCancel(context.Background())
	samplingDone := make(chan struct{})
	go func() {
		defer close(samplingDone)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-sampleCtx.Done():
				return
			case <-ticker.C:
				runner.recordGoldenProcessSample(localPID)
			}
		}
	}()
	t.Cleanup(func() {
		cancelSampling()
		<-samplingDone
	})

	for index := range 24 {
		session := &goldenSession{
			label: fmt.Sprintf("teardown-%d", index), route: "direct-hosted", transport: "websocket",
			home: root, codexHome: root, streamReleaseToken: fmt.Sprintf("%032x", index+1), issues: make(map[string]int),
			done: make(chan struct{}), threadAvailable: make(chan struct{}),
		}
		if err := runner.launchSession(context.Background(), client, session, "", "test"); err != nil {
			t.Fatal(err)
		}
		select {
		case <-session.done:
		case <-time.After(time.Second):
			t.Fatalf("session %d did not finish", index)
		}
		select {
		case pid := <-finished:
			if pid != session.command.Process.Pid {
				t.Fatalf("finished PID = %d, want %d", pid, session.command.Process.Pid)
			}
		case <-time.After(time.Second):
			t.Fatalf("session %d registration was not released", index)
		}
		select {
		case err := <-finishErrors:
			t.Fatal(err)
		default:
		}
		session.mu.Lock()
		failures := session.processSampleFailures
		session.mu.Unlock()
		if failures != 0 {
			t.Fatalf("session %d process sample failures = %d, want 0", index, failures)
		}
	}
	cancelSampling()
	<-samplingDone
	runner.localRSSMu.Lock()
	localFailures := runner.localSampleFailures
	localSamples := runner.localRSSSamples
	runner.localRSSMu.Unlock()
	if localFailures != 0 || localSamples == 0 {
		t.Fatalf("local samples = %d, failures = %d", localSamples, localFailures)
	}
}

func TestGoldenSocketSnapshotPreservesLsofNoMatchesExit(t *testing.T) {
	bin := t.TempDir()
	writeGoldenExecutable(t, filepath.Join(bin, "lsof"), "#!/bin/sh\nprintf 'n127.0.0.1:41000->203.0.113.10:443\\n'\nexit 1\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := goldenSocketSnapshot(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "n127.0.0.1:41000->203.0.113.10:443\n" {
		t.Fatalf("output = %q", output)
	}
}
