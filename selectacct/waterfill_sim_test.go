package selectacct

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"testing"

	"github.com/manaflow-ai/subrouter/account"
)

// Concurrent-load placement experiment (2026-10-14). The other simulations
// model sessions as a steady burn on one account. This one models what the
// team router actually sees on its Claude pool, one second at a time,
// through the real Scheduler.Pick:
//
//   - accounts carry a 5h window and a weekly window with staggered resets
//     and staggered starting use, scored by ScoreFromLimitWindows from an
//     integer-percent snapshot refreshed every 60s, with live debits for
//     requests routed since the refresh and live in-flight counts;
//   - each account admits 6 concurrent requests (defaultAdmissionLimit).
//     A request whose account already has 8 waiters spills to the least
//     loaded free account without moving the session (#608/#609); a queue of
//     64 rejects, and the rejected request fails over (#607);
//   - a session is an agent loop: one request at a time with tool and human
//     gaps, and a quarter of sessions fan out bursts of 3-10 parallel
//     subagent requests on the same session id;
//   - a request served by an account other than the one that holds the
//     session's prompt cache re-bills the prefix: it costs 1+wfCacheMissCost
//     units of quota instead of 1. A session move does that once; a spill
//     does it per spilled request;
//   - an account at 100% of a window answers 429: the session moves.
//
// Placement randomness comes from a seeded generator, so runs are
// deterministic. Run the table with SUBROUTER_SIM_TABLE=1 go test
// ./selectacct -run TestWaterfillPolicyExperiment -v.

const (
	wfTicks          = 10 * 3600
	wfRefreshEvery   = 60
	wfAdmission      = 6
	wfSpillWaiters   = 8
	wfQueueCap       = 64
	wfShortSeconds   = 5 * 3600
	wfWeekSeconds    = 7 * 24 * 3600
	wfCacheMissCost  = 4.0
	wfActiveWindow   = 30 * 60
	wfStoredCap      = 512
	wfBlockedBackoff = 30
)

type wfScenario struct {
	name       string
	accounts   int
	sessions   int     // mean concurrent sessions
	shortQuota float64 // request units per 5h window
	weekQuota  float64 // request units per weekly window
	usedShort  [2]float64
	usedWeek   [2]float64
	provider   account.Provider
	weeklyOnly bool
}

type wfPolicy struct {
	name         string
	floor        float64
	gain         float64
	exponent     float64
	activeCounts bool // sessions active in the last 30 min instead of every stored assignment
	overflowOnly bool // a queue-full rejection moves only the request, not the session
	spillWaiters int  // waiters that trigger a spill; 0 means wfSpillWaiters
}

type wfResult struct {
	requests     int
	blocked      int
	queued       int
	waits        []int
	moves        int
	spills       int
	sessions     int
	missCost     float64
	totalCost    float64
	firstExhaust int
	stranded     float64
	resets       int
	exhausted429 int
}

type wfSession struct {
	id          int
	acct        int
	end         int
	nextAt      int
	outstanding int
	fanout      bool
	lastActive  int
	placedOnce  bool
	cacheAcct   int // account holding the session's prompt cache; -1 none
}

type wfRequest struct {
	sess    *wfSession
	acct    int
	enq     int
	dur     int
	spilled bool
}

type wfAccount struct {
	usedShort, usedWeek float64
	resetShort, resetWk int
	active              []int // completion ticks
	activeReq           []*wfRequest
	queue               []*wfRequest
	marked              bool
	debits              int
	exhaustedSeen       bool
}

func (a *wfAccount) exhausted() bool { return a.usedShort >= 1 || a.usedWeek >= 1 }

func wfLogNormal(rng *rand.Rand, median, sigma float64) float64 {
	return median * math.Exp(sigma*rng.NormFloat64())
}

