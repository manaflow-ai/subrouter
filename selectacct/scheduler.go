package selectacct

import (
	"errors"
	"hash/fnv"
	"math"
	"math/rand/v2"
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
	affinityKey   string
}

// headroomBandWidth buckets headroom so affinity can break ties inside a band.
//
// Without bucketing, Headroom is a continuous float that is essentially never
// equal between two accounts, so every tiebreak below it (including Sessions)
// is unreachable and selection collapses to a deterministic argmax. Every
// client then computes the same maximum over the same periodically-refreshed
// snapshot and stampedes the same account until the next usage fetch.
const headroomBandWidth = 0.10

// headroomBand returns the descending-preference bucket for a headroom value.
func headroomBand(headroom float64) int {
	if headroom <= 0 {
		return 0
	}
	return int(headroom / headroomBandWidth)
}

// affinityWeight implements rendezvous (highest-random-weight) hashing.
//
// Each client independently prefers a different account with no coordination,
// and because the weight depends on both sides, adding or removing an account
// only reassigns the clients bound to it rather than reshuffling everyone.
func affinityWeight(key, accountID string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(accountID))
	return h.Sum64()
}

const MinNewSessionHeadroom = 0.40

// MinStickyRetentionHeadroom is the headroom an account must still have for an
// idle session to stay on it. It sits far below MinNewSessionHeadroom on
// purpose. Placing a NEW session on an account needs room for a whole
// conversation, but a session that is already running built the upstream
// prompt cache on that account, and the cache is per account: moving the
// session re-bills its entire conversation prefix as uncached input. Holding
// the session until its account is nearly empty costs less than moving it.
const MinStickyRetentionHeadroom = 0.05

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
	for accountKey, count := range counts {
		next.sessionCounts[accountKey] = count
	}
	return next
}

// ForModel narrows scoring to a model's dedicated quota pool when one exists.
// If the model maps to no known pool (the common case: regular models), the
// base scheduler is returned unchanged so account-wide quota is used. When a
// pool exists but a given account lacks it, that account scores zero so it is
// not picked for a model it cannot serve.
// WithAffinity binds selection to a stable client identity so different
// clients prefer different accounts. An empty key restores plain argmax
// ordering, which is what every existing caller gets.
func (s Scheduler) WithAffinity(key string) Scheduler {
	next := s
	next.affinityKey = key
	return next
}

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
			if score.Provider == account.ProviderAntigravity {
				// Antigravity omits disabled, unavailable, and sometimes merely
				// unreported buckets. Absence is unknown, not proof that this
				// account cannot serve a pool another account happened to expose.
				modelScore = score
				modelScore.ModelScores = nil
			} else {
				modelScore = Score{AccountID: score.AccountID, Provider: score.Provider, Headroom: 0, ShortHeadroom: 0}
			}
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

// HasModelPoolFor reports whether a model pool is present within one provider's
// scores. Pool names are not globally authoritative: two providers can expose
// the same model spelling while grouping quota differently.
func (s Scheduler) HasModelPoolFor(provider account.Provider, model string) bool {
	if provider == "" {
		provider = account.ProviderCodex
	}
	key := ModelKey(model)
	if key == "" {
		return false
	}
	for _, score := range s.scores {
		if score.Provider != provider {
			continue
		}
		if _, ok := score.ModelScores[key]; ok {
			return true
		}
	}
	return false
}

func (s Scheduler) hasModelScore(key string) bool {
	for _, score := range s.scores {
		if _, ok := score.ModelScores[key]; ok {
			return true
		}
	}
	return false
}

// Pick chooses an account for placing work (a new session, a failover
// retry). Codex pools spread across the usable accounts (see spreadPool);
// everything else takes the sort's leading candidate.
func (s Scheduler) Pick(candidates []account.Account) (account.Account, error) {
	if len(candidates) == 0 {
		return account.Account{}, ErrNoAccounts
	}
	sorted := s.sortCandidates(candidates)
	if pool := s.spreadPool(sorted); len(pool) > 1 {
		return pool[s.spreadIndex(pool)], nil
	}
	return sorted[0], nil
}

// PickBest returns the deterministic argmax of the selection order, with no
// placement spreading. Callers that maintain ONE choice over time (sr
// auto-switch re-picks the single active CLI account on an interval) use
// this so an unchanged pool keeps an unchanged answer instead of rotating
// through equally-usable accounts.
func (s Scheduler) PickBest(candidates []account.Account) (account.Account, error) {
	if len(candidates) == 0 {
		return account.Account{}, ErrNoAccounts
	}
	return s.sortCandidates(candidates)[0], nil
}

func (s Scheduler) sortCandidates(candidates []account.Account) []account.Account {
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
		if s.affinityKey != "" {
			// Compare by band, then by affinity, so a client sticks to its own
			// account while never preferring a materially emptier one.
			leftBand := headroomBand(left.Headroom)
			rightBand := headroomBand(right.Headroom)
			if leftBand != rightBand {
				return leftBand > rightBand
			}
			leftWeight := affinityWeight(s.affinityKey, sorted[i].ID)
			rightWeight := affinityWeight(s.affinityKey, sorted[j].ID)
			if leftWeight != rightWeight {
				return leftWeight > rightWeight
			}
		} else if left.Headroom != right.Headroom {
			return left.Headroom > right.Headroom
		}
		if left.Sessions != right.Sessions {
			return left.Sessions < right.Sessions
		}
		return sorted[i].ID < sorted[j].ID
	})
	return sorted
}

