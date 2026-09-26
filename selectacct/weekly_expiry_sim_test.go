package selectacct

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"testing"

	"github.com/manaflow-ai/subrouter/account"
)

// Weekly use-it-or-lose-it experiment. The other simulations run for a day,
// so a weekly window never resets inside them and the quota an account still
// holds at its reset (lost for good) is invisible to them. This one runs a
// Codex-shaped pool for four weeks through the real Scheduler.Pick:
//   - every account reports only a 7-day window, with staggered reset times,
//     so the short-window ExpiryPressure is 0 for all of them;
//   - the snapshot the scheduler sees is integer used_percent lagging true
//     consumption by 30 minutes, and exhaustion is known at once (a 429);
//   - sessions arrive on a diurnal curve, live 30 minutes to 4 hours and are
//     sticky: they move only when their account caps (a user-visible
//     failover) or its measured headroom drops under the retention floor
//     (a prompt-cache move);
//   - one API-key account sits behind the pool (selection tier 1), so any new
//     session the subscription pool will not take is billed spend.
//
// Placement randomness comes from a seeded generator, so every run is
// deterministic.

const (
	wkTickSeconds = 5 * 60
	wkWeekSeconds = 7 * 24 * 3600
	wkWeekTicks   = wkWeekSeconds / wkTickSeconds
	wkDays        = 28
	wkTicks       = wkDays * 24 * 3600 / wkTickSeconds
	wkAccounts    = 7
	wkLagTicks    = 6 // 30-minute usage lag
	wkWarmupTicks = wkWeekTicks
	wkMeanBurn    = 0.0006 // weekly-window fraction per session per tick
)

type weeklySimPolicy struct {
	name string
	// admit, minHeadroom and gain replace weeklySurplusMinAdmit,
	// weeklySurplusMinHeadroom and weeklySurplusSpreadGain during the run.
	admit, minHeadroom, gain float64
	// transform rewrites each account's score before the scheduler is built,
	// for variants that change the sort key instead of the spread weights.
	transform func(score Score, resetAfter int64) Score
	// subFloorFallback lets a new session use the roomiest OAuth account under
	// MinNewSessionHeadroom before an API-key account (problem 2 variant).
	subFloorFallback bool
	// noAPIKey drops the API-key account from the pool.
	noAPIKey bool
}

type weeklySimResult struct {
	wasted       float64 // weekly windows' worth of quota still unused at reset (after warm-up)
	demanded     float64 // weekly windows' worth of demand placed after warm-up
	failovers    int     // sticky sessions whose account capped under them
	cacheMoves   int     // sticky sessions moved by the retention floor
	placements   int     // new-session placements onto subscription accounts
	apiKeyPlaced int     // new sessions sent to the API-key account
	allSubFloor  int     // placements made while every live OAuth account sat in [0.05, 0.40)
	maxShare     float64 // largest share of one account's placements over the run
	herdShare    float64 // mean top-account share of placements per 6h block (>=10 placements)
}

type weeklySimSession struct {
	account int // -1: API key
	left    int
	burn    float64
}

func poissonDraw(rng *rand.Rand, lambda float64) int {
	limit := math.Exp(-lambda)
	k, p := 0, rng.Float64()
	for p > limit {
		k++
		p *= rng.Float64()
	}
	return k
}