func runWaterfillSim(sc wfScenario, policy wfPolicy, seed int64) wfResult {
	rng := rand.New(rand.NewSource(seed))
	pickRNG := rand.New(rand.NewSource(seed*7919 + 3))
	savedRand, savedFloor, savedGain, savedExp := spreadRandFloat, MinNewSessionHeadroom, spreadPressureGain, spreadLoadExponent
	spreadRandFloat = pickRNG.Float64
	MinNewSessionHeadroom, spreadPressureGain, spreadLoadExponent = policy.floor, policy.gain, policy.exponent
	defer func() {
		spreadRandFloat, MinNewSessionHeadroom, spreadPressureGain, spreadLoadExponent = savedRand, savedFloor, savedGain, savedExp
	}()

	n := sc.accounts
	ids := make([]string, n)
	candidates := make([]account.Account, n)
	accts := make([]*wfAccount, n)
	index := map[string]int{}
	for i := range ids {
		ids[i] = fmt.Sprintf("acct-%02d", i)
		index[ids[i]] = i
		candidates[i] = account.Account{ID: ids[i], Provider: sc.provider, AuthMode: account.AuthModeOAuth}
		accts[i] = &wfAccount{
			usedShort:  sc.usedShort[0] + rng.Float64()*(sc.usedShort[1]-sc.usedShort[0]),
			usedWeek:   sc.usedWeek[0] + rng.Float64()*(sc.usedWeek[1]-sc.usedWeek[0]),
			resetShort: 1 + rng.Intn(wfShortSeconds),
			resetWk:    1 + rng.Intn(wfWeekSeconds),
		}
		if sc.weeklyOnly {
			accts[i].usedShort = 0
		}
	}

	res := wfResult{firstExhaust: -1}
	var scores []Score
	refresh := func(now int) {
		scores = scores[:0]
		for i, a := range accts {
			windows := []LimitWindow{{
				Name: "seven_day", UsedPercent: math.Floor(math.Min(a.usedWeek, 1) * 100),
				LimitWindowSeconds: wfWeekSeconds, ResetAfterSeconds: int64(a.resetWk - now),
			}}
			if !sc.weeklyOnly {
				windows = append(windows, LimitWindow{
					Name: "five_hour", UsedPercent: math.Floor(math.Min(a.usedShort, 1) * 100),
					LimitWindowSeconds: wfShortSeconds, ResetAfterSeconds: int64(a.resetShort - now),
				})
			}
			score := ScoreFromLimitWindows(ids[i], 0, windows)
			score.Provider = sc.provider
			score.Fresh = true
			scores = append(scores, score)
			a.marked = false
			a.debits = 0
		}
	}
	refresh(0)

	stored := map[int]int{} // session -> account, every assignment ever made
	storedOrder := []int{}  // insertion order for the 512 cap
	sessions := map[int]*wfSession{}
	nextID := 0

	// load is each account's session count as placement sees it, recomputed
	// once per tick and kept current by assign within the tick.
	load := make([]int, n)
	recount := func(now int) {
		for i := range load {
			load[i] = 0
		}
		if policy.activeCounts {
			for _, s := range sessions {
				if s.acct >= 0 && (s.outstanding > 0 || now-s.lastActive < wfActiveWindow) {
					load[s.acct]++
				}
			}
			return
		}
		for _, acct := range stored {
			load[acct]++
		}
	}
	measuredList := func() []Score {
		list := append([]Score(nil), scores...)
		for i, a := range accts {
			if a.marked {
				list[i].Headroom, list[i].ShortHeadroom = 0, 0
			}
		}
		return list
	}
	scheduler := func() Scheduler {
		counts, debits, inflight := map[string]int{}, map[string]int{}, map[string]int{}
		for i, a := range accts {
			key := ScoreKey(sc.provider, ids[i])
			counts[key] = load[i]
			debits[key] = a.debits
			inflight[key] = len(a.activeReq) + len(a.queue)
		}
		return NewScheduler(measuredList()).WithSessionCounts(counts).WithLiveDebits(debits).WithInflightCounts(inflight)
	}

	pick := func(now int, exclude int) int {
		cands := candidates
		if exclude >= 0 {
			cands = make([]account.Account, 0, n-1)
			for i, c := range candidates {
				if i != exclude {
					cands = append(cands, c)
				}
			}
		}
		picked, err := scheduler().Pick(cands)
		if err != nil {
			return -1
		}
		idx := index[picked.ID]
		if accts[idx].exhausted() {
			return -1
		}
		return idx
	}

	assign := func(s *wfSession, acct int) {
		if s.placedOnce && s.acct != acct {
			res.moves++
		}
		if policy.activeCounts {
			if s.acct >= 0 && load[s.acct] > 0 {
				load[s.acct]--
			}
			load[acct]++
		} else {
			if old, ok := stored[s.id]; ok && load[old] > 0 {
				load[old]--
			}
			load[acct]++
		}
		s.acct = acct
		s.placedOnce = true
		if _, ok := stored[s.id]; !ok {
			storedOrder = append(storedOrder, s.id)
			if len(storedOrder) > wfStoredCap {
				delete(stored, storedOrder[0])
				storedOrder = storedOrder[1:]
			}
		}
		stored[s.id] = acct
	}

	newSession := func(now int) {
		s := &wfSession{id: nextID, acct: -1, cacheAcct: -1, nextAt: now, lastActive: now}
		nextID++
		s.end = now + int(math.Min(wfLogNormal(rng, 45*60, 1.0), 8*3600))
		s.fanout = rng.Float64() < 0.25
		sessions[s.id] = s
		res.sessions++
	}

	var start func(now int, r *wfRequest)
	start = func(now int, r *wfRequest) {
		a := accts[r.acct]
		cost := 1.0
		if r.spilled || r.acct != r.sess.cacheAcct {
			cost += wfCacheMissCost
			res.missCost += wfCacheMissCost
		}
		if !r.spilled {
			r.sess.cacheAcct = r.acct
		}
		res.totalCost += cost
		if !sc.weeklyOnly {
			a.usedShort += cost / sc.shortQuota
		}
		a.usedWeek += cost / sc.weekQuota
		if a.exhausted() && !a.exhaustedSeen {
			a.exhaustedSeen = true
			if res.firstExhaust < 0 {
				res.firstExhaust = now
			}
		}
		a.activeReq = append(a.activeReq, r)
		a.active = append(a.active, now+r.dur)
		res.waits = append(res.waits, now-r.enq)
		if now > r.enq {
			res.queued++
		}
	}

	// route sends one request to its account, applying 429, spill and queue-cap
	// rules. Returns false when the request could not be placed anywhere.
	var route func(now int, r *wfRequest, depth int) bool
	route = func(now int, r *wfRequest, depth int) bool {
		if depth > 4 {
			return false
		}
		a := accts[r.acct]
		if a.exhausted() {
			// 429: mark, move the session, retry once elsewhere.
			res.exhausted429++
			a.marked = true
			next := pick(now, r.acct)
			if next < 0 {
				return false
			}
			if !r.spilled {
				assign(r.sess, next)
			}
			r.acct = next
			return route(now, r, depth+1)
		}
		if len(a.activeReq) < wfAdmission && len(a.queue) == 0 {
			a.debits++
			start(now, r)
			return true
		}
		spillAt := wfSpillWaiters
		if policy.spillWaiters > 0 {
			spillAt = policy.spillWaiters
		}
		if len(a.queue) >= spillAt {
			best, bestLoad := -1, 0
			sched := NewScheduler(measuredList())
			for i, other := range accts {
				if i == r.acct || other.exhausted() || len(other.activeReq) >= wfAdmission || len(other.queue) > 0 {
					continue
				}
				if !sched.UsableForStickySession(sc.provider, ids[i]) {
					continue
				}
				if load := len(other.activeReq); best < 0 || load < bestLoad {
					best, bestLoad = i, load
				}
			}
			if best >= 0 {
				res.spills++
				r.acct = best
				r.spilled = true
				accts[best].debits++
				start(now, r)
				return true
			}
		}
		if len(a.queue) >= wfQueueCap {
			next := pick(now, r.acct)
			if next < 0 {
				return false
			}
			if policy.overflowOnly {
				r.spilled = true
			} else if !r.spilled {
				assign(r.sess, next)
			}
			r.acct = next
			return route(now, r, depth+1)
		}
		a.debits++
		a.queue = append(a.queue, r)
		return true
	}

	issue := func(now int, s *wfSession) {
		k := 1
		if s.fanout && rng.Float64() < 0.15 {
			k = 3 + rng.Intn(8)
		}
		s.lastActive = now
		// Sticky retention: a session whose account cannot hold it moves.
		if s.acct >= 0 {
			sched := NewScheduler(measuredList())
			if accts[s.acct].marked || !sched.UsableForStickySession(sc.provider, ids[s.acct]) {
				if next := pick(now, s.acct); next >= 0 {
					assign(s, next)
				}
			}
		}
		if s.acct < 0 {
			next := pick(now, -1)
			if next < 0 {
				res.blocked += k
				res.requests += k
				s.nextAt = now + wfBlockedBackoff
				return
			}
			assign(s, next)
		}
		for j := 0; j < k; j++ {
			res.requests++
			r := &wfRequest{sess: s, acct: s.acct, enq: now, dur: int(math.Max(2, math.Min(wfLogNormal(rng, 12, 0.8), 300)))}
			if route(now, r, 0) {
				s.outstanding++
			} else {
				res.blocked++
			}
		}
		if s.outstanding == 0 {
			s.nextAt = now + wfBlockedBackoff
		}
	}

	arrivalRate := float64(sc.sessions) / (45 * 60 * math.Exp(0.5))
	for i := 0; i < sc.sessions; i++ {
		newSession(0)
		sessions[i].nextAt = rng.Intn(60)
	}

	for now := 0; now < wfTicks; now++ {
		if now%wfRefreshEvery == 0 && now > 0 {
			refresh(now)
		}
		recount(now)
		// Resets.
		for _, a := range accts {
			if !sc.weeklyOnly && now >= a.resetShort {
				weekLeft := math.Max(0, 1-a.usedWeek) * sc.weekQuota / sc.shortQuota
				res.stranded += math.Min(math.Max(0, 1-a.usedShort), weekLeft)
				res.resets++
				a.usedShort = 0
				a.resetShort += wfShortSeconds
				a.exhaustedSeen = a.exhausted()
			}
			if now >= a.resetWk {
				a.usedWeek = 0
				a.resetWk += wfWeekSeconds
				a.exhaustedSeen = a.exhausted()
			}
		}
		// Completions, then admit queued requests.
		for _, a := range accts {
			keptAt, keptReq := a.active[:0], a.activeReq[:0]
			for i, done := range a.active {
				if done <= now {
					r := a.activeReq[i]
					r.sess.outstanding--
					if r.sess.outstanding == 0 {
						r.sess.lastActive = now
						gap := 3 + rng.Intn(13)
						if rng.Float64() < 0.3 {
							gap = 30 + rng.Intn(271)
						}
						r.sess.nextAt = now + gap
					}
					continue
				}
				keptAt, keptReq = append(keptAt, done), append(keptReq, a.activeReq[i])
			}
			a.active, a.activeReq = keptAt, keptReq
			for len(a.activeReq) < wfAdmission && len(a.queue) > 0 {
				r := a.queue[0]
				a.queue = a.queue[1:]
				if a.exhausted() {
					// The queued request reaches the upstream and gets a 429.
					if !route(now, r, 0) {
						res.blocked++
						r.sess.outstanding--
						if r.sess.outstanding == 0 {
							r.sess.nextAt = now + wfBlockedBackoff
						}
					}
					continue
				}
				start(now, r)
			}
		}
		// Arrivals.
		for k := poissonDraw(rng, arrivalRate); k > 0; k-- {
			newSession(now)
		}
		// Session turns, in id order for determinism.
		ready := make([]int, 0, len(sessions))
		for id, s := range sessions {
			if s.outstanding == 0 && now >= s.nextAt {
				ready = append(ready, id)
			}
		}
		sort.Ints(ready)
		for _, id := range ready {
			s := sessions[id]
			if now >= s.end {
				delete(sessions, id)
				continue
			}
			issue(now, s)
		}
	}
	return res
}

