package accounts

import (
	"fmt"
	"strings"
)

// longQuotaWindowMinSeconds is the shortest window length treated as an
// account-wide weekly limit.
const longQuotaWindowMinSeconds = 6 * 24 * 60 * 60

// IsModelScopedWindow reports a window that belongs to an additional
// per-model limit rather than the account-wide quota.
func IsModelScopedWindow(window UsageWindow) bool {
	return strings.TrimSpace(window.Feature) != ""
}

// IsLongQuotaWindow reports whether a window is the weekly quota. A reported
// length decides; without one, the name does. Codex once reported the weekly
// window only as "secondary" and now sometimes only as "primary" with a
// length, so the slot name alone is never trusted when a length is present.
func IsLongQuotaWindow(window UsageWindow) bool {
	if window.LimitWindowSeconds > 0 {
		return window.LimitWindowSeconds >= longQuotaWindowMinSeconds
	}
	name := strings.ToLower(window.Name)
	return name == "secondary" || strings.Contains(name, "7d") || strings.Contains(name, "weekly")
}

// WeeklyCookedWindow returns the first account-wide weekly window that is
// fully consumed. When no account-wide weekly window is reported at all, the
// synthetic "reached" window (upstream limit_reached) stands in for it,
// unless a full shorter window already explains the limit.
// This is the single definition of "cooked" shared by sr status, sr switch,
// sr reset, and the server's reset handler.
func WeeklyCookedWindow(windows []UsageWindow) (UsageWindow, bool) {
	var reached *UsageWindow
	sawLong := false
	shortFull := false
	for i, window := range windows {
		if IsModelScopedWindow(window) {
			continue
		}
		if window.Name == "reached" {
			reached = &windows[i]
			continue
		}
		if !IsLongQuotaWindow(window) {
			if window.LimitWindowSeconds > 0 && window.UsedPercent >= 100 {
				shortFull = true
			}
			continue
		}
		sawLong = true
		if window.UsedPercent >= 100 {
			return window, true
		}
	}
	if reached != nil && !sawLong && !shortFull {
		return *reached, true
	}
	return UsageWindow{}, false
}

// DescribeAccountWindows summarizes the account-wide windows a cooked
// decision was made from, e.g. "primary 7d 100%, secondary 5h 40%". Model
// scoped windows are left out because they never decide eligibility.
func DescribeAccountWindows(windows []UsageWindow) string {
	var parts []string
	for _, window := range windows {
		if IsModelScopedWindow(window) {
			continue
		}
		if window.Name == "reached" {
			parts = append(parts, "limit_reached")
			continue
		}
		length := "unknown length"
		switch {
		case window.LimitWindowSeconds >= 24*60*60:
			length = fmt.Sprintf("%dd", window.LimitWindowSeconds/(24*60*60))
		case window.LimitWindowSeconds > 0:
			length = fmt.Sprintf("%dh", (window.LimitWindowSeconds+1800)/3600)
		}
		parts = append(parts, fmt.Sprintf("%s %s %.0f%%", window.Name, length, window.UsedPercent))
	}
	if len(parts) == 0 {
		return "no account-wide windows reported"
	}
	return strings.Join(parts, ", ")
}