func runWeeklySim(policy weeklySimPolicy, seed int64, demand float64, meanDuration int) weeklySimResult {
	rng := rand.New(rand.NewSource(seed))
	pickRNG := rand.New(rand.NewSource(seed*7919 + 1))
	savedRand, savedAdmit, savedMin, savedGain := spreadRandFloat, weeklySurplusMinAdmit, weeklySurplusMinHeadroom, weeklySurplusSpreadGain
	spreadRandFloat = pickRNG.Float64
	weeklySurplusMinAdmit, weeklySurplusMinHeadroom, weeklySurplusSpreadGain = policy.admit, policy.minHeadroom, policy.gain
	defer func() {
		spreadRandFloat = savedRand
		weeklySurplusMinAdmit, weeklySurplusMinHeadroom, weeklySurplusSpreadGain = savedAdmit, savedMin, savedGain
	}()

	ids := make([]string, wkAccounts)
	candidates := make([]account.Account, 0, wkAccounts+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("sub-%d@example.com", i)
		candidates = append(candidates, account.Account{ID: ids[i], Provider: account.ProviderCodex, AuthMode: account.AuthModeOAuth})
	}
	apiKey := account.Account{ID: "api-key", Provider: account.ProviderCodex, AuthMode: account.AuthModeAPIKey}
	if !policy.noAPIKey {
		candidates = append(candidates, apiKey)
	}

	// Staggered resets; the pool starts at an even pace through each window.
	used := make([]float64, wkAccounts)
	resetAt := make([]int, wkAccounts)
	for i := range used {
		resetAt[i] = 1 + rng.Intn(wkWeekTicks)
		elapsed := 1 - float64(resetAt[i])/float64(wkWeekTicks)
		used[i] = math.Min(0.95, elapsed*demand*(0.6+0.8*rng.Float64()))
	}
	history := make([][]float64, 0, wkTicks)

	// Arrival rate so that weekly demand is demand * pool capacity.
	sessionsPerWeek := demand * wkAccounts / (wkMeanBurn * float64(meanDuration))
	baseRate := sessionsPerWeek / wkWeekTicks

	var sessions []*weeklySimSession
	placed := make([]int, wkAccounts)
	blockPlaced := make([]int, wkAccounts)
	blocks := 0
	var result weeklySimResult

	for tick := 0; tick < wkTicks; tick++ {
		for i := range used {
			if tick == resetAt[i] {
				if tick >= wkWarmupTicks {
					result.wasted += math.Max(0, 1-used[i])
				}
				used[i] = 0
				resetAt[i] += wkWeekTicks
			}
		}
		history = append(history, append([]float64(nil), used...))
		lagged := history[0]
		if len(history) > wkLagTicks {
			lagged = history[len(history)-1-wkLagTicks]
		}

		sessionCounts := map[string]int{}
		for _, session := range sessions {
			if session.account >= 0 {
				sessionCounts[ScoreKey(account.ProviderCodex, ids[session.account])]++
			}
		}
		scores := make([]Score, wkAccounts)
		usable := 0
		live, subFloor := 0, 0
		_ = usable
		for i := range scores {
			resetAfter := int64(resetAt[i]-tick) * wkTickSeconds
			percent := math.Floor(lagged[i] * 100)
			if used[i] >= 1 {
				percent = 100
			}
			score := ScoreFromLimitWindows(ids[i], 0, []LimitWindow{{
				Name: "primary", UsedPercent: percent,
				LimitWindowSeconds: wkWeekSeconds, ResetAfterSeconds: resetAfter,
			}})
			score.Provider = account.ProviderCodex
			if policy.transform != nil {
				score = policy.transform(score, resetAfter)
			}
			scores[i] = score
			if score.usableForNewSession() {
				usable++
			}
			if !score.exhausted() {
				live++
				if score.Headroom >= MinStickyRetentionHeadroom && score.Headroom < MinNewSessionHeadroom {
					subFloor++
				}
			}
		}
		scheduler := NewScheduler(scores).WithSessionCounts(sessionCounts)
		indexOf := func(id string) int {
			for i := range ids {
				if ids[i] == id {
					return i
				}
			}
			return -1
		}
		place := func(session *weeklySimSession) {
			picked, err := scheduler.Pick(candidates)
			if err != nil {
				panic(err)
			}
			if policy.subFloorFallback && picked.AuthMode == account.AuthModeAPIKey {
				best := -1
				for i, score := range scores {
					if score.usableForStickySession() && (best < 0 || score.Headroom > scores[best].Headroom) {
						best = i
					}
				}
				if best >= 0 {
					picked = candidates[best]
				}
			}
			if tick >= wkWarmupTicks && live > 0 && subFloor == live {
				result.allSubFloor++
			}
			if picked.AuthMode == account.AuthModeAPIKey {
				session.account = -1
				if tick >= wkWarmupTicks {
					result.apiKeyPlaced++
				}
				return
			}
			session.account = indexOf(picked.ID)
			if tick >= wkWarmupTicks {
				placed[session.account]++
				blockPlaced[session.account]++
				result.placements++
			}
		}

		// Arrivals follow a diurnal curve (peak mid-day, quiet at night).
		hour := float64(tick*wkTickSeconds%86400) / 3600
		rate := baseRate * (1 + 0.8*math.Sin(2*math.Pi*(hour-8)/24))
		for n := poissonDraw(rng, rate); n > 0; n-- {
			session := &weeklySimSession{
				left: 6 + rng.Intn(2*meanDuration-11),
				burn: wkMeanBurn * (0.5 + rng.Float64()),
			}
			place(session)
			sessions = append(sessions, session)
		}

		kept := sessions[:0]
		for _, session := range sessions {
			if session.account >= 0 {
				switch {
				case used[session.account] >= 1:
					if tick >= wkWarmupTicks {
						result.failovers++
					}
					place(session)
				case !scheduler.UsableForStickySession(account.ProviderCodex, ids[session.account]):
					if tick >= wkWarmupTicks {
						result.cacheMoves++
					}
					place(session)
				}
			}
			if session.account >= 0 {
				used[session.account] += session.burn
				if tick >= wkWarmupTicks {
					result.demanded += session.burn
				}
			} else if tick >= wkWarmupTicks {
				result.demanded += session.burn
			}
			session.left--
			if session.left > 0 {
				kept = append(kept, session)
			}
		}
		sessions = kept

		if (tick+1)%(6*3600/wkTickSeconds) == 0 {
			total, top := 0, 0
			for i, count := range blockPlaced {
				total += count
				top = max(top, count)
				blockPlaced[i] = 0
			}
			if total >= 10 {
				result.herdShare += float64(top) / float64(total)
				blocks++
			}
		}
	}
	if blocks > 0 {
		result.herdShare /= float64(blocks)
	}
	for _, count := range placed {
		if result.placements > 0 {
			if share := float64(count) / float64(result.placements); share > result.maxShare {
				result.maxShare = share
			}
		}
	}
	return result
}

