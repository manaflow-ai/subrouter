package selectacct

import (
	"errors"
	"math"
	"sort"

	"github.com/manaflow-ai/subrouter/account"
)

var ErrNoAccounts = errors.New("no accounts available")

type Score struct {
	AccountID              string
	Provider               account.Provider
	Headroom               float64
	ShortHeadroom          float64
	ShortResetAfterSeconds int64
	ExpiryPressure         float64
	Sessions               int
	ModelScores            map[string]Score
	// Fresh marks a score computed from a successful, current usage fetch, as
	// opposed to a seed carried forward from the previous scheduler (fetch
	// failed/stale) or a request-time exhaustion mark. Expiry reconciliation
	// uses it to tell "fresh evidence re-confirmed exhausted" apart from "old
	// zero score dragged along".
	Fresh bool
}

type Scheduler struct {
	scores        map[string]Score
	sessionCounts map[string]int
	liveDebits    map[string]int
}

const MinNewSessionHeadroom = 0.40

// ScoreKey is the scheduler's identity for an account. A Codex account's ID is
// its bare email and a Claude account's ID is its profile name, which can also
// be an email, so the same string routinely identifies one account per
// provider (e.g. lawrence@cmux.com exists as both a Codex account and a Claude
// profile). Keying scores by the bare ID alone lets one provider's score
// silently overwrite the other's; scoping the key by provider keeps them
// distinct.
func ScoreKey(provider account.Provider, accountID string) string {
	if provider == "" {
		provider = account.ProviderCodex
	}
	return string(provider) + "\x00" + accountID
}

func NewScheduler(scores []Score) Scheduler {
	byID := make(map[string]Score, len(scores))
	for _, score := range scores {
		byID[ScoreKey(score.Provider, score.AccountID)] = score
	}
	return Scheduler{scores: byID}
}

func (s Scheduler) WithScore(score Score) Scheduler {
	next := Scheduler{
		scores:        make(map[string]Score, len(s.scores)+1),
		sessionCounts: s.sessionCounts,
		liveDebits:    s.liveDebits,
	}
	for key, existing := range s.scores {
		next.scores[key] = existing
	}
	next.scores[ScoreKey(score.Provider, score.AccountID)] = score
	return next
}

func (s Scheduler) WithSessionCounts(counts map[string]int) Scheduler {
	next := Scheduler{
		scores:        s.scores,
		sessionCounts: map[string]int{},
		liveDebits:    s.liveDebits,
	}
	for accountID, count := range counts {
		next.sessionCounts[accountID] = count
	}
	return next
}

// ForModel narrows scoring to a model's dedicated quota pool when one exists.
// If the model maps to no known pool (the common case: regular models), the
// base scheduler is returned unchanged so account-wide quota is used. When a
// pool exists but a given account lacks it, that account scores zero so it is
// not picked for a model it cannot serve.
func (s Scheduler) ForModel(model string) Scheduler {
	key := ModelKey(model)
	if key == "" || !s.hasModelScore(key) {
		return s
	}
	next := Scheduler{
		scores:        make(map[string]Score, len(s.scores)),
		sessionCounts: s.sessionCounts,
		liveDebits:    s.liveDebits,
	}
	for scoreKey, score := range s.scores {
		modelScore, ok := score.ModelScores[key]
		if !ok {
			modelScore = Score{AccountID: score.AccountID, Provider: score.Provider, Headroom: 0, ShortHeadroom: 0}
		}
		next.scores[scoreKey] = modelScore
	}
	return next
}

// HasModelPool reports whether the model maps to a dedicated quota pool present
// on at least one scored account.
func (s Scheduler) HasModelPool(model string) bool {
	key := ModelKey(model)
	return key != "" && s.hasModelScore(key)
}

func (s Scheduler) hasModelScore(key string) bool {
	for _, score := range s.scores {
		if _, ok := score.ModelScores[key]; ok {
			return true
		}
	}
	return false
}

