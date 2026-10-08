package proxy

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
)

// poolExhaustedError is the account-selection failure when every candidate
// account of a provider is exhausted. Its message keeps the historical prefix
// "no non-exhausted <provider> accounts available" and, when subrouter knows
// when the first account frees up, appends
//
//	; next account frees up in <human> (retry after <N>s)
//
// The "(retry after <N>s)" token is a cross-repo contract: cmux's agent
// auto-resume parses `retry after (\d+)s` from the agent's error text to wait
// for capacity instead of re-sending on a blind backoff ladder. Do not change
// its spelling. The same N goes into the 503's Retry-After header.
type poolExhaustedError struct {
	provider      accounts.Provider
	nextAvailable time.Time
	// retryAfterSeconds is fixed at construction so the body and the
	// Retry-After header always agree; zero means no hint.
	retryAfterSeconds int64
}

func newPoolExhaustedError(provider accounts.Provider, nextAvailable time.Time, now time.Time) error {
	err := &poolExhaustedError{provider: provider, nextAvailable: nextAvailable}
	if !nextAvailable.IsZero() && nextAvailable.After(now) {
		err.retryAfterSeconds = ceilSeconds(nextAvailable.Sub(now))
	}
	return err
}

func (e *poolExhaustedError) Error() string {
	message := fmt.Sprintf("no non-exhausted %s accounts available", e.provider)
	if e.retryAfterSeconds <= 0 {
		return message
	}
	return fmt.Sprintf("%s; next account frees up in %s (retry after %ds)",
		message, humanRetryDuration(e.retryAfterSeconds), e.retryAfterSeconds)
}

// ceilSeconds rounds a positive duration up to whole seconds, minimum 1.
func ceilSeconds(d time.Duration) int64 {
	seconds := int64((d + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}

// humanRetryDuration renders a wait as "45s", "47m", "2h5m" or "9d3h"
// (minutes are dropped once the wait is a day or more). Past the first minute
// it rounds up, so it never promises capacity earlier than the exact
// retry-after seconds.
func humanRetryDuration(seconds int64) string {
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	minutes := (seconds + 59) / 60
	days := minutes / (24 * 60)
	hours := minutes % (24 * 60) / 60
	mins := minutes % 60
	var b strings.Builder
	if days > 0 {
		fmt.Fprintf(&b, "%dd", days)
	}
	if hours > 0 {
		fmt.Fprintf(&b, "%dh", hours)
	}
	if mins > 0 && days == 0 {
		fmt.Fprintf(&b, "%dm", mins)
	}
	return b.String()
}

// poolExhaustedError builds the selection failure for an exhausted pool,
// with the earliest time any candidate becomes usable again.
func (s Server) poolExhaustedError(provider accounts.Provider, candidates []accounts.Account, poolModel string) error {
	now := time.Now()
	return newPoolExhaustedError(provider, s.poolNextAvailable(candidates, poolModel, now), now)
}

// poolNextAvailable is the earliest time any candidate OAuth account stops
// being exhausted. One account is usable again only when all of its
// exhausting conditions clear: every measured usage window at zero headroom
// (short/5h and weekly alike, read from the measured scores without the
// request-time overlays) and any request-time mark (an upstream 429's reset,
// credential or model exclusion). Accounts with any unknown part are skipped;
// zero means no account's recovery time is known, and the caller must not
// guess.
func (s Server) poolNextAvailable(candidates []accounts.Account, poolModel string, now time.Time) time.Time {
	base := s.Scheduler
	if s.SchedulerRef != nil {
		base = s.SchedulerRef.RefreshSeed()
	}
	base = base.ForModel(poolModel)
	var earliest time.Time
	for _, account := range candidates {
		if account.AuthMode != accounts.AuthModeOAuth {
			continue
		}
		provider := schedulerAccountProvider(account.Provider)
		clears, known := base.ScoreFor(provider, account.ID).ExhaustionClearsAt()
		if !known {
			continue
		}
		if s.SchedulerRef != nil {
			if mark, blocked := s.SchedulerRef.ExplicitBlockedUntilFor(provider, account.ID, poolModel, now); blocked && mark.After(clears) {
				clears = mark
			}
		}
		if clears.IsZero() {
			// Nothing measured or marked holds this account; whatever made
			// selection reject it is not something with a known end.
			continue
		}
		if earliest.IsZero() || clears.Before(earliest) {
			earliest = clears
		}
	}
	return earliest
}

var retryAfterTokenPattern = regexp.MustCompile(`retry after \d+s`)

// writeAccountSelectionUnavailable answers a request whose account selection
// failed. An exhausted pool with a known recovery time sets Retry-After to the
// same N its message carries. A hosted broker's Retry-After is relayed, and
// because the broker error deliberately never copies the hosted body, the
// "(retry after <N>s)" token is appended so agents see it in the error text.
func writeAccountSelectionUnavailable(w http.ResponseWriter, err error) {
	message := err.Error()
	var exhausted *poolExhaustedError
	var brokerHTTPError *broker.HTTPStatusError
	switch {
	case errors.As(err, &exhausted) && exhausted.retryAfterSeconds > 0:
		w.Header().Set("Retry-After", strconv.FormatInt(exhausted.retryAfterSeconds, 10))
	case errors.As(err, &brokerHTTPError) && brokerHTTPError.RetryAfter != "":
		w.Header().Set("Retry-After", brokerHTTPError.RetryAfter)
		now := time.Now()
		if retryAt := parseRetryAfter(strings.TrimSpace(brokerHTTPError.RetryAfter), now); retryAt.After(now) && !retryAfterTokenPattern.MatchString(message) {
			message = fmt.Sprintf("%s (retry after %ds)", message, ceilSeconds(retryAt.Sub(now)))
		}
	}
	http.Error(w, message, http.StatusServiceUnavailable)
}

// poolExhaustedResponse is the transport equivalent of
// writeAccountSelectionUnavailable. Failover discovers exhaustion after the
// request has already entered ReverseProxy, so it must synthesize the same
// local 503 instead of returning one account's provider 429 body.
func poolExhaustedResponse(err error, req *http.Request) *http.Response {
	header := make(http.Header)
	header.Set("Content-Type", "text/plain; charset=utf-8")
	var exhausted *poolExhaustedError
	if errors.As(err, &exhausted) && exhausted.retryAfterSeconds > 0 {
		header.Set("Retry-After", strconv.FormatInt(exhausted.retryAfterSeconds, 10))
	}
	return &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Status:     "503 Service Unavailable",
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(err.Error() + "\n")),
		Request:    req,
	}
}
