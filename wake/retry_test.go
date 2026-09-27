package wake

import (
	"testing"
	"time"
)

func TestGoalResumePolicyAvoidsImmediateReplayAndBoundsFallback(t *testing.T) {
	now := time.Now().UTC()
	p := GoalResumePolicy{Cooldown: time.Minute, MaxGoalAttempts: 2, ContinueAfter: 2, AllowContinue: true}
	if got := p.Next(ResumeState{Failures: 1, GoalAttempts: 1, LastFailureAt: now}, now.Add(5*time.Second), true); got != ResumeWait {
		t.Fatalf("early action=%s, want wait", got)
	}
	if got := p.Next(ResumeState{Failures: 1, GoalAttempts: 1, LastFailureAt: now}, now.Add(time.Minute), false); got != ResumeProbe {
		t.Fatalf("unhealthy action=%s, want probe", got)
	}
	if got := p.Next(ResumeState{Failures: 2, GoalAttempts: 2, LastFailureAt: now}, now.Add(time.Minute), true); got != ResumeContinue {
		t.Fatalf("fallback action=%s, want continue", got)
	}
	if got := p.Next(ResumeState{Failures: 3, GoalAttempts: 2, ContinueSent: true, LastFailureAt: now}, now.Add(time.Minute), true); got != ResumeStop {
		t.Fatalf("bounded action=%s, want stop", got)
	}
}

func TestGoalResumePolicyDoesNotFallbackByDefault(t *testing.T) {
	p := DefaultGoalResumePolicy()
	state := ResumeState{Failures: 4, GoalAttempts: 2, LastFailureAt: time.Now().Add(-time.Hour)}
	if got := p.Next(state, time.Now(), true); got != ResumeStop {
		t.Fatalf("default action=%s, want stop", got)
	}
}
