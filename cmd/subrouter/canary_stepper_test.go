package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// autoClock advances by exactly the waited duration each time the stepper
// waits, so a 30-minute rollout runs instantly and deterministically.
type autoClock struct{ now time.Time }

func (c *autoClock) Now() time.Time { return c.now }
func (c *autoClock) After(d time.Duration) <-chan time.Time {
	c.now = c.now.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.now
	return ch
}

type fakeRolloutDriver struct {
	clock        *autoClock
	weight       int
	weights      []int
	observeFn    func(weight int, now time.Time) gateInput
	decisions    []gateDecision
	promoted     string
	promotedAt   time.Time
	aborted      string
	refusePromos int
}

func (d *fakeRolloutDriver) observe() (gateInput, bool) {
	return d.observeFn(d.weight, d.clock.Now()), true
}

func (d *fakeRolloutDriver) setWeight(weight, _ int) error {
	d.weight = weight
	d.weights = append(d.weights, weight)
	return nil
}

func (d *fakeRolloutDriver) record(_ int, decision gateDecision) {
	d.decisions = append(d.decisions, decision)
}

func (d *fakeRolloutDriver) promote(reason string) error {
	if d.refusePromos > 0 {
		d.refusePromos--
		return errors.New("inhibited")
	}
	d.promoted = reason
	d.promotedAt = d.clock.Now()
	return nil
}

func (d *fakeRolloutDriver) abort(reason string) error {
	d.aborted = reason
	return nil
}

func healthyWindows(weight int, _ time.Time) gateInput {
	requests := uint64(weight * 10)
	return gateInput{
		Candidate: &trafficWindow{Requests: requests},
		Incumbent: &trafficWindow{Requests: 1000, Proxy5xx: 1},
	}
}

func newTestStepper(driver *fakeRolloutDriver) canaryStepper {
	return canaryStepper{
		driver:     driver,
		steps:      []int{5, 25, 100},
		dwell:      10 * time.Minute,
		tick:       time.Minute,
		clock:      driver.clock,
		thresholds: defaultGateThresholds(),
	}
}

func TestCanaryStepperPromotesThroughEveryStep(t *testing.T) {
	start := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	clock := &autoClock{now: start}
	driver := &fakeRolloutDriver{clock: clock, weight: 5, observeFn: healthyWindows}
	newTestStepper(driver).run(context.Background())

	if !reflect.DeepEqual(driver.weights, []int{25, 100}) {
		t.Fatalf("weights = %v, want [25 100]", driver.weights)
	}
	if driver.aborted != "" || !strings.Contains(driver.promoted, "passed 3 steps") {
		t.Fatalf("promoted %q, aborted %q", driver.promoted, driver.aborted)
	}
	if got := driver.promotedAt.Sub(start); got != 30*time.Minute {
		t.Fatalf("promoted after %s, want 30m (three 10m dwells)", got)
	}
	holds := 0
	for _, decision := range driver.decisions {
		if decision.Action == gateHold {
			holds++
		}
	}
	if holds != 27 {
		t.Fatalf("holds = %d, want 27 (9 per step)", holds)
	}
}

func TestCanaryStepperAbortsOnRegressionMidStep(t *testing.T) {
	start := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	clock := &autoClock{now: start}
	driver := &fakeRolloutDriver{clock: clock, weight: 5}
	driver.observeFn = func(weight int, now time.Time) gateInput {
		if weight < 25 || now.Sub(start) < 13*time.Minute {
			return healthyWindows(weight, now)
		}
		return gateInput{
			Candidate: &trafficWindow{Requests: 500, Responses5xx: 16, Proxy5xx: 16},
			Incumbent: &trafficWindow{Requests: 1000, Responses5xx: 3, Proxy5xx: 3},
		}
	}
	newTestStepper(driver).run(context.Background())

	if driver.promoted != "" {
		t.Fatalf("regressed candidate promoted: %q", driver.promoted)
	}
	if !strings.Contains(driver.aborted, "proxy 5xx 3.2% vs 0.3%") {
		t.Fatalf("abort reason = %q", driver.aborted)
	}
	if !reflect.DeepEqual(driver.weights, []int{25}) {
		t.Fatalf("weights = %v, want [25]", driver.weights)
	}
	if got := clock.Now().Sub(start); got != 13*time.Minute {
		t.Fatalf("aborted after %s, want 13m (the first bad check, not the end of the dwell)", got)
	}
}

func TestCanaryStepperStopsWhenCancelled(t *testing.T) {
	clock := &autoClock{now: time.Unix(0, 0)}
	driver := &fakeRolloutDriver{clock: clock, weight: 5, observeFn: healthyWindows}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	newTestStepper(driver).run(ctx)
	if len(driver.decisions) != 0 || driver.promoted != "" || driver.aborted != "" {
		t.Fatalf("cancelled stepper acted: %+v", driver)
	}
}

func TestCanaryStepperRetriesRefusedPromotion(t *testing.T) {
	start := time.Unix(0, 0)
	clock := &autoClock{now: start}
	driver := &fakeRolloutDriver{clock: clock, weight: 5, observeFn: healthyWindows, refusePromos: 2}
	newTestStepper(driver).run(context.Background())
	if driver.promoted == "" {
		t.Fatal("promotion was not retried")
	}
	if got := driver.promotedAt.Sub(start); got != 32*time.Minute {
		t.Fatalf("promoted after %s, want 32m", got)
	}
}

func TestCanaryTick(t *testing.T) {
	for dwell, want := range map[time.Duration]time.Duration{
		10 * time.Minute:       time.Minute / 2,
		time.Hour:              30 * time.Second,
		time.Second:            100 * time.Millisecond,
		10 * time.Millisecond:  10 * time.Millisecond,
		100 * time.Microsecond: 10 * time.Millisecond,
	} {
		if got := canaryTick(dwell); got != want {
			t.Errorf("canaryTick(%s) = %s, want %s", dwell, got, want)
		}
	}
}

func TestParseCanarySteps(t *testing.T) {
	if steps, err := parseCanarySteps("default"); err != nil || !reflect.DeepEqual(steps, []int{5, 25, 100}) {
		t.Fatalf("default = %v, %v", steps, err)
	}
	if steps, err := parseCanarySteps("10%, 50,100"); err != nil || !reflect.DeepEqual(steps, []int{10, 50, 100}) {
		t.Fatalf("list = %v, %v", steps, err)
	}
	for _, bad := range []string{"0,50", "50,25", "5,101", "x"} {
		if _, err := parseCanarySteps(bad); err == nil {
			t.Errorf("parseCanarySteps(%q) accepted", bad)
		}
	}
}