// spreadPool returns the leading candidates a new session may be spread
// across: the usable OAuth accounts tied at the sort's top ExpiryPressure
// (the leading equal-pressure band).
//
// Between accounts with DIFFERENT pressure, the drain-soonest ordering is a
// deliberate, simulation-tuned policy (see gto_sim_test.go) and stays
// deterministic: distinct pressures form singleton bands and no spreading
// happens. WITHIN a band the pressure signal says nothing, and the remaining
// ordering input is headroom from a usage snapshot that lags consumption
// (integer-only and hours late for Codex). A deterministic argmax over that
// snapshot sends EVERY placement to the same account until the snapshot
// catches up; sessions are long-lived and sticky, so the pool drains one
// account at a time and a cap hit reroutes the whole herd at once
// (2026-08-18: one account served 100% of Codex traffic for five hours while
// six healthy accounts sat idle). Sampling within the band removes the herd
// without touching retention or the pressure policy.
//
// This is provider-agnostic by construction: Codex accounts report only a
// weekly window, so their pressure is always 0 and the band is the whole
// usable pool; scored Claude accounts carry distinct reset-time pressures,
// so their bands are singletons; an unscored pool of any provider (all
// optimistic defaults, pressure 0) spreads instead of herding. A future
// provider gets the right behaviour from whatever pressure signal its usage
// windows produce, with no per-provider case here.
func (s Scheduler) spreadPool(sorted []account.Account) []account.Account {
	topScore := s.score(sorted[0].Provider, sorted[0].ID)
	if selectionTier(sorted[0], topScore) != 0 {
		return nil
	}
	end := 1
	for end < len(sorted) {
		score := s.score(sorted[end].Provider, sorted[end].ID)
		if selectionTier(sorted[end], score) != 0 || score.ExpiryPressure != topScore.ExpiryPressure {
			break
		}
		end++
	}
	return sorted[:end]
}

// spreadWeightFloor keeps an account that sits exactly at the new-session
// threshold pickable (weight zero would starve it forever even when every
// pool member is at the threshold), while keeping it rare next to genuinely
// roomy accounts.
const spreadWeightFloor = 0.01

// spreadIndex samples one account from the pool, weighted by headroom
// surplus above the new-session threshold and damped by the number of
// sessions already assigned there. Surplus weighting drains roomy accounts
// faster, so the pool converges toward even headroom instead of even request
// counts; the session damping keeps a lagging snapshot (which still reports
// a busy account as roomy) from overloading it between refreshes.
func (s Scheduler) spreadIndex(pool []account.Account) int {
	weights := make([]float64, len(pool))
	total := 0.0
	for i, candidate := range pool {
		score := s.score(candidate.Provider, candidate.ID)
		surplus := math.Min(score.Headroom, score.ShortHeadroom) - MinNewSessionHeadroom
		if surplus < spreadWeightFloor {
			surplus = spreadWeightFloor
		}
		weight := surplus / float64(1+score.Sessions)
		weights[i] = weight
		total += weight
	}
	target := spreadRandFloat() * total
	for i, weight := range weights {
		target -= weight
		if target < 0 {
			return i
		}
	}
	return len(pool) - 1
}

// spreadRandFloat is swapped out by tests that need a deterministic pick
// sequence. The math/rand/v2 top-level generator is safe for concurrent use.
var spreadRandFloat = rand.Float64

func (s Scheduler) UsableForNewSession(provider account.Provider, accountID string) bool {
	return s.score(provider, accountID).usableForNewSession()
}

// UsableForStickySession reports whether an account still has enough headroom
// to keep serving a session already assigned to it. Callers placing a fresh
// session use UsableForNewSession instead.
// Retention reads the measured score, not the live-debited one. The debit
// floors headroom at 0.01, so on any busy account it sits below every
// retention threshold and would evict the session the debit was only meant to
// steer new picks away from.
func (s Scheduler) UsableForStickySession(provider account.Provider, accountID string) bool {
	return s.measuredScore(provider, accountID).usableForStickySession()
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
	return Scheduler{scores: s.scores, sessionCounts: s.sessionCounts, liveDebits: debits, affinityKey: s.affinityKey}
}

func (s Scheduler) score(provider account.Provider, accountID string) Score {
	score := s.measuredScore(provider, accountID)
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

// measuredScore is the score from the last usage refresh, with no live debit
// applied. The debit is our own optimism about requests already in flight, not
// evidence that an account is empty, so decisions that evict work rather than
// merely reorder picks read this instead.
func (s Scheduler) measuredScore(provider account.Provider, accountID string) Score {
	score, ok := s.scores[ScoreKey(provider, accountID)]
	if !ok {
		score = Score{AccountID: accountID, Provider: provider, Headroom: 1, ShortHeadroom: 1}
	}
	if s.sessionCounts != nil {
		key := ScoreKey(provider, accountID)
		if count, ok := s.sessionCounts[key]; ok {
			score.Sessions = count
		} else {
			// Keep source compatibility for callers that still pass bare account
			// IDs. Production routing supplies provider-scoped ScoreKey values.
			score.Sessions = s.sessionCounts[accountID]
		}
	}
	return score
}

func (s Score) usableForNewSession() bool {
	return s.Headroom >= MinNewSessionHeadroom && s.ShortHeadroom >= MinNewSessionHeadroom
}

func (s Score) usableForStickySession() bool {
	return s.Headroom >= MinStickyRetentionHeadroom && s.ShortHeadroom >= MinStickyRetentionHeadroom
}

func (s Score) exhausted() bool {
	return s.Headroom <= 0 || s.ShortHeadroom <= 0
}
