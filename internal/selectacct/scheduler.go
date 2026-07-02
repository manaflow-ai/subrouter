package selectacct

import (
	"errors"
	"sort"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

var ErrNoAccounts = errors.New("no accounts available")

type Score struct {
	AccountID              string
	Provider               accounts.Provider
	Headroom               float64
	ShortHeadroom          float64
	ShortResetAfterSeconds int64
	ExpiryPressure         float64
	Sessions               int
	ModelScores            map[string]Score
}

type Scheduler struct {
	scores        map[string]Score
	sessionCounts map[string]int
}

const MinNewSessionHeadroom = 0.40

// ScoreKey is the scheduler's identity for an account. A Codex account's ID is
// its bare email and a Claude account's ID is its profile name, which can also
// be an email, so the same string routinely identifies one account per
// provider (e.g. lawrence@cmux.com exists as both a Codex account and a Claude
// profile). Keying scores by the bare ID alone lets one provider's score
// silently overwrite the other's; scoping the key by provider keeps them
// distinct.
func ScoreKey(provider accounts.Provider, accountID string) string {
	if provider == "" {
		provider = accounts.ProviderCodex
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

func (s Scheduler) Pick(candidates []accounts.Account) (accounts.Account, error) {
	if len(candidates) == 0 {
		return accounts.Account{}, ErrNoAccounts
	}

	sorted := append([]accounts.Account(nil), candidates...)
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

func (s Scheduler) UsableForNewSession(provider accounts.Provider, accountID string) bool {
	return s.score(provider, accountID).usableForNewSession()
}

func (s Scheduler) Exhausted(provider accounts.Provider, accountID string) bool {
	return s.score(provider, accountID).exhausted()
}

func selectionTier(account accounts.Account, score Score) int {
	if account.AuthMode == accounts.AuthModeOAuth && score.usableForNewSession() {
		return 0
	}
	if account.AuthMode == accounts.AuthModeAPIKey {
		return 1
	}
	if account.AuthMode == accounts.AuthModeOAuth && !score.exhausted() {
		return 2
	}
	if account.AuthMode == accounts.AuthModeOAuth {
		return 3
	}
	return 4
}

// ScoreFor returns the stored score for an account, or an optimistic default
// (treated as fully healthy) when none is known. Callers use it to seed a
// refresh so accounts whose fresh usage could not be fetched preserve their
// last known score instead of being clobbered with low-confidence data.
func (s Scheduler) ScoreFor(provider accounts.Provider, accountID string) Score {
	if score, ok := s.scores[ScoreKey(provider, accountID)]; ok {
		return score
	}
	return Score{AccountID: accountID, Provider: provider, Headroom: 1, ShortHeadroom: 1}
}

func (s Scheduler) ScoreForKey(key string) Score {
	if score, ok := s.scores[key]; ok {
		return score
	}
	provider := accounts.ProviderCodex
	accountID := key
	if before, after, ok := strings.Cut(key, "\x00"); ok {
		provider = accounts.Provider(before)
		accountID = after
	}
	return Score{AccountID: accountID, Provider: provider, Headroom: 1, ShortHeadroom: 1}
}

func (s Scheduler) score(provider accounts.Provider, accountID string) Score {
	score, ok := s.scores[ScoreKey(provider, accountID)]
	if !ok {
		score = Score{AccountID: accountID, Provider: provider, Headroom: 1, ShortHeadroom: 1}
	}
	if s.sessionCounts != nil {
		score.Sessions = s.sessionCounts[accountID]
	}
	return score
}

func (s Score) usableForNewSession() bool {
	return s.Headroom >= MinNewSessionHeadroom && s.ShortHeadroom >= MinNewSessionHeadroom
}

func (s Score) exhausted() bool {
	return s.Headroom <= 0 || s.ShortHeadroom <= 0
}
