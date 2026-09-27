package wake

import "time"

// ResumeAction is the command a shared watcher may issue after validating the
// terminal binding. The proxy never sends these actions itself.
type ResumeAction string

const (
	ResumeWait     ResumeAction = "wait"
	ResumeProbe    ResumeAction = "probe"
	ResumeGoal     ResumeAction = "goal-resume"
	ResumeContinue ResumeAction = "continue"
	ResumeStop     ResumeAction = "stop"
)

// GoalResumePolicy bounds costly Codex /goal resume replays after a provider
// capacity failure. A lightweight probe is required after cooldown; repeated
// failures may use one optional continue fallback, then stop until a fresh
// provider event is observed.
type GoalResumePolicy struct {
	Cooldown        time.Duration
	MaxGoalAttempts int
	ContinueAfter   int
	AllowContinue   bool
}

func DefaultGoalResumePolicy() GoalResumePolicy {
	return GoalResumePolicy{Cooldown: 60 * time.Second, MaxGoalAttempts: 2, ContinueAfter: 2, AllowContinue: false}
}

type ResumeState struct {
	Failures        int
	GoalAttempts    int
	ContinueSent    bool
	LastFailureAt   time.Time
	GenerationBegan bool
}

// Next returns a deterministic next action. A zero policy uses safe defaults.
func (p GoalResumePolicy) Next(state ResumeState, now time.Time, probeHealthy bool) ResumeAction {
	if p.Cooldown <= 0 {
		p.Cooldown = time.Minute
	}
	if p.MaxGoalAttempts <= 0 {
		p.MaxGoalAttempts = 2
	}
	if p.ContinueAfter <= 0 {
		p.ContinueAfter = p.MaxGoalAttempts
	}
	if state.Failures == 0 {
		return ResumeGoal
	}
	if !state.LastFailureAt.IsZero() && now.Before(state.LastFailureAt.Add(p.Cooldown)) {
		return ResumeWait
	}
	if !probeHealthy {
		return ResumeProbe
	}
	if state.GoalAttempts < p.MaxGoalAttempts {
		return ResumeGoal
	}
	if p.AllowContinue && !state.ContinueSent && state.Failures >= p.ContinueAfter {
		return ResumeContinue
	}
	return ResumeStop
}
