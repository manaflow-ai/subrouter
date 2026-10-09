package selectacct

import (
	"errors"
	"math"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

var ErrNoAccounts = errors.New("no accounts available")

type Score struct {
	AccountID     string
	Provider      account.Provider
	Headroom      float64
	ShortHeadroom float64
	// WeeklyHeadroom is the remaining fraction of the account's long (weekly)
	// windows only, unlike Headroom which also folds in the short (5h) window.
	// Status views read it (WeeklyCooked). It defaults to 1 (unknown windows
	// read as "not cooked").
	WeeklyHeadroom      float64
	WeeklyHeadroomKnown bool
	// WeeklySurplus is the weekly quota the account will lose at reset if it
	// keeps its average pace (see unpacedSurplus). Placement spreading leans
	// new sessions toward it (spreadIndex); it never enters the sort order.
	WeeklySurplus          float64
	ShortResetAfterSeconds int64
	// ExhaustedResetAt is when every measured window at zero headroom has
	// reset (the latest of their reset times); zero when no window is
	// exhausted. ExhaustedResetUnknown is set when an exhausted window
	// reported no reset time. Request-time exhaustion marks are separate
	// (SchedulerRef), so callers combine the two; see ExhaustionClearsAt.
	ExhaustedResetAt      time.Time
	ExhaustedResetUnknown bool
	ExpiryPressure        float64
	Sessions              int
	ModelScores           map[string]Score
	// MissingModelSupport describes what an omitted model quota bucket means
	// for this account. The zero value defers to the provider default (see
	// DefaultMissingModelSupport), so seed, fallback, and exhaustion-mark
	// scores built without usage telemetry keep the provider's safe meaning.
	// Adapters with better evidence set Unsupported or Unknown explicitly.
	MissingModelSupport ModelSupport
	// Fresh marks a score computed from a successful, current usage fetch, as
	// opposed to a seed carried forward from the previous scheduler (fetch
	// failed/stale) or a request-time exhaustion mark. Expiry reconciliation
	// uses it to tell "fresh evidence re-confirmed exhausted" apart from "old
	// zero score dragged along".
	Fresh bool
	// ClaudeExtraUsage is kept outside Headroom: paid credits must never make an
	// account look like ordinary subscription capacity. Proxy routing consults
	// it only after every subscription account is exhausted.
	ClaudeExtraUsageEnabled   bool
	ClaudeExtraUsageKnown     bool
	ClaudeExtraUsageRemaining float64
	// CreditPromoted pulls an API key whose free credit grant would expire
	// unspent ahead of plan quota for new placements
	// (SelectionTierPromotedCredits). CreditExpiresAt orders API keys among
	// themselves: the grant that expires first is spent first.
	CreditPromoted  bool
	CreditExpiresAt time.Time
}

// ModelSupport is the provider-normalized meaning of an omitted model quota
// bucket.
type ModelSupport uint8

const (
	// ModelSupportProviderDefault resolves through DefaultMissingModelSupport.
	ModelSupportProviderDefault ModelSupport = iota
	// ModelSupportUnsupported excludes the account from a pool it lacks.
	ModelSupportUnsupported
	// ModelSupportUnknown keeps the account eligible on its account-level score.
	ModelSupportUnknown
)

// DefaultMissingModelSupport is the provider table for scores that do not
// declare MissingModelSupport. Antigravity omits disabled, unavailable, and
// sometimes merely unreported buckets, so absence is unknown rather than proof
// the account cannot serve a pool another account exposed. Other providers
// treat absence as unsupported.
func DefaultMissingModelSupport(provider account.Provider) ModelSupport {
	if provider == account.ProviderAntigravity {
		return ModelSupportUnknown
	}
	return ModelSupportUnsupported
}

func (s Score) missingModelSupport() ModelSupport {
	if s.MissingModelSupport != ModelSupportProviderDefault {
		return s.MissingModelSupport
	}
	return DefaultMissingModelSupport(s.Provider)
}

type Scheduler struct {
	scores        map[string]Score
	sessionCounts map[string]int
	liveDebits    map[string]int
	inflight      map[string]int
	// capacity is one (model, tier) pool's consecutive capacity failures per
	// ScoreKey (WithCapacityMarks). It only reorders candidates.
	capacity map[string]int
}

const (
	MinNewSessionHeadroom     = 0.40
	InflightPenaltyPerRequest = 0.15
)

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
		inflight:      s.inflight,
		capacity:      s.capacity,
	}
	for key, existing := range s.scores {
		next.scores[key] = existing
	}
	next.scores[ScoreKey(score.Provider, score.AccountID)] = score
	return next
}

