package proxy

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// Live Codex capacity visibility. /_subrouter/codex-capacity is admin-only
// and keyed by account; this is the unauthenticated view in
// /_subrouter/traffic (accounts hashed, never emails) plus process-lifetime
// counters of what the capacity retry did, and the per-account marks in
// usage-status.

// Counter kinds for codexCapacityCounterStore.add.
const (
	codexCapacityCounterSameAccountRetry = "same_account_retry"
	codexCapacityCounterAccountSwitch    = "account_switch"
	codexCapacityCounterExhausted        = "exhausted"
	codexCapacityCounterMark             = "mark"
)

// codexCapacityTrafficMaxMarks bounds the marks listed in the traffic payload.
const codexCapacityTrafficMaxMarks = 64

// codexCapacityReasonModelCapacity is the X-Subrouter-Reason and traffic
// error class of a capacity failure returned to the client.
const codexCapacityReasonModelCapacity = "model_capacity"

// codexCapacityCounterStore counts capacity retry actions for this process.
// Nil-safe; one per Server, shared by every copy of it.
type codexCapacityCounterStore struct {
	sameAccountRetries atomic.Uint64
	accountSwitches    atomic.Uint64
	exhausted          atomic.Uint64
	marks              atomic.Uint64
}

// add counts one action of the given kind (codexCapacityCounter*).
func (c *codexCapacityCounterStore) add(kind string) {
	if c == nil {
		return
	}
	switch kind {
	case codexCapacityCounterSameAccountRetry:
		c.sameAccountRetries.Add(1)
	case codexCapacityCounterAccountSwitch:
		c.accountSwitches.Add(1)
	case codexCapacityCounterExhausted:
		c.exhausted.Add(1)
	case codexCapacityCounterMark:
		c.marks.Add(1)
	}
}

type codexCapacityCounters struct {
	SameAccountRetries uint64 `json:"same_account_retries"`
	AccountSwitches    uint64 `json:"account_switches"`
	Exhausted          uint64 `json:"exhausted"`
	Marks              uint64 `json:"marks"`
}

func (c *codexCapacityCounterStore) snapshot() codexCapacityCounters {
	if c == nil {
		return codexCapacityCounters{}
	}
	return codexCapacityCounters{
		SameAccountRetries: c.sameAccountRetries.Load(),
		AccountSwitches:    c.accountSwitches.Load(),
		Exhausted:          c.exhausted.Load(),
		Marks:              c.marks.Load(),
	}
}

// codexCapacityTrafficMark is one live mark with the account hashed.
type codexCapacityTrafficMark struct {
	AccountHash string `json:"account_hash"`
	Pool        string `json:"pool"`
	Failures    int    `json:"failures"`
	ExpiresAt   string `json:"expires_at"`
	Reason      string `json:"reason,omitempty"`
}

// codexCapacityTraffic is the codex_capacity object of /_subrouter/traffic.
type codexCapacityTraffic struct {
	Marks    []codexCapacityTrafficMark `json:"marks"`
	Counters codexCapacityCounters      `json:"counters"`
}

// codexCapacityMarks is every live Codex capacity mark.
func (s Server) codexCapacityMarks() []selectacct.CapacityMarkInfo {
	if s.SchedulerRef == nil {
		return nil
	}
	all := s.SchedulerRef.CapacityMarkList()
	out := all[:0]
	for _, mark := range all {
		if accountProviderFor(mark.Provider) == accounts.ProviderCodex {
			out = append(out, mark)
		}
	}
	return out
}

// codexCapacityTrafficSnapshot builds the traffic view: marks sorted by pool
// then expiry, at most codexCapacityTrafficMaxMarks of them.
func (s Server) codexCapacityTrafficSnapshot() *codexCapacityTraffic {
	marks := s.codexCapacityMarks()
	out := &codexCapacityTraffic{Marks: make([]codexCapacityTrafficMark, 0, len(marks)), Counters: s.codexCapacityCounters.snapshot()}
	sort.Slice(marks, func(i, j int) bool {
		left, right := selectacct.CapacityPoolKey(marks[i].Model, marks[i].Tier), selectacct.CapacityPoolKey(marks[j].Model, marks[j].Tier)
		if left != right {
			return left < right
		}
		if !marks[i].Until.Equal(marks[j].Until) {
			return marks[i].Until.Before(marks[j].Until)
		}
		return marks[i].AccountID < marks[j].AccountID
	})
	for _, mark := range marks {
		if len(out.Marks) == codexCapacityTrafficMaxMarks {
			break
		}
		out.Marks = append(out.Marks, codexCapacityTrafficMark{
			AccountHash: admissionAccountHash(mark.AccountID),
			Pool:        selectacct.CapacityPoolKey(mark.Model, mark.Tier),
			Failures:    mark.Failures,
			ExpiresAt:   mark.Until.UTC().Format(time.RFC3339),
			Reason:      mark.Reason,
		})
	}
	return out
}

