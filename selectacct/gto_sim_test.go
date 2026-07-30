package selectacct

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"testing"
)

// GTO policy comparison v2. The v1 experiment (velocity_sim_test.go) modeled
// only quota drain and chose the shipped live-debit policy. This harness adds
// the second failure mode observed live: Anthropic burst-limits CONCURRENT
// streams per account (healthy accounts return headerless 429s under fast
// parallel traffic), so requests have a duration and each account has a
// concurrent-stream cap. "3 people each running a fleet of coding agents" is
// exactly the workload where a policy can lose to concurrency even with quota
// left everywhere.
//
// Signals available to a policy, mirroring what production has (or could
// cheaply have):
//   - snapHeadroom: refresh-stale quota headroom (5-minute-old snapshot)
//   - reqsSinceSnap: live per-account requests routed since the snapshot
//   - inflight: live per-account concurrent streams (production equivalent:
//     ActiveSessions tracked per account — not built yet, cheap to add)
//   - stickySessions: sticky assignment counts (the TTL-less session store)
//
// Run with: go test ./internal/selectacct -run TestGTOPolicyComparison -v
//
// CONCLUSION (2026-07-04, 32 seeds x 3 loads + 64-seed finalist run): the
// shipped debited-headroom policy is already GTO on user-visible outcomes.
// Below hard saturation it produces ZERO client-visible failures at every
// load tested; at hard saturation (24-thread fleet vs 12 accounts) failures
// are capacity-bound and every alternative is within noise (the l=0.15
// inflight-penalty finalist won clientFail on only 31/64 seeds). Alternatives
// ranked: power-of-two-choices and weighted-random are strictly worse (they
// discard headroom information); per-user partitioning is worse on both
// throughput and total failures (idle shards waste quota a shared pool would
// use) though mildly fairer to the lightest user at saturation;
// least-inflight ties current. The one robust marginal win is an inflight
// penalty (debited headroom - 0.15*inflight): -84% burst-429 churn and ~half
// the failover retry churn at heavy load, zero client-visible difference. Not
// shipped: it requires new per-account inflight tracking in the proxy and
// buys latency/churn, not outcomes. Revisit if truthful give-up/burst logs
// (v0.1.33) show burst-429 churn becoming user-visible.

const (
	gtoStepSeconds   = 10
	gtoHours         = 24
	gtoAccounts      = 12
	gtoRefreshSteps  = 30 // 5-minute usage refresh, as in production
	gtoInflightCap   = 6  // per-account concurrent streams before burst 429s
	gtoRefillPerStep = float64(gtoStepSeconds) / (5 * 60 * 60)
)

type gtoState struct {
	snapHeadroom  []float64
	reqsSinceSnap []int
	inflight      []int
	sticky        []int
}

type gtoPolicy struct {
	name string
	// pick chooses an account for a session that needs (re)placement. user is
	// the requesting user's index (only the partition policy uses it). tried
	// holds accounts already rejected for THIS request.
	pick func(state gtoState, user int, tried map[int]bool, rng *rand.Rand) int
}

type gtoThread struct {
	user       int
	account    int
	burstLeft  int
	launchDone int
	busyUntil  int // step when the in-flight request completes
	inflightOn int // account currently holding this thread's stream, -1 none
}

type gtoResult struct {
	served      int
	quotaReject int // request hit an account with no quota left
	burstReject int // request hit the concurrency cap on a healthy account
	clientFail  int // request could not be placed after retries (chain/paid tier)
	maxInflight int
	// perUserFail[i] = clientFail count for user i (fairness: does the heavy
	// fleet starve the light users?)
	perUserFail [3]int
}

