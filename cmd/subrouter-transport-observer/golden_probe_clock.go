package main

import "time"

// goldenProbeSummaryMaxStartGap is the largest start-to-start gap a probe
// label's run summary may report.
const goldenProbeSummaryMaxStartGap = 250 * time.Millisecond

// goldenProbeClock schedules the 10 Hz health prober and stamps its probes.
// Production uses goldenRealProbeClock. Synthetic harness tests inject a
// clock through goldenTestHooks.probeClock so probe cadence and timestamps do
// not depend on how promptly a loaded host runs the prober's goroutines.
type goldenProbeClock interface {
	now() time.Time
	// ticks delivers the scheduled time of each probe after the first, which
	// runs at start, until the returned stop function is called.
	ticks(start time.Time) (<-chan time.Time, func())
	// probeStart is the start time recorded for a probe scheduled at tick.
	probeStart(tick time.Time) time.Time
	// scheduledThrough reports how many probes each loop has been scheduled
	// at or before cutoff, counting the one at start. A clock that cannot
	// promise delivery of every scheduled tick returns false, and stopping the
	// prober then does not wait for the schedule to be recorded.
	scheduledThrough(start, cutoff time.Time) (int, bool)
}

// goldenRealProbeClock is wall-clock time. A time.Ticker drops ticks for a
// slow receiver, and each probe records the moment it actually started, so
// scheduler stalls surface as gaps in the evidence.
type goldenRealProbeClock struct{}

func (goldenRealProbeClock) now() time.Time { return time.Now().UTC() }

func (goldenRealProbeClock) ticks(time.Time) (<-chan time.Time, func()) {
	ticker := time.NewTicker(goldenProbeInterval)
	return ticker.C, ticker.Stop
}

func (goldenRealProbeClock) probeStart(time.Time) time.Time { return time.Now().UTC() }

func (goldenRealProbeClock) scheduledThrough(time.Time, time.Time) (int, bool) { return 0, false }

func goldenProbeClockForRun() goldenProbeClock {
	if goldenTestHooks.enabled && goldenTestHooks.probeClock != nil {
		return goldenTestHooks.probeClock
	}
	return goldenRealProbeClock{}
}

func goldenProbeHTTPTimeoutForRun() time.Duration {
	if goldenTestHooks.enabled && goldenTestHooks.probeHTTPTimeout > 0 {
		return goldenTestHooks.probeHTTPTimeout
	}
	return goldenHTTPTimeout
}

// goldenProbeSummaryMaxStartGapForRun keeps the summary cap consistent with
// validateInterval: a test run that widened the schedule tolerance accepts the
// same interval-plus-tolerance gap here, and never less than production allows.
func goldenProbeSummaryMaxStartGapForRun() time.Duration {
	if goldenTestHooks.enabled && goldenTestHooks.probeScheduleTolerance > 0 {
		if widened := goldenProbeInterval + goldenTestHooks.probeScheduleTolerance; widened > goldenProbeSummaryMaxStartGap {
			return widened
		}
	}
	return goldenProbeSummaryMaxStartGap
}

func goldenProbeSummaryGapExceeded(summary goldenProbeSummary) bool {
	return time.Duration(summary.MaxStartGapMillis)*time.Millisecond > goldenProbeSummaryMaxStartGapForRun()
}