type wfSummary struct {
	blockedPct, queuedPct, p95Wait, movesPerSession, spillPer1k, missPct, firstExhaustMin, strandedPerReset float64
}

func summarizeWaterfill(sc wfScenario, policy wfPolicy, seeds int) wfSummary {
	var sum wfSummary
	for seed := int64(1); seed <= int64(seeds); seed++ {
		r := runWaterfillSim(sc, policy, seed)
		sort.Ints(r.waits)
		p95 := 0.0
		if len(r.waits) > 0 {
			p95 = float64(r.waits[len(r.waits)*95/100])
		}
		first := float64(wfTicks) / 60
		if r.firstExhaust >= 0 {
			first = float64(r.firstExhaust) / 60
		}
		stranded := 0.0
		if r.resets > 0 {
			stranded = r.stranded / float64(r.resets)
		}
		sum.blockedPct += 100 * float64(r.blocked) / float64(r.requests)
		sum.queuedPct += 100 * float64(r.queued) / float64(r.requests)
		sum.p95Wait += p95
		sum.movesPerSession += float64(r.moves) / float64(r.sessions)
		sum.spillPer1k += 1000 * float64(r.spills) / float64(r.requests)
		sum.missPct += 100 * r.missCost / r.totalCost
		sum.firstExhaustMin += first
		sum.strandedPerReset += stranded
	}
	f := float64(seeds)
	return wfSummary{sum.blockedPct / f, sum.queuedPct / f, sum.p95Wait / f, sum.movesPerSession / f, sum.spillPer1k / f, sum.missPct / f, sum.firstExhaustMin / f, sum.strandedPerReset / f}
}