// WithScores returns a copy with each score replaced or added, in one copy
// of the score map.
func (s Scheduler) WithScores(scores []Score) Scheduler {
	if len(scores) == 0 {
		return s
	}
	next := Scheduler{
		scores:        make(map[string]Score, len(s.scores)+len(scores)),
		sessionCounts: s.sessionCounts,
		liveDebits:    s.liveDebits,
		inflight:      s.inflight,
		capacity:      s.capacity,
	}
	for key, existing := range s.scores {
		next.scores[key] = existing
	}
	for _, score := range scores {
		next.scores[ScoreKey(score.Provider, score.AccountID)] = score
	}
	return next
}

func (s Scheduler) WithSessionCounts(counts map[string]int) Scheduler {
	next := Scheduler{
		scores:        s.scores,
		sessionCounts: map[string]int{},
		liveDebits:    s.liveDebits,
		inflight:      s.inflight,
		capacity:      s.capacity,
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
func (s Scheduler) ForModel(model string) Scheduler {
	key := ModelKey(model)
	if key == "" || !s.hasModelScore(key) {
		return s
	}
	next := Scheduler{
		scores:        make(map[string]Score, len(s.scores)),
		sessionCounts: s.sessionCounts,
		liveDebits:    s.liveDebits,
		inflight:      s.inflight,
		capacity:      s.capacity,
	}
	for scoreKey, score := range s.scores {
		modelScore, ok := score.ModelScores[key]
		if !ok {
			if score.missingModelSupport() == ModelSupportUnknown {
				// Some providers omit unmeasured buckets. Absence is unknown,
				// not proof that this account cannot serve the pool.
				modelScore = score
				modelScore.ModelScores = nil
			} else {
				modelScore = Score{
					AccountID: score.AccountID, Provider: score.Provider, Headroom: 0, ShortHeadroom: 0,
					// Weekly headroom is account-level evidence; carry it.
					WeeklyHeadroom:      score.WeeklyHeadroom,
					WeeklyHeadroomKnown: score.WeeklyHeadroomKnown,
					// Paid Claude capacity is account metadata, not model-pool
					// subscription headroom. Preserve it on the synthetic exhausted
					// model score so a different account's model overlay cannot hide
					// the funded fallback after every subscription is cooked.
					ClaudeExtraUsageEnabled:   score.ClaudeExtraUsageEnabled,
					ClaudeExtraUsageKnown:     score.ClaudeExtraUsageKnown,
					ClaudeExtraUsageRemaining: score.ClaudeExtraUsageRemaining,
				}
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
		leftTier := s.tier(sorted[i])
		rightTier := s.tier(sorted[j])
		if leftTier != rightTier {
			return leftTier < rightTier
		}
		// Credit grants are lost at expiry: spend the one that expires first.
		if (leftTier == SelectionTierPromotedCredits || leftTier == SelectionTierAPIKey) &&
			!left.CreditExpiresAt.IsZero() && !right.CreditExpiresAt.IsZero() &&
			!left.CreditExpiresAt.Equal(right.CreditExpiresAt) {
			return left.CreditExpiresAt.Before(right.CreditExpiresAt)
		}
		// Within a tier, an account shedding this pool's requests goes after
		// the ones that are not; fewer consecutive failures first.
		leftCapacity := s.CapacityFailures(sorted[i].Provider, sorted[i].ID)
		rightCapacity := s.CapacityFailures(sorted[j].Provider, sorted[j].ID)
		if leftCapacity != rightCapacity {
			return leftCapacity < rightCapacity
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
	if s.tier(sorted[0]) != SelectionTierPlan {
		return nil
	}
	topCapacity := s.CapacityFailures(sorted[0].Provider, sorted[0].ID)
	end := 1
	for end < len(sorted) {
		score := s.score(sorted[end].Provider, sorted[end].ID)
		if s.tier(sorted[end]) != SelectionTierPlan || score.ExpiryPressure != topScore.ExpiryPressure ||
			s.CapacityFailures(sorted[end].Provider, sorted[end].ID) != topCapacity {
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
// counts; live in-flight pressure adds a direct short-horizon signal while
// the existing assignment damping remains unchanged.
func (s Scheduler) spreadIndex(pool []account.Account) int {
	weights := make([]float64, len(pool))
	total := 0.0
	for i, candidate := range pool {
		score := s.score(candidate.Provider, candidate.ID)
		surplus := math.Min(score.Headroom, score.ShortHeadroom) - MinNewSessionHeadroom
		if surplus < spreadWeightFloor {
			surplus = spreadWeightFloor
		}
		weight := (surplus + weeklySurplusSpreadGain*score.WeeklySurplus) / float64(1+score.Sessions)
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

// Expiring weekly quota. An account's weekly surplus (Score.WeeklySurplus)
// is quota that is lost at reset unless the account is used faster than its
// average pace. Two things follow from it, and neither touches the sort key:
//
//   - Admission: an account below MinNewSessionHeadroom still takes new
//     sessions while its weekly surplus is at least weeklySurplusMinAdmit
//     and its headroom at least weeklySurplusMinHeadroom. The floor exists so
//     a new conversation does not run the account dry; an account ahead of
//     pace this close to reset will not, and without admission the quota
//     sits idle until reset while new sessions go to API keys.
//   - Weighting: within a spread band the surplus adds to an account's
//     weight (weeklySurplusSpreadGain), so expiring quota drains first
//     without collapsing the band into a single account.
//
// Keeping the surplus out of the sort order is what keeps the 2026-08-18 herd
// away: every Codex account still has pressure 0, so the band stays the whole
// usable pool. Tuned by TestWeeklyExpiryPolicyExperiment (rerun with
// SUBROUTER_SIM_TABLE=1 -v).
var (
	weeklySurplusMinAdmit    = 0.10
	weeklySurplusMinHeadroom = 0.15
	weeklySurplusSpreadGain  = 4.0
)

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

// tier places an account by its measured score. Live debits are the
// proxy's own guess about requests in flight: they reorder and reweight
// subscription accounts (the debited score still drives both), but must
// never be what moves a measured-healthy subscription account behind a paid
// API key. Six routed requests (0.12) used to take a 0.50 account under
// MinNewSessionHeadroom and send new sessions and failovers to the key.
func (s Scheduler) tier(acct account.Account) int {
	return selectionTier(acct, s.measuredScore(acct.Provider, acct.ID))
}

// Selection tiers, cheapest money first. Pick sorts by tier before any
// pressure or headroom signal, so a tier is a hard preference and the
// existing spreading logic only operates inside one tier.
//
//   - SelectionTierPlan: subscription quota with room for a new session.
//   - SelectionTierAPIKey: an API key that has not reported exhaustion. For
//     Claude these are expected to be funded by free monthly Console credits
//     (see the cost order in internal/proxy/claude_cost_tiers.go), so they
//     still cost no money.
//   - SelectionTierPlanConstrained: subscription quota below the new-session
//     floor but not exhausted. Still free, but a new conversation placed
//     there would soon have to move and re-bill its prompt prefix.
//   - SelectionTierPlanExhausted: subscription accounts with no quota. The
//     Claude proxy turns a pick here into paid extra usage (tier 3 of the
//     Claude cost order) or a pool-exhausted 503.
//   - SelectionTierExhaustedAPIKey: an API key held out by an explicit
//     exhaustion mark (spent credits, rejected key). It must rank behind
//     exhausted subscriptions, or it would shadow the extra-usage fallback.
const (
	// SelectionTierPromotedCredits is an API key whose free grant is behind
	// pace to be spent before it expires (see CreditPromoted). Plan quota
	// keeps until its own reset; the grant does not, so it goes first.
	SelectionTierPromotedCredits = iota
	SelectionTierPlan
	SelectionTierAPIKey
	SelectionTierPlanConstrained
	SelectionTierPlanExhausted
	SelectionTierExhaustedAPIKey
	SelectionTierOther
)

// SelectionTier reports the tier Pick sorts this account into, from its
// measured score. Status views use it to show the routing order.
func (s Scheduler) SelectionTier(acct account.Account) int {
	return s.tier(acct)
}

func selectionTier(acct account.Account, score Score) int {
	if acct.AuthMode == account.AuthModeAPIKey && score.CreditPromoted && !score.exhausted() {
		return SelectionTierPromotedCredits
	}
	if acct.AuthMode == account.AuthModeOAuth && score.usableForNewSession() {
		return SelectionTierPlan
	}
	if acct.AuthMode == account.AuthModeAPIKey && !score.exhausted() {
		return SelectionTierAPIKey
	}
	if acct.AuthMode == account.AuthModeOAuth && !score.exhausted() {
		return SelectionTierPlanConstrained
	}
	if acct.AuthMode == account.AuthModeOAuth {
		return SelectionTierPlanExhausted
	}
	if acct.AuthMode == account.AuthModeAPIKey {
		return SelectionTierExhaustedAPIKey
	}
	return SelectionTierOther
}

// ScoreFor returns the stored score for an account, or an optimistic default
// (treated as fully healthy) when none is known. Callers use it to seed a
// refresh so accounts whose fresh usage could not be fetched preserve their
// last known score instead of being clobbered with low-confidence data.
func (s Scheduler) ScoreFor(provider account.Provider, accountID string) Score {
	if score, ok := s.scores[ScoreKey(provider, accountID)]; ok {
		return score
	}
	return Score{AccountID: accountID, Provider: provider, Headroom: 1, ShortHeadroom: 1, WeeklyHeadroom: 1}
}

// WithLiveDebits attaches per-account routed-request counts accumulated since
// the last usage refresh. score() debits LiveDebitPerRequest of headroom per
// routed request, so between refreshes the scheduler sees its OWN traffic
// draining the snapshot instead of herding every pick onto the same account.
// Keys are ScoreKey(provider, accountID).
func (s Scheduler) WithLiveDebits(debits map[string]int) Scheduler {
	return Scheduler{scores: s.scores, sessionCounts: s.sessionCounts, liveDebits: debits, inflight: s.inflight, capacity: s.capacity}
}

// WithInflightCounts attaches the number of physical upstream attempts whose
// response is still open for each account. It is a placement-only signal:
// measured quota still controls exhaustion, sticky retention and paid-fallback
// tiering.
func (s Scheduler) WithInflightCounts(counts map[string]int) Scheduler {
	return Scheduler{scores: s.scores, sessionCounts: s.sessionCounts, liveDebits: s.liveDebits, inflight: counts, capacity: s.capacity}
}

func (s Scheduler) score(provider account.Provider, accountID string) Score {
	score := s.measuredScore(provider, accountID)
	key := ScoreKey(provider, accountID)
	if count := s.liveDebits[key]; count > 0 {
		score = applyPlacementPenalty(score, LiveDebitPerRequest*float64(count))
	}
	if count := s.inflight[key]; count > 0 {
		score = applyPlacementPenalty(score, InflightPenaltyPerRequest*float64(count))
	}
	return score
}

// applyPlacementPenalty is deliberately soft. Live load may steer a new pick
// below the admission threshold, but it cannot create or clear exhaustion.
// Those decisions require measured quota or an authoritative upstream mark.
func applyPlacementPenalty(score Score, penalty float64) Score {
	if penalty <= 0 {
		return score
	}
	if score.Headroom > 0.01 {
		score.Headroom = math.Max(0.01, score.Headroom-penalty)
	}
	if score.ShortHeadroom > 0.01 {
		score.ShortHeadroom = math.Max(0.01, score.ShortHeadroom-penalty)
	}
	score.WeeklySurplus = math.Max(0, score.WeeklySurplus-penalty)
	// Surplus only admits an account whose short window is above the floor;
	// re-check it after a live-load penalty. A reported short window always
	// has a reset time once it is in use; Codex's weekly-only shape has none,
	// and there ShortHeadroom is the weekly reading.
	if score.ShortResetAfterSeconds > 0 && score.ShortHeadroom < MinNewSessionHeadroom {
		score.WeeklySurplus = 0
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
		score = Score{AccountID: accountID, Provider: provider, Headroom: 1, ShortHeadroom: 1, WeeklyHeadroom: 1}
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

// UsableForNewSession reports whether the scheduler would place a new session
// on an account with this score: headroom above MinNewSessionHeadroom in every
// window, or expiring weekly quota below it (see weeklySurplusMinAdmit).
func (s Score) UsableForNewSession() bool {
	return s.usableForNewSession()
}

func (s Score) usableForNewSession() bool {
	if s.Headroom >= MinNewSessionHeadroom && s.ShortHeadroom >= MinNewSessionHeadroom {
		return true
	}
	// WeeklySurplus is zero unless every non-weekly window is above the
	// floor (scoreFromLimitWindows, and score() after live debits), so this
	// never admits an account whose short window is nearly spent.
	return s.WeeklySurplus >= weeklySurplusMinAdmit && s.Headroom >= weeklySurplusMinHeadroom
}

func (s Score) usableForStickySession() bool {
	return s.Headroom >= MinStickyRetentionHeadroom && s.ShortHeadroom >= MinStickyRetentionHeadroom
}

func (s Score) exhausted() bool {
	return s.Headroom <= 0 || s.ShortHeadroom <= 0
}

// ExhaustionClearsAt reports when the measured windows stop holding this
// score exhausted. A score that is not exhausted returns (zero, true). An
// exhausted score returns its windows' latest reset, or false when any
// exhausting window's reset is unknown (including scores with no window
// evidence at all, such as a synthetic zero for a missing model pool).
func (s Score) ExhaustionClearsAt() (time.Time, bool) {
	if !s.exhausted() {
		return time.Time{}, true
	}
	if s.ExhaustedResetUnknown || s.ExhaustedResetAt.IsZero() {
		return time.Time{}, false
	}
	return s.ExhaustedResetAt, true
}

// WeeklyCooked reports that every long (weekly) window is exhausted.
func (s Score) WeeklyCooked() bool {
	return s.WeeklyHeadroomKnown && s.WeeklyHeadroom <= 0
}
