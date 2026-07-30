package selectacct

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"testing"
)

// This file is a routing-policy experiment, kept as a deterministic test so
// the chosen production constants stay justified by a reproducible workload.
//
// The modeled phenomenon: the scheduler only refreshes usage scores every few
// minutes, so between refreshes it routes on stale snapshots. An account that
// several concurrent threads are hammering looks healthy at refresh time and
// is cooked minutes later; every new session routed there in between burns a
// failover. A velocity term (consumption rate observed between refreshes)
// projects the snapshot forward and steers new sessions away from
// fast-draining accounts before they cook.
//
// Workload shape mirrors the real deployment: ~3 people share the pool, one
// running a large agent fleet, the others lighter, all bursty.

const (
	simStepSeconds       = 10
	simHours             = 24
	simAccounts          = 12
	simRefreshEverySteps = 30 // 5 minutes, matches the server's usage refresh cadence
	// fiveHourRefillPerStep approximates the rolling 5h window as a leaky
	// bucket: consumed fraction drains back at cap/5h.
	fiveHourRefillPerStep = float64(simStepSeconds) / (5 * 60 * 60)
)

type simPolicy struct {
	name string
	// pick returns the index of the account a new/rerouted session should use.
	// snapHeadroom/velocity are refresh-stale. staleSessions mirrors the
	// production session store (no TTL, counts dead sessions). activeSessions
	// counts sessions with recent activity. reqsSinceSnap counts requests the
	// router itself sent to each account since the last snapshot (live).
	pick func(snapHeadroom, velocityPerMin []float64, staleSessions, activeSessions, reqsSinceSnap []int, rng *rand.Rand) int
}

type simThread struct {
	user       int
	account    int
	burstLeft  int // requests remaining in the current burst
	launchDone int // last fleet-launch sequence this thread completed
	lastActive int // step of this thread's most recent request
}

type simResult struct {
	rejects     int // requests that hit a cooked account (user-visible failover/429)
	served      int
	unservable  int     // requests when every account was cooked (no policy can win these)
	maxSessions int     // worst concurrent sessions on one account
	usedStddev  float64 // imbalance of total consumption across accounts
}

func runVelocitySim(policy simPolicy, seed int64) simResult {
	rng := rand.New(rand.NewSource(seed))
	steps := simHours * 3600 / simStepSeconds

	used := make([]float64, simAccounts)       // live consumed fraction of the 5h window
	totalUsed := make([]float64, simAccounts)  // lifetime consumption per account
	snapUsed := make([]float64, simAccounts)   // refresh-stale snapshot the policy sees
	velocity := make([]float64, simAccounts)   // EWMA consumed-fraction per minute
	sessions := make([]int, simAccounts)       // sticky assignment counts (store semantics, no TTL)
	reqsSinceSnap := make([]int, simAccounts)  // requests routed per account since last snapshot
	activeSessions := make([]int, simAccounts) // sessions with activity in the last 15 min

	// 3 users: one heavy fleet, two lighter. Threads are sticky sessions.
	// A user's threads burst TOGETHER (a person launches an agent fleet at
	// once): correlated bursts are what create the consumption spikes a stale
	// snapshot misses and a velocity term catches.
	threadCounts := []int{12, 4, 3}
	var threads []*simThread
	userNextBurst := make([]int, len(threadCounts))
	userLaunchSeq := make([]int, len(threadCounts))
	for user, n := range threadCounts {
		userNextBurst[user] = rng.Intn(180)
		for i := 0; i < n; i++ {
			threads = append(threads, &simThread{user: user, account: -1})
		}
	}

	result := simResult{}
	refreshMinutes := float64(simRefreshEverySteps*simStepSeconds) / 60

	for step := 0; step < steps; step++ {
		// Refill the rolling windows.
		for i := range used {
			used[i] = math.Max(0, used[i]-fiveHourRefillPerStep)
		}
		// Periodic score refresh: snapshot usage, update velocity EWMA.
		if step%simRefreshEverySteps == 0 {
			for i := range used {
				rawNow := (used[i] - snapUsed[i]) / refreshMinutes
				if rawNow < 0 {
					rawNow = 0
				}
				velocity[i] = 0.5*velocity[i] + 0.5*rawNow
				snapUsed[i] = used[i]
				reqsSinceSnap[i] = 0
			}
		}
		// Recompute active-session counts (sessions with a request in the
		// last 15 minutes), the TTL'd counterpart of the stale store counts.
		for i := range activeSessions {
			activeSessions[i] = 0
		}
		for _, other := range threads {
			if other.account != -1 && step-other.lastActive <= 90 {
				activeSessions[other.account]++
			}
		}

		// A user's launch opens when its scheduled step arrives; each thread
		// runs exactly one burst per launch.
		for user := range userNextBurst {
			if step == userNextBurst[user] {
				userLaunchSeq[user]++
			}
		}
		for _, thread := range threads {
			if step < userNextBurst[thread.user] {
				continue
			}
			if thread.burstLeft == 0 {
				if thread.launchDone >= userLaunchSeq[thread.user] {
					continue // already ran this launch; waiting for the next one
				}
				// The whole fleet bursts together: 30-100 requests per thread.
				thread.burstLeft = 30 + rng.Intn(70)
				thread.launchDone = userLaunchSeq[thread.user]
			}
			// One request this step with p=0.5 (agent think time).
			if rng.Float64() > 0.5 {
				continue
			}
			cost := 0.001 + rng.Float64()*0.004 // 0.1%-0.5% of the 5h window per request

			if thread.account == -1 || used[thread.account] >= 1 {
				if thread.account != -1 {
					// Sticky session hit a cooked account: user-visible failover.
					result.rejects++
					sessions[thread.account]--
					thread.account = -1
				}
				snapHeadroom := make([]float64, simAccounts)
				anyOpen := false
				for i := range snapUsed {
					snapHeadroom[i] = 1 - snapUsed[i]
					if used[i] < 1 {
						anyOpen = true
					}
				}
				if !anyOpen {
					result.unservable++
					continue
				}
				pick := policy.pick(snapHeadroom, velocity, sessions, activeSessions, reqsSinceSnap, rng)
				if used[pick] >= 1 {
					// Policy routed into a cooked account (stale snapshot).
					result.rejects++
					// One bounded retry on the live-best account, mirroring failover.
					best := -1
					for i := range used {
						if used[i] < 1 && (best == -1 || used[i] < used[best]) {
							best = i
						}
					}
					pick = best
				}
				thread.account = pick
				sessions[pick]++
			}

			used[thread.account] += cost
			totalUsed[thread.account] += cost
			reqsSinceSnap[thread.account]++
			thread.lastActive = step
			result.served++
			if sessions[thread.account] > result.maxSessions {
				result.maxSessions = sessions[thread.account]
			}
			thread.burstLeft--
			if thread.burstLeft == 0 {
				// Burst ends but the session stays sticky (and idle) on its
				// account, like the real session store: session counts include
				// idle sessions, which is exactly where a pure session-count
				// penalty over-penalizes and velocity tells hot from held.
				allDone := true
				for _, other := range threads {
					if other.user == thread.user && other.burstLeft > 0 {
						allDone = false
						break
					}
				}
				if allDone {
					userNextBurst[thread.user] = step + 120 + rng.Intn(300) // 20-70 min idle
				}
			}
		}
	}

	mean := 0.0
	for _, u := range totalUsed {
		mean += u
	}
	mean /= float64(len(totalUsed))
	variance := 0.0
	for _, u := range totalUsed {
		variance += (u - mean) * (u - mean)
	}
	result.usedStddev = math.Sqrt(variance / float64(len(totalUsed)))
	return result
}