func (s Scheduler) Pick(candidates []account.Account) (account.Account, error) {
	if len(candidates) == 0 {
		return account.Account{}, ErrNoAccounts
	}

	sorted := append([]account.Account(nil), candidates...)
	sort.SliceStable(sorted, func(i, j int) bool {
		left := s.score(sorted[i].Provider, sorted[i].ID)
		right := s.score(sorted[j].Provider, sorted[j].ID)
		leftTier := selectionTier(sorted[i], left)
		rightTier := selectionTier(sorted[j], right)
		if leftTier != rightTier {
			return leftTier < rightTier
		}
		leftUsable := left.usableForNewSession()
		rightUsable := right.usableForNewSession()
		if leftUsable != rightUsable {
			return leftUsable
		}
		if leftUsable && rightUsable && left.ExpiryPressure != right.ExpiryPressure {
			return left.ExpiryPressure > right.ExpiryPressure
		}
		if left.Headroom != right.Headroom {
			return left.Headroom > right.Headroom
		}
		if left.Sessions != right.Sessions {
			return left.Sessions < right.Sessions
		}
		return sorted[i].ID < sorted[j].ID
	})
	return sorted[0], nil
}

func (s Scheduler) UsableForNewSession(provider account.Provider, accountID string) bool {
	return s.score(provider, accountID).usableForNewSession()
}

func (s Scheduler) Exhausted(provider account.Provider, accountID string) bool {
	return s.score(provider, accountID).exhausted()
}

func selectionTier(acct account.Account, score Score) int {
	if acct.AuthMode == account.AuthModeOAuth && score.usableForNewSession() {
		return 0
	}
	if acct.AuthMode == account.AuthModeAPIKey {
		return 1
	}
	if acct.AuthMode == account.AuthModeOAuth && !score.exhausted() {
		return 2
	}
	if acct.AuthMode == account.AuthModeOAuth {
		return 3
	}
	return 4
}

// ScoreFor returns the stored score for an account, or an optimistic default
// (treated as fully healthy) when none is known. Callers use it to seed a
// refresh so accounts whose fresh usage could not be fetched preserve their
// last known score instead of being clobbered with low-confidence data.
func (s Scheduler) ScoreFor(provider account.Provider, accountID string) Score {
	if score, ok := s.scores[ScoreKey(provider, accountID)]; ok {
		return score
	}
	return Score{AccountID: accountID, Provider: provider, Headroom: 1, ShortHeadroom: 1}
}

// WithLiveDebits attaches per-account routed-request counts accumulated since
// the last usage refresh. score() debits LiveDebitPerRequest of headroom per
// routed request, so between refreshes the scheduler sees its OWN traffic
// draining the snapshot instead of herding every pick onto the same account.
// Keys are ScoreKey(provider, accountID).
func (s Scheduler) WithLiveDebits(debits map[string]int) Scheduler {
	return Scheduler{scores: s.scores, sessionCounts: s.sessionCounts, liveDebits: debits}
}

func (s Scheduler) score(provider account.Provider, accountID string) Score {
	score, ok := s.scores[ScoreKey(provider, accountID)]
	if !ok {
		score = Score{AccountID: accountID, Provider: provider, Headroom: 1, ShortHeadroom: 1}
	}
	if s.sessionCounts != nil {
		score.Sessions = s.sessionCounts[accountID]
	}
	if count := s.liveDebits[ScoreKey(provider, accountID)]; count > 0 {
		// A soft, self-correcting signal: it reorders picks and can push an
		// account below the new-session threshold, but never onto (or off of)
		// the exhaustion floor: routing our own optimism into an account is
		// recoverable; falsely marking it exhausted, or resurrecting a
		// genuinely exhausted one, is not.
		debit := LiveDebitPerRequest * float64(count)
		if score.Headroom > 0.01 {
			score.Headroom = math.Max(0.01, score.Headroom-debit)
		}
		if score.ShortHeadroom > 0.01 {
			score.ShortHeadroom = math.Max(0.01, score.ShortHeadroom-debit)
		}
	}
	return score
}

func (s Score) usableForNewSession() bool {
	return s.Headroom >= MinNewSessionHeadroom && s.ShortHeadroom >= MinNewSessionHeadroom
}

func (s Score) exhausted() bool {
	return s.Headroom <= 0 || s.ShortHeadroom <= 0
}