// AccountCapacityMark is one live capacity mark on an account in
// usage-status.
type AccountCapacityMark struct {
	Pool      string `json:"pool"`
	Failures  int    `json:"failures"`
	ExpiresAt string `json:"expires_at"`
	Reason    string `json:"reason,omitempty"`
}

// withCodexCapacityMarks attaches each Codex account's live capacity marks.
// The input may be the shared cached snapshot, so it is copied, not mutated.
func (s Server) withCodexCapacityMarks(statuses []AccountUsageStatus) []AccountUsageStatus {
	marks := s.codexCapacityMarks()
	if len(marks) == 0 {
		return statuses
	}
	sort.Slice(marks, func(i, j int) bool {
		left, right := selectacct.CapacityPoolKey(marks[i].Model, marks[i].Tier), selectacct.CapacityPoolKey(marks[j].Model, marks[j].Tier)
		if left != right {
			return left < right
		}
		return marks[i].Until.Before(marks[j].Until)
	})
	byAccount := make(map[string][]AccountCapacityMark)
	for _, mark := range marks {
		key := selectacct.ScoreKey(accounts.ProviderCodex, mark.AccountID)
		byAccount[key] = append(byAccount[key], AccountCapacityMark{
			Pool:      selectacct.CapacityPoolKey(mark.Model, mark.Tier),
			Failures:  mark.Failures,
			ExpiresAt: mark.Until.UTC().Format(time.RFC3339),
			Reason:    mark.Reason,
		})
	}
	out := append([]AccountUsageStatus(nil), statuses...)
	for i := range out {
		if provider := accountProviderFor(out[i].Provider); provider != "" && provider != accounts.ProviderCodex {
			continue
		}
		out[i].CapacityMarks = byAccount[selectacct.ScoreKey(accounts.ProviderCodex, out[i].ID)]
	}
	return out
}

// codexCapacitySoonestExpiry is when the first live mark in the (model,
// tier) pool expires, zero when the pool has none.
func (s Server) codexCapacitySoonestExpiry(model, tier string) time.Time {
	modelKey, tierKey := selectacct.ModelKey(model), selectacct.NormalizeServiceTier(tier)
	var soonest time.Time
	for _, mark := range s.codexCapacityMarks() {
		if mark.Model != modelKey || mark.Tier != tierKey {
			continue
		}
		if soonest.IsZero() || mark.Until.Before(soonest) {
			soonest = mark.Until
		}
	}
	return soonest
}

// codexCapacityRetryAfterSeconds is the Retry-After for an exhausted
// capacity failure: whole seconds until the pool's soonest mark expires,
// clamped to 1..60, or 5 when no mark is known.
func codexCapacityRetryAfterSeconds(until, now time.Time) int {
	if until.IsZero() {
		return 5
	}
	seconds := int(math.Ceil(until.Sub(now).Seconds()))
	return min(max(seconds, 1), 60)
}

// labelCodexCapacityExhausted marks a capacity failure the retry ladder is
// returning to the client: X-Subrouter-Reason, a Retry-After from the
// pool's marks, the traffic error class, and the exhausted counter. Status
// and body are unchanged.
func (s *Server) labelCodexCapacityExhausted(ctx context.Context, response *http.Response, model, tier string) {
	if s == nil {
		return
	}
	s.codexCapacityCounters.add(codexCapacityCounterExhausted)
	markTrafficErrorClass(ctx, codexCapacityReasonModelCapacity)
	if response == nil {
		return
	}
	if response.Header == nil {
		response.Header = make(http.Header)
	}
	response.Header.Set(subrouterReasonHeader, codexCapacityReasonModelCapacity)
	response.Header.Set("Retry-After", strconv.Itoa(codexCapacityRetryAfterSeconds(s.codexCapacitySoonestExpiry(model, tier), time.Now())))
}