func runGTOSim(policy gtoPolicy, seed int64, heavyThreads int) gtoResult {
	rng := rand.New(rand.NewSource(seed))
	steps := gtoHours * 3600 / gtoStepSeconds

	used := make([]float64, gtoAccounts)
	snapUsed := make([]float64, gtoAccounts)
	reqsSinceSnap := make([]int, gtoAccounts)
	inflight := make([]int, gtoAccounts)
	sticky := make([]int, gtoAccounts)

	threadCounts := []int{heavyThreads, 4, 3}
	var threads []*gtoThread
	userNextBurst := make([]int, len(threadCounts))
	userLaunchSeq := make([]int, len(threadCounts))
	for user, n := range threadCounts {
		userNextBurst[user] = rng.Intn(180)
		for i := 0; i < n; i++ {
			threads = append(threads, &gtoThread{user: user, account: -1, inflightOn: -1})
		}
	}

	result := gtoResult{}

	place := func(thread *gtoThread, rng *rand.Rand) bool {
		// Mirror production: policy pick, bounded retries over its next
		// choices, count each rejection by cause. Failure = chain/paid tier.
		tried := map[int]bool{}
		for attempt := 0; attempt < 4; attempt++ {
			state := gtoState{snapHeadroom: make([]float64, gtoAccounts), reqsSinceSnap: reqsSinceSnap, inflight: inflight, sticky: sticky}
			for i := range snapUsed {
				state.snapHeadroom[i] = 1 - snapUsed[i]
			}
			pick := policy.pick(state, thread.user, tried, rng)
			if pick < 0 {
				break
			}
			tried[pick] = true
			if used[pick] >= 1 {
				result.quotaReject++
				continue
			}
			if inflight[pick] >= gtoInflightCap {
				result.burstReject++
				continue
			}
			thread.account = pick
			sticky[pick]++
			return true
		}
		result.clientFail++
		result.perUserFail[thread.user]++
		return false
	}

	for step := 0; step < steps; step++ {
		for i := range used {
			used[i] = math.Max(0, used[i]-gtoRefillPerStep)
		}
		if step%gtoRefreshSteps == 0 {
			for i := range used {
				snapUsed[i] = used[i]
				reqsSinceSnap[i] = 0
			}
		}
		for user := range userNextBurst {
			if step == userNextBurst[user] {
				userLaunchSeq[user]++
			}
		}

		for _, thread := range threads {
			// Finish an in-flight request.
			if thread.inflightOn >= 0 && step >= thread.busyUntil {
				inflight[thread.inflightOn]--
				thread.inflightOn = -1
			}
			if thread.inflightOn >= 0 {
				continue // still streaming
			}
			if step < userNextBurst[thread.user] {
				continue
			}
			if thread.burstLeft == 0 {
				if thread.launchDone >= userLaunchSeq[thread.user] {
					continue
				}
				thread.burstLeft = 20 + rng.Intn(40)
				thread.launchDone = userLaunchSeq[thread.user]
			}
			// Agent think time between requests.
			if rng.Float64() > 0.6 {
				continue
			}

			// Sticky reuse: keep the account unless it can't serve right now.
			if thread.account >= 0 && (used[thread.account] >= 1 || inflight[thread.account] >= gtoInflightCap) {
				if used[thread.account] >= 1 {
					result.quotaReject++
				} else {
					result.burstReject++
				}
				sticky[thread.account]--
				thread.account = -1
			}
			if thread.account < 0 {
				if !place(thread, rng) {
					thread.burstLeft--
					continue
				}
			}

			cost := 0.0015 + rng.Float64()*0.0035
			used[thread.account] += cost
			reqsSinceSnap[thread.account]++
			inflight[thread.account]++
			if inflight[thread.account] > result.maxInflight {
				result.maxInflight = inflight[thread.account]
			}
			thread.inflightOn = thread.account
			thread.busyUntil = step + 1 + rng.Intn(5) // 10-60s per request
			result.served++
			thread.burstLeft--
			if thread.burstLeft == 0 {
				allDone := true
				for _, other := range threads {
					if other.user == thread.user && other.burstLeft > 0 {
						allDone = false
						break
					}
				}
				if allDone {
					userNextBurst[thread.user] = step + 120 + rng.Intn(300)
				}
			}
		}
	}
	return result
}

