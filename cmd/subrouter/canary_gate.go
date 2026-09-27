package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// The canary gate compares the candidate worker generation with the
// incumbent over the same window. It uses the thresholds and minimum-sample
// rules of the post-upgrade bake gate in deploy/macos/release-bake-lib.sh,
// and reads the same SUBROUTER_BAKE_* overrides. Unlike the bake, the
// comparison is against a generation serving at the same time, so an
// upstream incident that hits both sides equally cannot trip it.
//
// The bake's restart rule has no counterpart here: a supervised candidate is
// one process, and the supervisor aborts the rollout the moment it exits.

type gateAction string

const (
	gatePromote gateAction = "promote"
	gateHold    gateAction = "hold"
	gateAbort   gateAction = "abort"
)

type gateDecision struct {
	Action gateAction `json:"action"`
	Reason string     `json:"reason"`
}

// trafficWindow is the outcome counts one generation served in a window.
type trafficWindow struct {
	Requests         uint64 `json:"requests"`
	Responses5xx     uint64 `json:"responses_5xx"`
	Proxy5xx         uint64 `json:"proxy_5xx"`
	StreamProxyDrops uint64 `json:"stream_proxy_drops"`
}

func trafficWindowOf(snapshot proxy.TrafficSnapshot) trafficWindow {
	return trafficWindow{
		Requests:         snapshot.Requests,
		Responses5xx:     snapshot.Responses.Class5xx,
		Proxy5xx:         snapshot.Proxy5xx,
		StreamProxyDrops: snapshot.Streams.Proxy,
	}
}

// since returns the counts served after base. A counter lower than base means
// the process restarted, so the whole current count is the window.
func (w trafficWindow) since(base trafficWindow) trafficWindow {
	if w.Requests < base.Requests || w.Responses5xx < base.Responses5xx ||
		w.Proxy5xx < base.Proxy5xx || w.StreamProxyDrops < base.StreamProxyDrops {
		return w
	}
	return trafficWindow{
		Requests:         w.Requests - base.Requests,
		Responses5xx:     w.Responses5xx - base.Responses5xx,
		Proxy5xx:         w.Proxy5xx - base.Proxy5xx,
		StreamProxyDrops: w.StreamProxyDrops - base.StreamProxyDrops,
	}
}

type gateThresholds struct {
	Factor           float64
	Proxy5xxMargin   float64
	StreamDropMargin float64
	All5xxMargin     float64
	MinRequests      uint64
	MinErrors        uint64
}

// defaultGateThresholds mirrors release-bake-lib.sh's defaults.
func defaultGateThresholds() gateThresholds {
	return gateThresholds{
		Factor:           2,
		Proxy5xxMargin:   0.02,
		StreamDropMargin: 0.02,
		All5xxMargin:     0.05,
		MinRequests:      50,
		MinErrors:        5,
	}
}

// gateThresholdsFromEnv applies the bake gate's SUBROUTER_BAKE_* overrides.
func gateThresholdsFromEnv(getenv func(string) string) gateThresholds {
	thresholds := defaultGateThresholds()
	float := func(name string, target *float64) {
		if value, err := strconv.ParseFloat(strings.TrimSpace(getenv(name)), 64); err == nil && value >= 0 {
			*target = value
		}
	}
	count := func(name string, target *uint64) {
		if value, err := strconv.ParseUint(strings.TrimSpace(getenv(name)), 10, 64); err == nil {
			*target = value
		}
	}
	float("SUBROUTER_BAKE_RATIO_FACTOR", &thresholds.Factor)
	float("SUBROUTER_BAKE_PROXY_5XX_MARGIN", &thresholds.Proxy5xxMargin)
	float("SUBROUTER_BAKE_STREAM_DROP_MARGIN", &thresholds.StreamDropMargin)
	float("SUBROUTER_BAKE_5XX_MARGIN", &thresholds.All5xxMargin)
	count("SUBROUTER_BAKE_MIN_REQUESTS", &thresholds.MinRequests)
	count("SUBROUTER_BAKE_MIN_ERRORS", &thresholds.MinErrors)
	return thresholds
}

var processGateThresholds = func() gateThresholds { return gateThresholdsFromEnv(os.Getenv) }

type gateInput struct {
	// Candidate is nil when the candidate reported no traffic counters.
	Candidate *trafficWindow
	// Incumbent is nil when the incumbent reported none; the baseline is
	// then zero, which only makes the margin rule stricter to trip.
	Incumbent *trafficWindow
	// Elapsed is how long the current step has run; Dwell is its length.
	Elapsed time.Duration
	Dwell   time.Duration
}

// evaluateCanaryGate decides one step. A regression aborts at once, even
// before the dwell ends. Otherwise the step holds until the dwell passes and
// then promotes; a step with too few requests for a ratio promotes on health
// alone and says so.
func evaluateCanaryGate(input gateInput, thresholds gateThresholds) gateDecision {
	if input.Candidate != nil && input.Candidate.Requests >= thresholds.MinRequests && input.Candidate.Requests > 0 {
		candidate := *input.Candidate
		var incumbent trafficWindow
		if input.Incumbent != nil {
			incumbent = *input.Incumbent
		}
		checks := []struct {
			label              string
			failures, baseline uint64
			margin             float64
		}{
			{"proxy 5xx", candidate.Proxy5xx, incumbent.Proxy5xx, thresholds.Proxy5xxMargin},
			{"proxy stream drops", candidate.StreamProxyDrops, incumbent.StreamProxyDrops, thresholds.StreamDropMargin},
			{"all 5xx", candidate.Responses5xx, incumbent.Responses5xx, thresholds.All5xxMargin},
		}
		for _, check := range checks {
			if check.failures < thresholds.MinErrors {
				continue
			}
			ratio := float64(check.failures) / float64(candidate.Requests)
			base := 0.0
			if incumbent.Requests > 0 {
				base = float64(check.baseline) / float64(incumbent.Requests)
			}
			if ratio > base*thresholds.Factor && ratio > base+check.margin {
				return gateDecision{gateAbort, fmt.Sprintf("%s %s vs %s (%d of %d requests; incumbent %d of %d)",
					check.label, gatePercent(ratio), gatePercent(base),
					check.failures, candidate.Requests, check.baseline, incumbent.Requests)}
			}
		}
	}
	if input.Elapsed < input.Dwell {
		requests := uint64(0)
		if input.Candidate != nil {
			requests = input.Candidate.Requests
		}
		return gateDecision{gateHold, fmt.Sprintf("%d candidate requests, %s left", requests, (input.Dwell - input.Elapsed).Round(time.Second))}
	}
	if input.Candidate == nil {
		return gateDecision{gatePromote, "dwell passed; the candidate reported no traffic counters, so only health was checked"}
	}
	if input.Candidate.Requests < thresholds.MinRequests {
		return gateDecision{gatePromote, fmt.Sprintf("dwell passed with %d candidate requests, under the %d-request floor; only health and restarts were checked",
			input.Candidate.Requests, thresholds.MinRequests)}
	}
	return gateDecision{gatePromote, fmt.Sprintf("%d candidate requests: proxy 5xx %d, all 5xx %d, proxy stream drops %d",
		input.Candidate.Requests, input.Candidate.Proxy5xx, input.Candidate.Responses5xx, input.Candidate.StreamProxyDrops)}
}

func gatePercent(value float64) string {
	return fmt.Sprintf("%.1f%%", value*100)
}
