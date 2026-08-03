package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
