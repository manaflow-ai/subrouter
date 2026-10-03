package proxy

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/wake"
)

func TestRecoveryTrackerSeparatesKindsAndRecordsActivity(t *testing.T) {
	tracker := NewRecoveryTracker()
	now := time.Now().UTC()
	tracker.RecordCapacityFailure("codex", "s1", "gpt", now)
	tracker.RecordQuotaFailure("claude", "s2", wake.KindClaudeQuota, "", now, now.Add(time.Hour))
	states := tracker.List(now)
	if len(states) != 2 {
		t.Fatalf("states=%d, want 2", len(states))
	}
	for _, state := range states {
		if state.Agent == "codex" && state.Kind != wake.KindCodexProvider {
			t.Fatalf("codex kind=%q", state.Kind)
		}
		if state.Agent == "claude" && (state.Kind != wake.KindClaudeQuota || state.ResetAt.IsZero()) {
			t.Fatalf("claude state=%+v", state)
		}
		if state.LastActivityAt.IsZero() {
			t.Fatalf("missing activity in %+v", state)
		}
	}
}

func TestRecoveryTrackerRecordsReplayOutcome(t *testing.T) {
	tracker := NewRecoveryTracker()
	now := time.Now().UTC()
	tracker.RecordCapacityFailure("codex", "s1", "gpt", now)
	tracker.RecordReplayDispatch("codex", "s1", "/goal resume", now.Add(time.Minute))
	states := tracker.List(now)
	if len(states) != 1 || !states[0].ReplayPending || states[0].LastReplayOutcome != "dispatched" {
		t.Fatalf("dispatch state=%+v", states)
	}
	tracker.RecordReplayResponse("codex", "s1", false, false, 0, 0)
	states = tracker.List(now)
	if states[0].ReplayPending || states[0].LastReplayOutcome != "provider_or_quota_failure" || states[0].GoalAttempts != 1 {
		t.Fatalf("failed replay state=%+v", states[0])
	}
	tracker.RecordReplayDispatch("codex", "s1", "/goal resume", now.Add(2*time.Minute))
	tracker.RecordReplayResponse("codex", "s1", true, true, 10, 5)
	state := tracker.List(now)[0]
	if state.ReplayPending || state.LastReplayOutcome != "generation_began" || state.RequestTokens != 10 || state.ResponseTokens != 5 {
		t.Fatalf("successful replay state=%+v", state)
	}
}

func TestRecoveryTrackerKeepsSoonestResetWithinFailureBatch(t *testing.T) {
	tracker := NewRecoveryTracker()
	now := time.Now().UTC()
	tracker.RecordQuotaFailure("codex", "s1", wake.KindCodexQuota, "", now, now.Add(2*time.Hour))
	tracker.RecordQuotaFailure("codex", "s1", wake.KindCodexQuota, "", now.Add(time.Second), now.Add(20*time.Minute))
	state := tracker.List(now)[0]
	if !state.ResetAt.Equal(now.Add(20 * time.Minute)) {
		t.Fatalf("reset=%s, want soonest reset %s", state.ResetAt, now.Add(20*time.Minute))
	}
	tracker.RecordQuotaFailure("codex", "s1", wake.KindCodexQuota, "", now.Add(10*time.Minute), now.Add(3*time.Hour))
	state = tracker.List(now)[0]
	if !state.ResetAt.Equal(now.Add(3 * time.Hour)) {
		t.Fatalf("new batch reset=%s, want %s", state.ResetAt, now.Add(3*time.Hour))
	}
}

func TestRecoveryTrackerRecordsLaterSessionSuccess(t *testing.T) {
	tracker := NewRecoveryTracker()
	now := time.Now().UTC()
	tracker.RecordQuotaFailure("claude", "resumed", wake.KindClaudeQuota, "claude-opus", now, now.Add(5*24*time.Hour))
	tracker.RecordSessionSuccess("claude", "resumed", now.Add(30*time.Second))
	state := tracker.List(now)[0]
	if !state.LastSuccessAt.Equal(now.Add(30*time.Second)) || !state.LastFailureAt.Equal(now) {
		t.Fatalf("session success erased failure evidence: %+v", state)
	}
}
