package main

import (
	"fmt"
	"strings"
	"time"
)

// releaseStateView mirrors proxy.ReleaseState: the post-upgrade bake state a
// supervised team host reports as "release" in /_subrouter/health.
type releaseStateView struct {
	Version         string `json:"version"`
	PreviousVersion string `json:"previous_version"`
	State           string `json:"state"`
	Reason          string `json:"reason"`
	Since           string `json:"since"`
	BakeUntil       string `json:"bake_until"`
}

// releaseStatusText renders the bake state in one line, e.g.
// "v0.1.150 baking (12m left)" or
// "rolled back from v0.1.150: proxy 5xx 4.1% vs 0.2% baseline".
// It returns "" when there is nothing to report.
func releaseStatusText(release *releaseStateView, now time.Time) string {
	if release == nil || release.State == "" {
		return ""
	}
	version := release.Version
	if version == "" {
		version = "(unknown version)"
	}
	switch release.State {
	case "baking":
		until, err := time.Parse(time.RFC3339, release.BakeUntil)
		if err != nil {
			return fmt.Sprintf("%s baking", version)
		}
		left := until.Sub(now)
		if left <= 0 {
			return fmt.Sprintf("%s baking (window over; promoted on the next guard check)", version)
		}
		return fmt.Sprintf("%s baking (%s left)", version, roundedMinutes(left))
	case "promoted":
		return fmt.Sprintf("%s promoted", version)
	case "rolled_back":
		text := "rolled back from " + version
		if release.PreviousVersion != "" {
			text += " to " + release.PreviousVersion
		}
		if reason := strings.TrimSpace(release.Reason); reason != "" {
			text += ": " + reason
		}
		return text
	default:
		return fmt.Sprintf("%s %s", version, release.State)
	}
}

func roundedMinutes(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	return fmt.Sprintf("%dm", int((d+30*time.Second)/time.Minute))
}
