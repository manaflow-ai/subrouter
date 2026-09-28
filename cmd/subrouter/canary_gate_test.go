package main

import (
	"strings"
	"testing"
	"time"
)

func TestEvaluateCanaryGate(t *testing.T) {
	window := func(requests, all5xx, proxy5xx, drops uint64) *trafficWindow {
		return &trafficWindow{Requests: requests, Responses5xx: all5xx, Proxy5xx: proxy5xx, StreamProxyDrops: drops}
	}
	dwell := 10 * time.Minute
	cases := []struct {
		name       string
		input      gateInput
		want       gateAction
		wantReason string
	}{
		{"quiet step holds", gateInput{Candidate: window(3, 0, 0, 0), Incumbent: window(100, 0, 0, 0), Elapsed: time.Minute, Dwell: dwell}, gateHold, "3 candidate requests"},
		{"quiet step promotes on health alone", gateInput{Candidate: window(3, 2, 2, 0), Incumbent: window(100, 0, 0, 0), Elapsed: dwell, Dwell: dwell}, gatePromote, "under the 50-request floor"},
		{"no counters promotes on health alone", gateInput{Elapsed: dwell, Dwell: dwell}, gatePromote, "no traffic counters"},
		{"proxy 5xx regression aborts early", gateInput{Candidate: window(500, 16, 16, 0), Incumbent: window(1000, 3, 3, 0), Elapsed: time.Minute, Dwell: dwell}, gateAbort, "proxy 5xx 3.2% vs 0.3%"},
		{"stream drop regression aborts", gateInput{Candidate: window(100, 0, 0, 10), Incumbent: window(100, 0, 0, 1), Elapsed: dwell, Dwell: dwell}, gateAbort, "proxy stream drops 10.0% vs 1.0%"},
		{"all 5xx regression aborts", gateInput{Candidate: window(100, 20, 0, 0), Incumbent: window(100, 5, 0, 0), Elapsed: dwell, Dwell: dwell}, gateAbort, "all 5xx 20.0% vs 5.0%"},
		{"under the error floor never trips", gateInput{Candidate: window(60, 4, 4, 4), Incumbent: window(1000, 0, 0, 0), Elapsed: dwell, Dwell: dwell}, gatePromote, "60 candidate requests"},
		{"under the request floor never trips", gateInput{Candidate: window(49, 49, 49, 0), Incumbent: window(1000, 0, 0, 0), Elapsed: dwell, Dwell: dwell}, gatePromote, "under the 50-request floor"},
		{"within factor of a noisy incumbent", gateInput{Candidate: window(100, 0, 15, 0), Incumbent: window(100, 0, 10, 0), Elapsed: dwell, Dwell: dwell}, gatePromote, "proxy 5xx 15"},
		{"within margin of a clean incumbent", gateInput{Candidate: window(1000, 0, 6, 0), Incumbent: window(1000, 0, 0, 0), Elapsed: dwell, Dwell: dwell}, gatePromote, "1000 candidate requests"},
		{"shared upstream incident does not trip", gateInput{Candidate: window(200, 40, 40, 0), Incumbent: window(2000, 400, 400, 0), Elapsed: dwell, Dwell: dwell}, gatePromote, "200 candidate requests"},
		{"a candidate failing readiness aborts", gateInput{CandidateHealthFailures: 3, CandidateHealthError: "context deadline exceeded", Incumbent: window(100, 0, 0, 0), Elapsed: dwell, Dwell: dwell}, gateAbort, "candidate failed 3 consecutive readiness checks: context deadline exceeded"},
		{"two readiness failures only hold", gateInput{CandidateHealthFailures: 2, Candidate: window(3, 0, 0, 0), Elapsed: time.Minute, Dwell: dwell}, gateHold, "3 candidate requests"},
		{"missing incumbent is a zero baseline", gateInput{Candidate: window(100, 0, 10, 0), Elapsed: dwell, Dwell: dwell}, gateAbort, "proxy 5xx 10.0% vs 0.0%"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evaluateCanaryGate(tc.input, defaultGateThresholds())
			if got.Action != tc.want || !strings.Contains(got.Reason, tc.wantReason) {
				t.Fatalf("gate = %+v, want %s containing %q", got, tc.want, tc.wantReason)
			}
		})
	}
}

func TestGateThresholdsFromEnvUsesBakeOverrides(t *testing.T) {
	env := map[string]string{
		"SUBROUTER_BAKE_MIN_REQUESTS":     "10",
		"SUBROUTER_BAKE_RATIO_FACTOR":     "3",
		"SUBROUTER_BAKE_PROXY_5XX_MARGIN": "bad",
	}
	got := gateThresholdsFromEnv(func(name string) string { return env[name] })
	want := defaultGateThresholds()
	want.MinRequests = 10
	want.Factor = 3
	if got != want {
		t.Fatalf("thresholds = %+v, want %+v", got, want)
	}
}

func TestTrafficWindowSinceTreatsLowerCountsAsRestart(t *testing.T) {
	base := trafficWindow{Requests: 100, Proxy5xx: 5}
	if got := (trafficWindow{Requests: 150, Proxy5xx: 6}).since(base); got != (trafficWindow{Requests: 50, Proxy5xx: 1}) {
		t.Fatalf("delta = %+v", got)
	}
	if got := (trafficWindow{Requests: 20}).since(base); got != (trafficWindow{Requests: 20}) {
		t.Fatalf("restart delta = %+v", got)
	}
}