func headroomPickPolicy() simPolicy {
	return simPolicy{
		name: "current(headroom+staleSessions)",
		pick: func(snapHeadroom, _ []float64, staleSessions, _, _ []int, _ *rand.Rand) int {
			return argBest(snapHeadroom, func(i, j int) bool {
				if snapHeadroom[i] != snapHeadroom[j] {
					return snapHeadroom[i] > snapHeadroom[j]
				}
				return staleSessions[i] < staleSessions[j]
			})
		},
	}
}

func activeSessionsPickPolicy() simPolicy {
	return simPolicy{
		name: "headroom+activeSessions",
		pick: func(snapHeadroom, _ []float64, _, activeSessions, _ []int, _ *rand.Rand) int {
			return argBest(snapHeadroom, func(i, j int) bool {
				if snapHeadroom[i] != snapHeadroom[j] {
					return snapHeadroom[i] > snapHeadroom[j]
				}
				return activeSessions[i] < activeSessions[j]
			})
		},
	}
}

// liveDebitPickPolicy debits the snapshot headroom by the requests the router
// itself sent since that snapshot (perRequestDebit = assumed mean cost as a
// window fraction). This is the live form of "velocity": it needs no estimator
// and updates instantly, so within-refresh herding self-corrects.
func liveDebitPickPolicy(perRequestDebit float64) simPolicy {
	return simPolicy{
		name: fmt.Sprintf("liveDebit(d=%.3f)+activeSessions", perRequestDebit),
		pick: func(snapHeadroom, _ []float64, _, activeSessions, reqsSinceSnap []int, _ *rand.Rand) int {
			projected := make([]float64, len(snapHeadroom))
			for i := range snapHeadroom {
				projected[i] = snapHeadroom[i] - perRequestDebit*float64(reqsSinceSnap[i])
			}
			return argBest(projected, func(i, j int) bool {
				if projected[i] != projected[j] {
					return projected[i] > projected[j]
				}
				return activeSessions[i] < activeSessions[j]
			})
		},
	}
}

