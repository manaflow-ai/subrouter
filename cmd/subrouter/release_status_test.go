package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/buildversion"
)

func TestReleaseStatusText(t *testing.T) {
	now := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		release *releaseStateView
		want    string
	}{
		{nil, ""},
		{&releaseStateView{}, ""},
		{&releaseStateView{Version: "v0.1.150", State: "baking", BakeUntil: now.Add(12 * time.Minute).Format(time.RFC3339)}, "v0.1.150 baking (12m left)"},
		{&releaseStateView{Version: "v0.1.150", State: "baking", BakeUntil: now.Add(-time.Minute).Format(time.RFC3339)}, "v0.1.150 baking (window over; promoted on the next guard check)"},
		{&releaseStateView{Version: "v0.1.150", State: "promoted"}, "v0.1.150 promoted"},
		{&releaseStateView{Version: "v0.1.150", PreviousVersion: "v0.1.149", State: "rolled_back", Reason: "proxy 5xx 4.1% vs 0.2% baseline"}, "rolled back from v0.1.150 to v0.1.149: proxy 5xx 4.1% vs 0.2% baseline"},
	}
	for _, tc := range cases {
		if got := releaseStatusText(tc.release, now); got != tc.want {
			t.Errorf("releaseStatusText(%+v) = %q, want %q", tc.release, got, tc.want)
		}
	}
}

func TestDoctorWarnsOnRolledBackRelease(t *testing.T) {
	noTeamPlists(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"version": buildversion.Version(),
			"release": map[string]any{
				"version":          "v0.1.150",
				"previous_version": "v0.1.149",
				"state":            "rolled_back",
				"reason":           "proxy 5xx 4.1% vs 0.2% baseline",
			},
		})
	}))
	t.Cleanup(server.Close)
	out := runDoctorForTest(t, server.URL)
	if !strings.Contains(out, "rolled back from v0.1.150 to v0.1.149: proxy 5xx 4.1% vs 0.2% baseline") {
		t.Fatalf("doctor did not report the rollback:\n%s", out)
	}
}

func TestDoctorOmitsReleaseForLocalDaemons(t *testing.T) {
	noTeamPlists(t)
	server := versionHealthServer(t, buildversion.Version())
	out := runDoctorForTest(t, server.URL)
	if strings.Contains(out, "release") {
		t.Fatalf("doctor printed a release line for a daemon without one:\n%s", out)
	}
}
