package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCodexSheddingTrackerEntersAndLeavesWithHysteresis(t *testing.T) {
	tracker := newCodexSheddingTracker()
	now := time.Now()
	for i := range codexSheddingMinSamples - 1 {
		tracker.record("gpt-6-astra", "priority", true, now.Add(time.Duration(i)*time.Millisecond))
	}
	if tracker.shedding("gpt-6-astra", "priority", now) {
		t.Fatal("shedding declared below the minimum sample count")
	}
	tracker.record("gpt-6-astra", "priority", true, now)
	if !tracker.shedding("gpt-6-astra", "priority", now) {
		t.Fatal("all-failure window not declared shedding")
	}
	if tracker.shedding("gpt-6-astra", "", now) || tracker.shedding("gpt-6-other", "priority", now) {
		t.Fatal("shedding leaked to another (model, tier)")
	}
	states := tracker.snapshot(now)
	if len(states) != 1 || !states[0].Shedding || states[0].Model != "gpt-6-astra" || states[0].Tier != "priority" ||
		states[0].FailureRatio != 1 || states[0].Since.IsZero() {
		t.Fatalf("snapshot = %+v", states)
	}
	// Successes bring the ratio down; shedding holds until it drops below
	// the exit threshold, not merely the entry one.
	for range codexSheddingMinSamples - 2 {
		tracker.record("gpt-6-astra", "priority", false, now)
	}
	if !tracker.shedding("gpt-6-astra", "priority", now) {
		t.Fatal("left shedding while the failure ratio was still above the exit threshold")
	}
	for range 3 * codexSheddingMinSamples {
		tracker.record("gpt-6-astra", "priority", false, now)
	}
	if tracker.shedding("gpt-6-astra", "priority", now) {
		t.Fatal("still shedding after the pool recovered")
	}
	// Old outcomes age out of the window.
	later := now.Add(codexSheddingWindow + time.Second)
	if states := tracker.snapshot(later); len(states) != 0 {
		t.Fatalf("snapshot after the window = %+v, want nothing", states)
	}
}

// While the model sheds pool-wide, the default policy shrinks its budget so
// retries do not amplify the overload; persist mode keeps its own budget.
func TestCodexCapacitySheddingShrinksDefaultBudgetOnly(t *testing.T) {
	poolURL, seen := codexCapacityPool(t, func(string, int) bool { return true })
	server := codexOverloadServer(t, poolURL, 8, true)
	server.CodexOverloadFailover.MaxAccounts = 7
	fastCapacityGaps(server.CodexOverloadFailover, 1200*time.Millisecond)
	server.codexShedding = newCodexSheddingTracker()
	now := time.Now()
	for range 2 * codexSheddingMinSamples {
		server.codexShedding.record("gpt-6-astra", "", true, now)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	started := time.Now()
	if _, _, err := codexCapacityPost(context.Background(), t, proxy.URL, "session-shed", "default", nil); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	if elapsed > 3500*time.Millisecond {
		t.Fatalf("default retry ran %v while shedding, want the ~3s shrunk budget", elapsed)
	}
	if n := len(seen()); n != 3 {
		t.Fatalf("pool saw %d attempts while shedding, want 3 inside 3s with 1.2s gaps", n)
	}

	started = time.Now()
	if _, _, err := codexCapacityPost(context.Background(), t, proxy.URL, "session-shed-persist", "persist", map[string]string{
		CodexCapacityRetryHeader:       "persist",
		CodexCapacityRetryBudgetHeader: "5s",
	}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 3800*time.Millisecond {
		t.Fatalf("persist request ran %v while shedding, want its own 5s budget", elapsed)
	}
}

// Every capacity outcome of the failover feeds the tracker, and the state
// shows up in /_subrouter/health.
func TestCodexCapacitySheddingIsExposedInHealth(t *testing.T) {
	poolURL, _ := codexCapacityPool(t, func(string, int) bool { return true })
	server := codexOverloadServer(t, poolURL, 3, true)
	fastCapacityGaps(server.CodexOverloadFailover, time.Millisecond)
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()
	for i := range 4 {
		if _, _, err := codexCapacityPost(context.Background(), t, proxy.URL, "session-health-"+string(rune('a'+i)), "a", nil); err != nil {
			t.Fatal(err)
		}
	}
	response, err := http.Get(proxy.URL + "/_subrouter/health")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var health struct {
		Shedding []CodexSheddingState `json:"codex_capacity_shedding"`
	}
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if len(health.Shedding) != 1 {
		t.Fatalf("health shedding = %+v, want one pool", health.Shedding)
	}
	state := health.Shedding[0]
	if !state.Shedding || state.Model != "gpt-6-astra" || state.Tier != "default" || state.FailureRatio != 1 ||
		state.Samples < codexSheddingMinSamples || state.Since.IsZero() || !strings.Contains(state.RetryBudget, "3s") {
		t.Fatalf("health shedding state = %+v", state)
	}
}