// Scenarios. Live on 2026-10-14 the team router's Claude pool had 11
// accounts able to take new sessions, about 30 requests in flight and a
// handful of fan-out sessions. "team" gives that pool twice the 5h quota its
// demand needs, so placement and concurrency effects dominate; "tight" gives
// it about as much quota as demand (accounts exhaust and reset during the
// run); "busy" runs 200 sessions, about 55 requests in flight on 72 slots.
// "codex-weekly" is a weekly-only pool, the shape of the Codex accounts.
var wfScenarios = []wfScenario{
	{
		name: "team", accounts: 12, sessions: 120, shortQuota: 6000, weekQuota: 48000,
		usedShort: [2]float64{0, 0.8}, usedWeek: [2]float64{0.3, 0.9}, provider: account.ProviderClaude,
	},
	{
		name: "tight", accounts: 12, sessions: 120, shortQuota: 3000, weekQuota: 24000,
		usedShort: [2]float64{0, 0.8}, usedWeek: [2]float64{0.3, 0.9}, provider: account.ProviderClaude,
	},
	{
		name: "busy", accounts: 12, sessions: 200, shortQuota: 10000, weekQuota: 80000,
		usedShort: [2]float64{0, 0.8}, usedWeek: [2]float64{0.3, 0.9}, provider: account.ProviderClaude,
	},
	{
		name: "codex-weekly", accounts: 20, sessions: 120, shortQuota: 1, weekQuota: 9000,
		usedWeek: [2]float64{0.4, 0.95}, provider: account.ProviderCodex, weeklyOnly: true,
	},
}

