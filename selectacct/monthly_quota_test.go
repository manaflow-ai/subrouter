package selectacct

import (
	"testing"
	"time"
)

func TestMonthlyQuotaConstrainsHeadroomWithoutInventingPace(t *testing.T) {
	reset := time.Now().AddDate(0, 1, 0).UTC()
	score := ScoreFromLimitWindows("kimi", 0, []LimitWindow{{Name: "5h", UsedPercent: 0, LimitWindowSeconds: 18000}, {Name: "monthly", UsedPercent: 100, ResetAt: reset, ResetAfterSeconds: int64(time.Until(reset).Seconds())}})
	if score.Headroom != 0 || !score.WeeklyCooked() {
		t.Fatalf("monthly exhaustion was ignored: %+v", score)
	}
	if score.WeeklySurplus != 0 {
		t.Fatalf("pace was fabricated for an unknown calendar duration: %+v", score)
	}
	if got, ok := score.ExhaustionClearsAt(); !ok || !got.Equal(reset) {
		t.Fatalf("monthly reset lost: %v %v", got, ok)
	}
}