func liveDebitVelocityPickPolicy(perRequestDebit, horizonMinutes float64) simPolicy {
	return simPolicy{
		name: fmt.Sprintf("liveDebit(d=%.3f)+velocity(h=%.0fm)", perRequestDebit, horizonMinutes),
		pick: func(snapHeadroom, velocityPerMin []float64, _, activeSessions, reqsSinceSnap []int, _ *rand.Rand) int {
			projected := make([]float64, len(snapHeadroom))
			for i := range snapHeadroom {
				projected[i] = snapHeadroom[i] - perRequestDebit*float64(reqsSinceSnap[i]) - velocityPerMin[i]*horizonMinutes
			}
			return argBest(projected, func(i, j int) bool {
				if projected[i] != projected[j] {
					return projected[i] > projected[j]
				}
				return activeSessions[i] < activeSessions[j]
			})
		},
	}
}

func liveDebitActivePenaltyPickPolicy(perRequestDebit, sessionPenalty float64) simPolicy {
	return simPolicy{
		name: fmt.Sprintf("liveDebit(d=%.3f)+activePenalty(k=%.2f)", perRequestDebit, sessionPenalty),
		pick: func(snapHeadroom, _ []float64, _, activeSessions, reqsSinceSnap []int, _ *rand.Rand) int {
			projected := make([]float64, len(snapHeadroom))
			for i := range snapHeadroom {
				projected[i] = snapHeadroom[i] - perRequestDebit*float64(reqsSinceSnap[i]) - sessionPenalty*float64(activeSessions[i])
			}
			return argBest(projected, func(i, j int) bool {
				if projected[i] != projected[j] {
					return projected[i] > projected[j]
				}
				return activeSessions[i] < activeSessions[j]
			})
		},
	}
}

func velocityPickPolicy(horizonMinutes float64) simPolicy {
	return simPolicy{
		name: fmt.Sprintf("velocity(h=%.0fm)", horizonMinutes),
		pick: func(snapHeadroom, velocityPerMin []float64, staleSessions, _, _ []int, _ *rand.Rand) int {
			projected := make([]float64, len(snapHeadroom))
			for i := range snapHeadroom {
				projected[i] = snapHeadroom[i] - velocityPerMin[i]*horizonMinutes
			}
			return argBest(projected, func(i, j int) bool {
				if projected[i] != projected[j] {
					return projected[i] > projected[j]
				}
				return staleSessions[i] < staleSessions[j]
			})
		},
	}
}

func randomPickPolicy() simPolicy {
	return simPolicy{
		name: "random",
		pick: func(snapHeadroom []float64, _ []float64, _, _, _ []int, rng *rand.Rand) int {
			return rng.Intn(len(snapHeadroom))
		},
	}
}

func argBest(values []float64, less func(i, j int) bool) int {
	idx := make([]int, len(values))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return less(idx[a], idx[b]) })
	return idx[0]
}

// TestVelocityPolicyExperiment runs the policy comparison across seeds and
// prints the table (go test -v -run TestVelocityPolicyExperiment). It asserts
// the production choice (velocity projection with the shipped horizon plus the
// shipped session penalty) does not lose to the pre-velocity policy on
// user-visible rejects, so the constants stay justified.
func TestVelocityPolicyExperiment(t *testing.T) {
	policies := []simPolicy{
		randomPickPolicy(),
		headroomPickPolicy(),
		activeSessionsPickPolicy(),
		velocityPickPolicy(VelocityProjectionMinutes),
		liveDebitPickPolicy(0.006),
		liveDebitPickPolicy(LiveDebitPerRequest),
		liveDebitPickPolicy(0.020),
		liveDebitPickPolicy(0.050),
		liveDebitPickPolicy(0.100),
		liveDebitPickPolicy(0.300),
	}
	seeds := make([]int64, 24)
	for i := range seeds {
		seeds[i] = int64(i + 1)
	}

	totals := make(map[string]*simResult)
	for _, policy := range policies {
		agg := &simResult{}
		for _, seed := range seeds {
			r := runVelocitySim(policy, seed)
			agg.rejects += r.rejects
			agg.served += r.served
			agg.unservable += r.unservable
			if r.maxSessions > agg.maxSessions {
				agg.maxSessions = r.maxSessions
			}
			agg.usedStddev += r.usedStddev
		}
		agg.usedStddev /= float64(len(seeds))
		totals[policy.name] = agg
		t.Logf("%-34s rejects=%5d served=%6d unservable=%4d maxSessions=%2d imbalance=%.4f",
			policy.name, agg.rejects, agg.served, agg.unservable, agg.maxSessions, agg.usedStddev)
	}

	current := totals["current(headroom+staleSessions)"]
	shipped := totals[fmt.Sprintf("liveDebit(d=%.3f)+activeSessions", LiveDebitPerRequest)]
	if shipped == nil {
		t.Fatal("shipped policy missing from experiment table")
	}
	if shipped.rejects > current.rejects {
		t.Fatalf("shipped liveDebit policy rejects=%d worse than current=%d; revisit LiveDebitPerRequest",
			shipped.rejects, current.rejects)
	}
}