type weeklySimTotals struct {
	wasted, demanded                  float64
	failovers, cacheMoves, apiKey     int
	placements, allSubFloor           int
	meanMaxShare, herdShare, worstMax float64
}

func runWeeklySimSeeds(policy weeklySimPolicy, demand float64, meanDuration, seeds int) weeklySimTotals {
	var totals weeklySimTotals
	for seed := int64(1); seed <= int64(seeds); seed++ {
		r := runWeeklySim(policy, seed, demand, meanDuration)
		totals.wasted += r.wasted
		totals.demanded += r.demanded
		totals.failovers += r.failovers
		totals.cacheMoves += r.cacheMoves
		totals.apiKey += r.apiKeyPlaced
		totals.placements += r.placements
		totals.allSubFloor += r.allSubFloor
		totals.meanMaxShare += r.maxShare / float64(seeds)
		totals.herdShare += r.herdShare / float64(seeds)
		totals.worstMax = math.Max(totals.worstMax, r.maxShare)
	}
	return totals
}

// weeklyPressure is the naive fix: the Fable weekly drain pressure applied to
// every weekly window, continuous in remaining/resetAfter.
func weeklyNaivePressure(score Score, resetAfter int64) Score {
	score.ExpiryPressure += expiryPressure(score.WeeklyHeadroom, resetAfter)
	score.WeeklySurplus = 0
	return score
}

// weeklyGatedPressure adds pressure only for quota that will clearly go
// unused before reset (a positive unpaced surplus).
func weeklyGatedPressure(score Score, resetAfter int64) Score {
	if score.WeeklySurplus > 0 {
		score.ExpiryPressure += expiryPressure(score.WeeklySurplus, resetAfter)
	}
	score.WeeklySurplus = 0
	return score
}

// weeklyBucketedPressure rounds the surplus into quarter buckets so equally
// at-risk accounts still share a band.
func weeklyBucketedPressure(score Score, _ int64) Score {
	score.ExpiryPressure += math.Floor(score.WeeklySurplus*4) * 1e-9
	score.WeeklySurplus = 0
	return score
}

func weeklyBaseline(score Score, _ int64) Score {
	score.WeeklySurplus = 0
	return score
}

func weeklySimPolicies() []weeklySimPolicy {
	off := math.Inf(1)
	policies := []weeklySimPolicy{
		{name: "baseline", admit: off, transform: weeklyBaseline},
		{name: "sort:naive-pressure", admit: off, transform: weeklyNaivePressure},
		{name: "sort:gated-pressure", admit: off, transform: weeklyGatedPressure},
		{name: "sort:bucketed-surplus", admit: off, transform: weeklyBucketedPressure},
		{name: "fallback:roomiest-sub-floor", admit: off, transform: weeklyBaseline, subFloorFallback: true},
		{name: "spread-only:g=4", admit: off, gain: 4},
		{name: "no-api-key:baseline", admit: off, transform: weeklyBaseline, noAPIKey: true},
		{name: "no-api-key:admit(shipped)", admit: 0.10, minHeadroom: 0.15, gain: 4, noAPIKey: true},
	}
	for _, knobs := range [][3]float64{
		{0.05, 0.10, 1}, {0.05, 0.10, 4},
		{0.05, 0.15, 1}, {0.05, 0.15, 2}, {0.05, 0.15, 4}, {0.05, 0.15, 8},
		{0.10, 0.15, 1}, {0.10, 0.15, 4},
		{0.20, 0.15, 4},
	} {
		policies = append(policies, weeklySimPolicy{
			name:  fmt.Sprintf("admit:s>=%.2f,h>=%.2f,g=%.0f", knobs[0], knobs[1], knobs[2]),
			admit: knobs[0], minHeadroom: knobs[1], gain: knobs[2],
		})
	}
	return policies
}

// TestWeeklyExpiryPolicyExperiment prints the policy table (run with -v).
func TestWeeklyExpiryPolicyExperiment(t *testing.T) {
	if os.Getenv("SUBROUTER_SIM_TABLE") == "" {
		t.Skip("set SUBROUTER_SIM_TABLE=1 to print the experiment table")
	}
	const seeds = 24
	for _, scenario := range []struct {
		demand   float64
		duration int
	}{{0.6, 27}, {0.85, 27}, {1.0, 27}, {1.2, 27}, {0.85, 96}, {1.0, 96}} {
		demand := scenario.demand
		t.Logf("== demand %.2f of pool capacity, mean session %d min, %d seeds ==", demand, scenario.duration*5, seeds)
		for _, policy := range weeklySimPolicies() {
			r := runWeeklySimSeeds(policy, demand, scenario.duration, seeds)
			t.Logf("%-24s wasted=%6.2f (%.1f%%) failovers=%5d cacheMoves=%5d apiKey=%5d allSubFloor=%5d maxShare=%.2f(worst %.2f) herd6h=%.2f",
				policy.name, r.wasted, 100*r.wasted/float64(seeds*wkAccounts*3), r.failovers, r.cacheMoves, r.apiKey, r.allSubFloor, r.meanMaxShare, r.worstMax, r.herdShare)
		}
	}
}
