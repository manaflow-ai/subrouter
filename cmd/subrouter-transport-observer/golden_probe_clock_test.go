package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// goldenScheduledProbeClock stamps each probe with its scheduled time,
// start + n*goldenProbeInterval, and delivers every tick once wall-clock time
// reaches it instead of dropping ticks a busy receiver missed. A stalled
// scheduler delays probes but cannot open a gap in their evidence, while the
// schedule stays anchored to the real interval the runner validates.
type goldenScheduledProbeClock struct{}

func (goldenScheduledProbeClock) now() time.Time { return time.Now().UTC() }

func (goldenScheduledProbeClock) ticks(start time.Time) (<-chan time.Time, func()) {
	ticks := make(chan time.Time)
	stop := make(chan struct{})
	go func() {
		timer := time.NewTimer(0)
		defer timer.Stop()
		<-timer.C
		for index := 1; ; index++ {
			tick := start.Add(time.Duration(index) * goldenProbeInterval)
			timer.Reset(time.Until(tick))
			select {
			case <-stop:
				return
			case <-timer.C:
			}
			select {
			case <-stop:
				return
			case ticks <- tick:
			}
		}
	}()
	var once sync.Once
	return ticks, func() { once.Do(func() { close(stop) }) }
}

func (goldenScheduledProbeClock) probeStart(tick time.Time) time.Time {
	if tick.IsZero() {
		return time.Now().UTC()
	}
	return tick
}

func (goldenScheduledProbeClock) scheduledThrough(start, cutoff time.Time) (int, bool) {
	if cutoff.Before(start) {
		return 0, true
	}
	return int(cutoff.Sub(start)/goldenProbeInterval) + 1, true
}

// goldenManualProbeClock is virtual time the test advances explicitly: no
// probe tick or timestamp depends on the wall clock.
type goldenManualProbeClock struct {
	mu         sync.Mutex
	current    time.Time
	loops      []chan time.Time
	registered chan struct{}
}

func newGoldenManualProbeClock(start time.Time) *goldenManualProbeClock {
	return &goldenManualProbeClock{current: start, registered: make(chan struct{}, 16)}
}

func (c *goldenManualProbeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func (c *goldenManualProbeClock) ticks(time.Time) (<-chan time.Time, func()) {
	ticks := make(chan time.Time)
	c.mu.Lock()
	c.loops = append(c.loops, ticks)
	c.mu.Unlock()
	c.registered <- struct{}{}
	return ticks, func() {}
}

func (c *goldenManualProbeClock) probeStart(tick time.Time) time.Time {
	if tick.IsZero() {
		return c.now()
	}
	return tick
}

func (c *goldenManualProbeClock) scheduledThrough(start, cutoff time.Time) (int, bool) {
	return goldenScheduledProbeClock{}.scheduledThrough(start, cutoff)
}

// advance moves virtual time one probe interval and hands the tick to every
// probe loop, returning once each loop has received it.
func (c *goldenManualProbeClock) advance(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	c.current = c.current.Add(goldenProbeInterval)
	tick := c.current
	loops := append([]chan time.Time(nil), c.loops...)
	c.mu.Unlock()
	for _, loop := range loops {
		select {
		case loop <- tick:
		case <-time.After(time.Minute):
			t.Fatal("probe loop did not accept its tick")
		}
	}
}

func TestGoldenProbeStopRecordsEveryVirtualTickBeforeCancelling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	origin, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	clock := newGoldenManualProbeClock(start)
	previous := goldenTestHooks
	goldenTestHooks.enabled = true
	goldenTestHooks.probeClock = clock
	t.Cleanup(func() { goldenTestHooks = previous })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stats, err := (&goldenRunner{artifactDir: t.TempDir()}).startProbes(ctx, origin, origin)
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		<-clock.registered
	}
	const ticks = 20
	for range ticks {
		clock.advance(t)
	}
	// No probe launched by the final ticks has necessarily finished; stop
	// must wait for all of them rather than cancel them away.
	stats.stop(cancel)

	for _, summary := range stats.summaries() {
		if summary.Samples != ticks+1 || summary.Failures != 0 || summary.MaxStartGapMillis != goldenProbeInterval.Milliseconds() {
			t.Fatalf("summary = %+v, want %d samples 100ms apart with no failures", summary, ticks+1)
		}
		if goldenProbeSummaryGapExceeded(summary) {
			t.Fatalf("summary %+v exceeds the production start-gap cap", summary)
		}
	}
	if err := stats.validateInterval(start, clock.now()); err != nil {
		t.Fatalf("validateInterval = %v, want production cadence satisfied", err)
	}
}

func TestGoldenProbeSummaryGapCapFollowsScheduleTolerance(t *testing.T) {
	previous := goldenTestHooks
	t.Cleanup(func() { goldenTestHooks = previous })
	summary := func(gap time.Duration) goldenProbeSummary {
		return goldenProbeSummary{Label: "public-ready", Samples: 2, MaxStartGapMillis: gap.Milliseconds()}
	}

	goldenTestHooks.enabled = false
	goldenTestHooks.probeScheduleTolerance = time.Second
	if goldenProbeSummaryGapExceeded(summary(250 * time.Millisecond)) {
		t.Fatal("production rejected a 250ms start gap")
	}
	if !goldenProbeSummaryGapExceeded(summary(251 * time.Millisecond)) {
		t.Fatal("production accepted a 251ms start gap; the hook must be inert outside test mode")
	}

	goldenTestHooks.enabled = true
	goldenTestHooks.probeScheduleTolerance = 0
	if !goldenProbeSummaryGapExceeded(summary(251 * time.Millisecond)) {
		t.Fatal("test mode without a tolerance hook accepted a 251ms start gap")
	}

	goldenTestHooks.probeScheduleTolerance = time.Second
	if goldenProbeSummaryGapExceeded(summary(goldenProbeInterval + time.Second)) {
		t.Fatal("summary cap rejected a gap validateInterval accepts under the same tolerance")
	}
	if !goldenProbeSummaryGapExceeded(summary(goldenProbeInterval + time.Second + time.Millisecond)) {
		t.Fatal("summary cap accepted a gap beyond interval plus tolerance")
	}

	goldenTestHooks.probeScheduleTolerance = 10 * time.Millisecond
	if goldenProbeSummaryGapExceeded(summary(250 * time.Millisecond)) {
		t.Fatal("a narrow tolerance hook tightened the summary cap below production")
	}
}