// debitedHeadroom is the shipped production signal: snapshot headroom minus
// LiveDebitPerRequest per request routed since the snapshot.
func debitedHeadroom(state gtoState, i int) float64 {
	return state.snapHeadroom[i] - LiveDebitPerRequest*float64(state.reqsSinceSnap[i])
}

func gtoCurrentPolicy() gtoPolicy {
	return gtoPolicy{
		name: "current(debited-headroom)",
		pick: func(state gtoState, _ int, tried map[int]bool, _ *rand.Rand) int {
			return gtoArgBest(state, tried, func(i, j int) bool {
				hi, hj := debitedHeadroom(state, i), debitedHeadroom(state, j)
				if hi != hj {
					return hi > hj
				}
				return state.sticky[i] < state.sticky[j]
			})
		},
	}
}

func gtoInflightPenaltyPolicy(lambda float64) gtoPolicy {
	return gtoPolicy{
		name: fmt.Sprintf("debited+inflight(l=%.2f)", lambda),
		pick: func(state gtoState, _ int, tried map[int]bool, _ *rand.Rand) int {
			return gtoArgBest(state, tried, func(i, j int) bool {
				hi := debitedHeadroom(state, i) - lambda*float64(state.inflight[i])
				hj := debitedHeadroom(state, j) - lambda*float64(state.inflight[j])
				if hi != hj {
					return hi > hj
				}
				return state.inflight[i] < state.inflight[j]
			})
		},
	}
}

func gtoLeastInflightPolicy() gtoPolicy {
	return gtoPolicy{
		name: "least-inflight",
		pick: func(state gtoState, _ int, tried map[int]bool, _ *rand.Rand) int {
			return gtoArgBest(state, tried, func(i, j int) bool {
				if state.inflight[i] != state.inflight[j] {
					return state.inflight[i] < state.inflight[j]
				}
				return debitedHeadroom(state, i) > debitedHeadroom(state, j)
			})
		},
	}
}

func gtoPowerOfTwoPolicy() gtoPolicy {
	return gtoPolicy{
		name: "power-of-two-choices",
		pick: func(state gtoState, _ int, tried map[int]bool, rng *rand.Rand) int {
			var open []int
			for i := 0; i < gtoAccounts; i++ {
				if !tried[i] {
					open = append(open, i)
				}
			}
			if len(open) == 0 {
				return -1
			}
			a := open[rng.Intn(len(open))]
			b := open[rng.Intn(len(open))]
			scoreA := debitedHeadroom(state, a) - 0.05*float64(state.inflight[a])
			scoreB := debitedHeadroom(state, b) - 0.05*float64(state.inflight[b])
			if scoreB > scoreA {
				return b
			}
			return a
		},
	}
}

func gtoWeightedRandomPolicy(k float64) gtoPolicy {
	return gtoPolicy{
		name: fmt.Sprintf("weighted-random(k=%.0f)", k),
		pick: func(state gtoState, _ int, tried map[int]bool, rng *rand.Rand) int {
			weights := make([]float64, gtoAccounts)
			total := 0.0
			for i := 0; i < gtoAccounts; i++ {
				if tried[i] {
					continue
				}
				w := math.Max(0, debitedHeadroom(state, i))
				w = math.Pow(w, k)
				weights[i] = w
				total += w
			}
			if total <= 0 {
				return -1
			}
			r := rng.Float64() * total
			for i, w := range weights {
				r -= w
				if r <= 0 && w > 0 {
					return i
				}
			}
			return -1
		},
	}
}

func gtoPartitionPolicy() gtoPolicy {
	return gtoPolicy{
		name: "per-user-partition(+overflow)",
		pick: func(state gtoState, user int, tried map[int]bool, _ *rand.Rand) int {
			better := func(i, j int) bool {
				hi := debitedHeadroom(state, i) - 0.05*float64(state.inflight[i])
				hj := debitedHeadroom(state, j) - 0.05*float64(state.inflight[j])
				return hi > hj
			}
			best := -1
			// Own shard first (accounts where idx%3 == user)...
			for i := 0; i < gtoAccounts; i++ {
				if tried[i] || i%3 != user {
					continue
				}
				if debitedHeadroom(state, i) <= 0.05 {
					continue
				}
				if best == -1 || better(i, best) {
					best = i
				}
			}
			if best >= 0 {
				return best
			}
			// ...overflow anywhere.
			for i := 0; i < gtoAccounts; i++ {
				if tried[i] {
					continue
				}
				if best == -1 || better(i, best) {
					best = i
				}
			}
			return best
		},
	}
}