// Candidate policies. Commit 31ba40c1 also ran three rejected variants
// through temporary knobs: in-flight requests counted as load in the
// denominator instead of as a headroom penalty (worse p95 in every Claude
// scenario), headroom instead of surplus as the quota term, and cubed load.
var wfPolicies = []wfPolicy{
	{name: "main: stored counts, 1/(1+s), lean 3, floor 0.15", floor: 0.15, gain: 3, exponent: 1},
	{name: "main at floor 0.40", floor: 0.40, gain: 3, exponent: 1},
	{name: "main + queue-full overflow", floor: 0.15, gain: 3, exponent: 1, overflowOnly: true},
	{name: "active counts, 1/(1+s), lean 3", floor: 0.15, gain: 3, exponent: 1, activeCounts: true},
	{name: "active, 1/(1+s)^2, lean 3", floor: 0.15, gain: 3, exponent: 2, activeCounts: true},
	{name: "active, 1/(1+s)^2, lean 1", floor: 0.15, gain: 1, exponent: 2, activeCounts: true},
	{name: "active, 1/(1+s)^2, lean 0", floor: 0.15, gain: 0, exponent: 2, activeCounts: true},
	{name: "chosen: active, 1/(1+s)^2, lean 1, overflow", floor: 0.15, gain: 1, exponent: 2, activeCounts: true, overflowOnly: true},
	{name: "chosen, stored counts", floor: 0.15, gain: 1, exponent: 2, overflowOnly: true},
	{name: "chosen at floor 0.05", floor: 0.05, gain: 1, exponent: 2, activeCounts: true, overflowOnly: true},
	{name: "chosen at floor 0.10", floor: 0.10, gain: 1, exponent: 2, activeCounts: true, overflowOnly: true},
	{name: "chosen at floor 0.25", floor: 0.25, gain: 1, exponent: 2, activeCounts: true, overflowOnly: true},
	{name: "chosen at floor 0.40", floor: 0.40, gain: 1, exponent: 2, activeCounts: true, overflowOnly: true},
	{name: "chosen, spill at 4 waiters", floor: 0.15, gain: 1, exponent: 2, activeCounts: true, overflowOnly: true, spillWaiters: 4},
	{name: "chosen, spill at 16 waiters", floor: 0.15, gain: 1, exponent: 2, activeCounts: true, overflowOnly: true, spillWaiters: 16},
}

