package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// defaultCanarySteps and defaultCanaryDwell are the rollout a candidate
// started with steps=default follows.
var defaultCanarySteps = []int{5, 25, 100}

const defaultCanaryDwell = 10 * time.Minute

type rolloutClock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

type realRolloutClock struct{}

func (realRolloutClock) Now() time.Time                         { return time.Now() }
func (realRolloutClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// rolloutDriver is what the stepper needs from the supervisor. Every call
// acts only on the rollout the stepper was started for; once that rollout is
// promoted, aborted or replaced, the calls fail and the stepper stops.
type rolloutDriver interface {
	// observe returns the gate input for the rollout window so far, with
	// Elapsed and Dwell unset. ok is false when the rollout is over.
	observe() (input gateInput, ok bool)
	setWeight(weight, step int) error
	record(step int, decision gateDecision)
	promote(reason string) error
	abort(reason string) error
}

// canaryStepper walks a candidate through its steps. At each step it sets
// the weight, then checks the gate every tick: an abort ends the rollout at
// once, a hold waits, and a promote moves to the next step, or, after the
// last step, makes the candidate the sole generation.
type canaryStepper struct {
	driver     rolloutDriver
	steps      []int
	dwell      time.Duration
	tick       time.Duration
	clock      rolloutClock
	thresholds gateThresholds
}

// canaryTick checks about ten times per step, at most every 30s.
func canaryTick(dwell time.Duration) time.Duration {
	tick := dwell / 10
	if tick > 30*time.Second {
		tick = 30 * time.Second
	}
	if tick < 10*time.Millisecond {
		tick = 10 * time.Millisecond
	}
	return tick
}

func (st canaryStepper) run(ctx context.Context) {
	for index, weight := range st.steps {
		if index > 0 {
			if err := st.driver.setWeight(weight, index); err != nil {
				slog.Info("canary stepper stopped", "reason", err)
				return
			}
		}
		stepStart := st.clock.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-st.clock.After(st.tick):
			}
			if ctx.Err() != nil {
				return
			}
			input, ok := st.driver.observe()
			if !ok {
				return
			}
			input.Elapsed = st.clock.Now().Sub(stepStart)
			input.Dwell = st.dwell
			decision := evaluateCanaryGate(input, st.thresholds)
			st.driver.record(index, decision)
			if decision.Action == gateAbort {
				if err := st.driver.abort(decision.Reason); err != nil {
					slog.Warn("canary stepper abort failed", "error", err)
				}
				return
			}
			if decision.Action != gatePromote {
				continue
			}
			if index < len(st.steps)-1 {
				break
			}
			if err := st.driver.promote(fmt.Sprintf("passed %d steps: %s", len(st.steps), decision.Reason)); err != nil {
				// An inhibit marker blocks promotion; keep checking so the
				// rollout promotes once it is lifted.
				slog.Warn("canary promotion refused; retrying", "error", err)
				continue
			}
			return
		}
	}
}