func gtoArgBest(state gtoState, tried map[int]bool, less func(i, j int) bool) int {
	idx := make([]int, 0, gtoAccounts)
	for i := 0; i < gtoAccounts; i++ {
		if !tried[i] {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return -1
	}
	sort.SliceStable(idx, func(a, b int) bool { return less(idx[a], idx[b]) })
	return idx[0]
}

func TestGTOPolicyComparison(t *testing.T) {
	policies := []gtoPolicy{
		gtoCurrentPolicy(),
		gtoInflightPenaltyPolicy(0.02),
		gtoInflightPenaltyPolicy(0.05),
		gtoInflightPenaltyPolicy(0.08),
		gtoInflightPenaltyPolicy(0.10),
		gtoInflightPenaltyPolicy(0.15),
		gtoInflightPenaltyPolicy(0.25),
		gtoLeastInflightPolicy(),
		gtoPartitionPolicy(),
	}
	seeds := make([]int64, 32)
	for i := range seeds {
		seeds[i] = int64(i + 1)
	}
	for _, load := range []struct {
		name         string
		heavyThreads int
	}{
		{"moderate(12-thread fleet)", 12},
		{"heavy(18-thread fleet)", 18},
		{"extreme(24-thread fleet)", 24},
	} {
		t.Logf("== load: %s ==", load.name)
		for _, policy := range policies {
			agg := gtoResult{}
			for _, seed := range seeds {
				r := runGTOSim(policy, seed, load.heavyThreads)
				agg.served += r.served
				agg.quotaReject += r.quotaReject
				agg.burstReject += r.burstReject
				agg.clientFail += r.clientFail
				if r.maxInflight > agg.maxInflight {
					agg.maxInflight = r.maxInflight
				}
				for u := range r.perUserFail {
					agg.perUserFail[u] += r.perUserFail[u]
				}
			}
			t.Logf("%-30s served=%7d quotaRej=%6d burstRej=%6d clientFail=%5d perUserFail=%v maxInflight=%d",
				policy.name, agg.served, agg.quotaReject, agg.burstReject, agg.clientFail, agg.perUserFail, agg.maxInflight)
		}
	}
}

// Focused high-seed confirmation of the finalist against the shipped policy at
// the saturated load, where the 32-seed grid was noisy.
func TestGTOFinalistConfirmation(t *testing.T) {
	finalists := []gtoPolicy{
		gtoCurrentPolicy(),
		gtoInflightPenaltyPolicy(0.15),
	}
	perSeedWins := 0
	totals := map[string]*gtoResult{}
	for seed := int64(1); seed <= 64; seed++ {
		var perSeed [2]gtoResult
		for i, policy := range finalists {
			r := runGTOSim(policy, seed, 24)
			perSeed[i] = r
			agg, ok := totals[policy.name]
			if !ok {
				agg = &gtoResult{}
				totals[policy.name] = agg
			}
			agg.served += r.served
			agg.quotaReject += r.quotaReject
			agg.burstReject += r.burstReject
			agg.clientFail += r.clientFail
		}
		if perSeed[1].clientFail <= perSeed[0].clientFail {
			perSeedWins++
		}
	}
	for _, policy := range finalists {
		agg := totals[policy.name]
		t.Logf("%-28s served=%7d quotaRej=%6d burstRej=%5d clientFail=%6d", policy.name, agg.served, agg.quotaReject, agg.burstReject, agg.clientFail)
	}
	t.Logf("inflight(0.15) wins-or-ties clientFail on %d/64 seeds", perSeedWins)
}