// TestWaterfillPolicyExperiment prints one table row per scenario and
// policy. Subtests are named scenario/pNN so rows can run as separate
// processes in parallel (the knobs are package globals):
//
//	SUBROUTER_SIM_TABLE=1 go test ./selectacct -run 'TestWaterfillPolicyExperiment/^team$/^p07$' -v
func TestWaterfillPolicyExperiment(t *testing.T) {
	if os.Getenv("SUBROUTER_SIM_TABLE") == "" {
		t.Skip("set SUBROUTER_SIM_TABLE=1 to print the placement policy table")
	}
	const seeds = 16
	for _, sc := range wfScenarios {
		t.Run(sc.name, func(t *testing.T) {
			for i, p := range wfPolicies {
				t.Run(fmt.Sprintf("p%02d", i+1), func(t *testing.T) {
					s := summarizeWaterfill(sc, p, seeds)
					t.Logf("ROW %s | p%02d %s | %.2f | %.1f | %.1f | %.3f | %.1f | %.1f | %.0f | %.3f |", sc.name, i+1, p.name, s.blockedPct, s.queuedPct, s.p95Wait, s.movesPerSession, s.spillPer1k, s.missPct, s.firstExhaustMin, s.strandedPerReset)
				})
			}
		})
	}
}

// The chosen policy must keep conversations on their account and spread
// load at least as well as the linear-damping policy it replaced, on the
// quota-tight pool where placement decides who exhausts first. The sim is
// deterministic per seed, so these are exact comparisons, not samples.
func TestWaterfillPlacementKeepsSessionsAndSpreadsLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("simulation")
	}
	const seeds = 3
	tight := wfScenarios[1]
	baseline := summarizeWaterfill(tight, wfPolicies[0], seeds)
	chosen := summarizeWaterfill(tight, wfPolicies[7], seeds)
	t.Logf("baseline %+v", baseline)
	t.Logf("chosen   %+v", chosen)
	if chosen.movesPerSession > 0.6*baseline.movesPerSession {
		t.Fatalf("moves/session %.3f, want well under the baseline's %.3f", chosen.movesPerSession, baseline.movesPerSession)
	}
	if chosen.blockedPct > baseline.blockedPct || chosen.p95Wait > baseline.p95Wait {
		t.Fatalf("blocked %.2f%% p95 %.0fs, want no worse than the baseline's %.2f%% %.0fs",
			chosen.blockedPct, chosen.p95Wait, baseline.blockedPct, baseline.p95Wait)
	}
	if chosen.firstExhaustMin < baseline.firstExhaustMin {
		t.Fatalf("first exhaustion at %.0f min, want no earlier than the baseline's %.0f", chosen.firstExhaustMin, baseline.firstExhaustMin)
	}
}
