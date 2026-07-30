package selectacct

import "strings"

type LimitWindow struct {
	Name               string
	UsedPercent        float64
	LimitWindowSeconds int64
	ResetAfterSeconds  int64
	// Feature is the upstream limit_name of the model-specific quota pool this
	// window belongs to (e.g. "GPT-5.3-Codex-Spark"); empty for account-wide
	// windows.
	Feature string
}

// ScoreFromLimitWindows computes an account's base score from its account-wide
// windows, plus a per-feature score for every model-specific quota pool present
// in the windows. Model-specific windows are excluded from the base score so a
// request for a regular model is not penalized by an unrelated pool's usage, and
// a request for a metered model (e.g. Spark) is scored only against its own pool.
// This is fully general: any additional rate limit the upstream reports becomes
// its own pool, keyed by the limit name, with no per-model special cases.
func ScoreFromLimitWindows(accountID string, sessions int, windows []LimitWindow) Score {
	base := scoreFromLimitWindows(accountID, sessions, filterWindowsByModelKey(windows, ""), "")
	var modelScores map[string]Score
	for _, key := range distinctModelKeys(windows) {
		if modelScores == nil {
			modelScores = make(map[string]Score)
		}
		modelScores[key] = scoreFromLimitWindows(accountID, sessions, filterWindowsByModelKey(windows, key), key)
	}
	base.ModelScores = modelScores
	return base
}

// ModelKey reduces a request model (or a window feature name) to a comparison
// key by lowercasing and dropping non-alphanumeric characters, so a request
// model "gpt-5.3-codex-spark" and the upstream quota name "GPT-5.3-Codex-Spark"
// resolve to the same key. Matching is strict equality on this key: a regular
// model key ("gpt53codex") is a prefix of the Spark key ("gpt53codexspark"), so
// any looser (substring) match would misroute regular traffic into the Spark pool.
func ModelKey(model string) string {
	return normalizeModelKey(model)
}

func limitWindowModelKey(window LimitWindow) string {
	return normalizeModelKey(window.Feature)
}

func normalizeModelKey(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func distinctModelKeys(windows []LimitWindow) []string {
	seen := make(map[string]bool)
	var keys []string
	for _, window := range windows {
		key := limitWindowModelKey(window)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	return keys
}

func filterWindowsByModelKey(windows []LimitWindow, key string) []LimitWindow {
	filtered := make([]LimitWindow, 0, len(windows))
	for _, window := range windows {
		if limitWindowModelKey(window) == key {
			filtered = append(filtered, window)
		}
	}
	return filtered
}

func scoreFromLimitWindows(accountID string, sessions int, windows []LimitWindow, modelKey string) Score {
	headroom := 1.0
	shortHeadroom := 1.0
	shortResetAfterSeconds := int64(0)
	weeklyPressure := 0.0
	hasShortWindow := false
	for _, window := range windows {
		remaining := 1 - clampPercent(window.UsedPercent)/100
		if remaining < headroom {
			headroom = remaining
		}
		if isShortWindow(window) {
			hasShortWindow = true
			if remaining <= shortHeadroom {
				shortHeadroom = remaining
				shortResetAfterSeconds = window.ResetAfterSeconds
			}
		}
		if fableDrainPressureModelKey(modelKey) && isNonShortResettingWindow(window) {
			if pressure := expiryPressure(remaining, window.ResetAfterSeconds); pressure > weeklyPressure {
				weeklyPressure = pressure
			}
		}
	}
	if !hasShortWindow {
		shortHeadroom = headroom
	}
	pressure := expiryPressure(headroom, shortResetAfterSeconds)
	if fableDrainPressureModelKey(modelKey) {
		pressure += weeklyPressure
	}
	return Score{
		AccountID:              accountID,
		Headroom:               headroom,
		ShortHeadroom:          shortHeadroom,
		ShortResetAfterSeconds: shortResetAfterSeconds,
		ExpiryPressure:         pressure,
		Sessions:               sessions,
	}
}

func isShortWindow(window LimitWindow) bool {
	if window.LimitWindowSeconds > 0 {
		return window.LimitWindowSeconds <= 6*60*60
	}
	return false
}

func isNonShortResettingWindow(window LimitWindow) bool {
	if window.LimitWindowSeconds <= 6*60*60 {
		return false
	}
	return window.ResetAfterSeconds > 0
}

func fableDrainPressureModelKey(key string) bool {
	return key == "claudefable"
}

func expiryPressure(headroom float64, resetAfterSeconds int64) float64 {
	if resetAfterSeconds <= 0 {
		return 0
	}
	return headroom / float64(resetAfterSeconds)
}

func clampPercent(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 100:
		return 100
	default:
		return value
	}
}

// VelocityProjectionMinutes is how far ahead a refresh-delta velocity signal
// projects headroom (kept for the experiment's comparison policies), and
// LiveDebitPerRequest is the assumed cost of one routed request as a fraction
// of an account's short window: Pick debits the snapshot headroom by it for
// every request the proxy itself routed to the account since the last usage
// refresh, so concurrent fleets spread across accounts instead of herding
// onto the snapshot's single best account until it cooks. Deliberately an
// over-estimate (real mean is ~0.3%): the experiment shows 2% minimizes
// user-visible failovers (-31% vs no debit), with degradation on both sides
// (0.6%: -23%, 5%: -27%, 10%: -22%). Chosen by TestVelocityPolicyExperiment
// (a deterministic 24h 3-user/19-thread fleet-burst workload over 24 seeds);
// rerun with -v when tuning.
const (
	VelocityProjectionMinutes = 20.0
	LiveDebitPerRequest       = 0.020
)
