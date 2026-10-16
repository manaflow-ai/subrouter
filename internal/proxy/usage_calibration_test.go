package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func calibrationStatus() AccountUsageStatus {
	return AccountUsageStatus{
		AccountStatus: AccountStatus{ID: "private@example.com", Provider: accounts.ProviderClaude, Email: "private@example.com", Label: "private-label", Source: "private-source"},
		UsageFresh:    true, UsageFetchedAt: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC), PlanType: "max",
		Windows: []accounts.UsageWindow{{Name: "5h", UsedPercent: 10, LimitWindowSeconds: 18000, ResetAfterSeconds: 3600}},
	}
}

func calibrationNext(t *testing.T, c *UsageCalibrationRecorder) calibrationObservation {
	t.Helper()
	select {
	case body := <-c.queue:
		for _, private := range []string{"private@example.com", "private-label", "private-source", "private-device", "private-secret"} {
			if strings.Contains(string(body), private) {
				t.Fatalf("private data persisted: %s", body)
			}
		}
		var observation calibrationObservation
		if err := json.Unmarshal(body, &observation); err != nil {
			t.Fatal(err)
		}
		return observation
	default:
		t.Fatal("missing observation")
		return calibrationObservation{}
	}
}

func TestUsageCalibrationCountersSurviveHourlyRollover(t *testing.T) {
	c := NewUsageCalibrationRecorder("")
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	usage := NewTokenUsageRecorder("", nil, WithUsageCalibrationRecorder(c), WithTokenUsageClock(func() time.Time { return now }))
	usage.Record("claude", "private@example.com", "model", "private-device", tokenUsage{InputTokens: 100, CachedInputTokens: 80, CacheWriteInputTokens: 5, OutputTokens: 20}, true)
	status := calibrationStatus()
	c.observe(status, "generation-one")
	first := calibrationNext(t, c)
	now = now.Add(2 * time.Hour)
	usage.Record("claude", "private@example.com", "model", "private-device", tokenUsage{}, false)
	status.UsageFetchedAt = now
	c.observe(status, "generation-one")
	second := calibrationNext(t, c)
	if len(second.Counters) != 1 || second.Counters[0].Requests != 2 || second.Counters[0].RequestsWithoutUsage != 1 || second.Counters[0].InputTokens != 100 || second.Counters[0].CachedInputTokens != 80 {
		t.Fatalf("not cumulative: %+v", second)
	}
	if first.WorkerEpoch != second.WorkerEpoch || first.Sequence >= second.Sequence {
		t.Fatal("epoch/sequence not stable")
	}
	if !first.Windows[0].ResetAt.Equal(first.ProviderObservedAt.Add(time.Hour)) {
		t.Fatalf("missing reset identity: %+v", first.Windows)
	}
	if len(first.Limitations) == 0 || first.CountersSampledAt.IsZero() {
		t.Fatal("missing uncertainty or capture time")
	}
}

func TestUsageCalibrationDeduplicatesFreshWindowsAndExcludesFallback(t *testing.T) {
	c := NewUsageCalibrationRecorder("")
	status := calibrationStatus()
	c.observe(status, "g1")
	calibrationNext(t, c)
	c.observe(status, "g1")
	status.UsageFetchedAt = status.UsageFetchedAt.Add(time.Minute)
	status.UsageFresh = false
	c.observe(status, "g1")
	status.UsageFresh = true
	status.UsageThrottled = true
	c.observe(status, "g1")
	status.UsageThrottled = false
	status.Windows[0].Feature = "model-only"
	c.observe(status, "g1")
	if len(c.queue) != 0 {
		t.Fatal("cached, stale, throttled, or partial sample recorded")
	}
	status.Windows[0].Feature = ""
	status.AccountWideUsageFresh = true
	status.UsageFresh = false
	c.observe(status, "g1")
	calibrationNext(t, c)
	c.observe(status, "g2")
	if calibrationNext(t, c).AccountGeneration != "g2" {
		t.Fatal("generation change lost")
	}
	if NewUsageCalibrationRecorder("").epoch == c.epoch {
		t.Fatal("worker epoch reused")
	}
}

func TestUsageCalibrationPersistenceHook(t *testing.T) {
	c := NewUsageCalibrationRecorder("")
	ref := cacheTestAccountRef(t, &usageRoundTripper{responses: []*http.Response{usageOKResponse()}})
	ref.SetUsageCalibrationRecorder(c)
	statuses := ref.UsageStatuses(context.Background())
	if len(statuses) != 1 {
		t.Fatal("missing fixture status")
	}
	got := calibrationNext(t, c)
	if got.Provider != "claude" || got.AccountGeneration == "" || len(got.Windows) == 0 {
		t.Fatalf("bad hook snapshot: %+v", got)
	}
}

func TestUsageCalibrationBoundsAndDrops(t *testing.T) {
	c := NewUsageCalibrationRecorder("")
	status := calibrationStatus()
	for i := 0; i < usageCalibrationQueueSize+1; i++ {
		status.UsageFetchedAt = status.UsageFetchedAt.Add(time.Minute)
		c.observe(status, "g")
	}
	if len(c.queue) != usageCalibrationQueueSize || c.dropped != 1 {
		t.Fatalf("queue not bounded: %d, %d", len(c.queue), c.dropped)
	}
	<-c.queue
	status.UsageFetchedAt = status.UsageFetchedAt.Add(time.Minute)
	c.observe(status, "g")
	var last calibrationObservation
	for len(c.queue) > 0 {
		last = calibrationNext(t, c)
	}
	if last.DroppedObservations != 1 {
		t.Fatal("missing queue drop metadata")
	}
	for i := 0; i < tokenUsageMaxKeysPerHour; i++ {
		c.counters[calibrationCounterKey{account: "a", model: string(rune(i + 1))}] = calibrationCounters{}
	}
	c.record(tokenUsageKey{Provider: "claude", AccountID: "new", Model: "new"}, tokenUsageCounts{Requests: 1})
	if len(c.counters) != tokenUsageMaxKeysPerHour || !c.incomplete {
		t.Fatal("unbounded model counters")
	}
}

func TestUsageCalibrationWriterLocksRotatesAndKeepsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "calibration")
	c := NewUsageCalibrationRecorder(dir)
	lock, err := tryLockAccountImportTransaction(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.write([]byte("{}\n")); err == nil {
		t.Fatal("second writer bypassed cross-process lock")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "observations.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(usageCalibrationFileMaxBytes); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if err := c.write([]byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + ".1"} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > usageCalibrationFileMaxBytes {
			t.Fatal("segment cap exceeded")
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("public permissions on %s", p)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.queue <- []byte("{}\n")
	cancel()
	c.Run(ctx)
	if len(c.queue) != 0 {
		t.Fatal("shutdown did not drain queue")
	}
}
