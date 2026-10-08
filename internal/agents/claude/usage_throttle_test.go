package claude

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

type usage429Transport func(*http.Request) (*http.Response, error)

func (f usage429Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestUsageThrottleDeadlineFromProviderHeaders(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	date := now.Add(time.Hour).Format(http.TimeFormat)
	for _, tc := range []struct {
		name, raw string
		want      time.Duration
	}{
		{"seconds", "1800", 30 * time.Minute},
		{"http_date", date, time.Hour},
		{"http_date_capped", now.Add(31 * 24 * time.Hour).Format(http.TimeFormat), 30 * 24 * time.Hour},
		{"zero", "0", usageThrottleFallbackWait},
		{"missing", "", usageThrottleFallbackWait},
		{"malformed", "oops", usageThrottleFallbackWait},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := usageThrottleRetryAt(tc.raw, now)
			if got.Sub(now) != tc.want {
				t.Fatalf("Retry-After %q: got %s, want %s", tc.raw, got.Sub(now), tc.want)
			}
		})
	}
}

func TestFetchUsageReturnsTypedThrottleWithDeadline(t *testing.T) {
	client := &http.Client{Transport: usage429Transport(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Status:     "429 Too Many Requests",
			Header:     http.Header{"Retry-After": []string{"1800"}},
			Body:       io.NopCloser(strings.NewReader("{}")),
		}, nil
	})}
	before := time.Now()
	_, err := FetchUsage(context.Background(), client, "fake-credential")
	var throttled *UsageThrottleError
	if !errors.As(err, &throttled) {
		t.Fatalf("got %v, want typed UsageThrottleError", err)
	}
	if throttled.RetryAt.Before(before.Add(29 * time.Minute)) {
		t.Fatalf("retry at %v before 30m provider deadline", throttled.RetryAt)
	}
}

func TestFetchUsageKeepsQuotaWindowsFromThrottledResponse(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).Unix()
	client := &http.Client{Transport: usage429Transport(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Status:     "429 Too Many Requests",
			Header: http.Header{
				"Retry-After":                                []string{"60"},
				"Anthropic-Ratelimit-Unified-5h-Status":      []string{"allowed"},
				"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.42"},
				"Anthropic-Ratelimit-Unified-5h-Reset":       []string{strconv.FormatInt(reset, 10)},
			},
			Body: io.NopCloser(strings.NewReader("{}")),
		}, nil
	})}
	_, err := FetchUsage(context.Background(), client, "fake-credential")
	var throttled *UsageThrottleError
	if !errors.As(err, &throttled) || len(throttled.Windows) != 1 {
		t.Fatalf("error = %#v, want one reset window", err)
	}
	if throttled.Windows[0].UsedPercent != 42 || throttled.Windows[0].ResetAt.IsZero() {
		t.Fatalf("windows = %+v, want utilization and reset", throttled.Windows)
	}
}
