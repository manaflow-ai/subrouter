package proxy

import (
	"fmt"
	"math/rand"
	"os"
	"sort"
	"testing"
	"time"
)

// Simulation of the capacity hold-out rules against a synthetic pool. Not a
// regression test: run with SUBROUTER_CAPACITY_SIM=1 go test ./internal/proxy
// -run TestCodexCapacitySimulation -v. It drives the real codexCapacityStats
// and codexOverloadReroutes with an injected clock and prints, per scenario
// and policy, what clients see and how much of the pool is held out.

type simAccount struct {
	id   string
	p    func(t time.Duration) float64 // true per-turn capacity failure probability
	held time.Time
}

type simSession struct {
	id        string
	ws        bool
	account   int
	nextTurn  time.Duration
	turnStart time.Duration
	failing   bool
	waited    []time.Duration
	failures  int
	reroutes  int
	rerouteAt []time.Duration
}

type simPolicy struct {
	name          string
	flat          bool // base mark only, no escalation, no taint, no persistent reroute
	taintEpisodes int
	taintRate     float64
	taintHoldout  time.Duration
	capFraction   float64
}

type simResult struct {
	turns, clientFailures, upstreamFailures int
	waits                                   []time.Duration
	heldSamples                             []float64
	trueTaints, falseTaints                 int
	noAlternate                             int
}

func simScenario(name string, n int) (string, []*simAccount) {
	accts := make([]*simAccount, n)
	base := func(i int) float64 {
		switch {
		case i < n*3/4:
			return 0.005
		case i < n*9/10:
			return 0.03
		case i < n-1:
			return 0.10
		default:
			return 0.16
		}
	}
	for i := range accts {
		i := i
		var p func(time.Duration) float64
		switch name {
		case "today":
			p = func(time.Duration) float64 { return base(i) }
		case "regional-blip-15m":
			p = func(t time.Duration) float64 {
				if t >= 30*time.Minute && t < 45*time.Minute && i%5 != 0 {
					return 0.6
				}
				return base(i)
			}
		case "global-overload-20m":
			p = func(t time.Duration) float64 {
				if t >= 30*time.Minute && t < 50*time.Minute {
					return 0.7
				}
				return base(i)
			}
		case "flapper":
			p = func(t time.Duration) float64 {
				if i == n-1 {
					if int(t/(10*time.Minute))%2 == 0 {
						return 0.5
					}
					return 0
				}
				return 0.005
			}
		case "bad-half":
			p = func(time.Duration) float64 {
				if i%2 == 0 {
					return 0.5
				}
				return 0.005
			}
		}
		accts[i] = &simAccount{id: fmt.Sprintf("acct-%02d", i), p: p}
	}
	return name, accts
}

func runSim(scenario string, accts []*simAccount, policy simPolicy, seed int64) simResult {
	rng := rand.New(rand.NewSource(seed))
	start := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	var now time.Time
	stats := newCodexCapacityStats()
	stats.now = func() time.Time { return now }
	reroutes := newCodexOverloadReroutes()
	oldEp, oldRate, oldHold, oldCap := codexCapacityTaintMinEpisodes, codexCapacityTaintRate, codexCapacityTaintHoldout, codexCapacityMaxHeldOutFraction
	codexCapacityTaintMinEpisodes, codexCapacityTaintRate, codexCapacityTaintHoldout, codexCapacityMaxHeldOutFraction = policy.taintEpisodes, policy.taintRate, policy.taintHoldout, policy.capFraction
	defer func() {
		codexCapacityTaintMinEpisodes, codexCapacityTaintRate, codexCapacityTaintHoldout, codexCapacityMaxHeldOutFraction = oldEp, oldRate, oldHold, oldCap
	}()
	for _, a := range accts {
		a.held = time.Time{}
	}
	const base = 2 * time.Minute
	const horizon = 3 * time.Hour
	sessions := make([]*simSession, 60)
	pick := func(exclude int) int {
		best, bestLoad := -1, 1<<30
		loads := map[int]int{}
		for _, s := range sessions {
			if s != nil {
				loads[s.account]++
			}
		}
		order := rng.Perm(len(accts))
		for _, i := range order {
			if i == exclude || accts[i].held.After(now) {
				continue
			}
			if loads[i] < bestLoad {
				best, bestLoad = i, loads[i]
			}
		}
		if best >= 0 {
			return best
		}
		// Every account held out: Pick still returns one (ranked last).
		return order[0]
	}
	for i := range sessions {
		sessions[i] = &simSession{id: fmt.Sprintf("s%02d", i), ws: i%2 == 0, account: -1, nextTurn: time.Duration(rng.Intn(60)) * time.Second}
		sessions[i].account = pick(-1)
	}
	res := simResult{}
	sinceStart := func() time.Duration { return now.Sub(start) }
	holdOut := func(a *simAccount, v codexCapacityVerdict, wasBad bool) {
		ttl := v.MarkTTL
		if policy.flat {
			ttl = base
		}
		if !policy.flat && v.Tainted {
			if wasBad {
				res.trueTaints++
			} else {
				res.falseTaints++
			}
		}
		if until := now.Add(ttl); until.After(a.held) {
			a.held = until
		}
	}
	for now = start; now.Before(start.Add(horizon)); now = now.Add(time.Second) {
		if int(sinceStart()/time.Second)%30 == 0 {
			held := 0
			for _, a := range accts {
				if a.held.After(now) {
					held++
				}
			}
			res.heldSamples = append(res.heldSamples, float64(held)/float64(len(accts)))
		}
		for _, s := range sessions {
			if sinceStart() < s.nextTurn {
				continue
			}
			if !s.failing {
				s.turnStart = sinceStart()
				s.failing = true
				res.turns++
			}
			// Sticky reuse fails on a held-out account: reroute if allowed.
			a := accts[s.account]
			if a.held.After(now) {
				if !s.ws {
					s.account = pick(s.account)
					a = accts[s.account]
				} else {
					key := "codex\x00" + s.id
					moved := reroutes.allow(key, codexOverloadMaxWebSocketReroutes)
					if !moved && !policy.flat && stats.tainted(a.id) || !moved && !policy.flat && stats.persistent(a.id) {
						moved = reroutes.allowPersistent(key)
					}
					if moved {
						s.account = pick(s.account)
						a = accts[s.account]
					}
				}
			}
			p := a.p(sinceStart())
			if rng.Float64() < p {
				res.upstreamFailures++
				v := stats.noteFailure(a.id, "codex\x00"+s.id, "server_is_overloaded", base, len(accts))
				holdOut(a, v, p >= 0.3)
				if !s.ws {
					// HTTP failover: up to 3 more accounts inside the request.
					served := false
					for sw := 0; sw < 3; sw++ {
						next := pick(s.account)
						if accts[next].held.After(now) {
							res.noAlternate++
							break
						}
						s.account = next
						b := accts[next]
						if rng.Float64() < b.p(sinceStart()) {
							res.upstreamFailures++
							holdOut(b, stats.noteFailure(b.id, "codex\x00"+s.id, "server_is_overloaded", base, len(accts)), b.p(sinceStart()) >= 0.3)
							continue
						}
						stats.noteSuccess(b.id, "codex\x00"+s.id)
						served = true
						break
					}
					if served {
						s.failing = false
						res.waits = append(res.waits, sinceStart()-s.turnStart)
						s.nextTurn = sinceStart() + 20*time.Second + time.Duration(rng.Intn(30))*time.Second
						continue
					}
				}
				res.clientFailures++
				s.failures++
				s.nextTurn = sinceStart() + 2*time.Second // the fork retries at once
				continue
			}
			stats.noteSuccess(a.id, "codex\x00"+s.id)
			s.failing = false
			res.waits = append(res.waits, sinceStart()-s.turnStart)
			s.nextTurn = sinceStart() + 20*time.Second + time.Duration(rng.Intn(30))*time.Second
		}
	}
	return res
}

func pct(d []time.Duration, q float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	c := append([]time.Duration(nil), d...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[int(float64(len(c)-1)*q)]
}

func TestCodexCapacitySimulation(t *testing.T) {
	if os.Getenv("SUBROUTER_CAPACITY_SIM") == "" {
		t.Skip("set SUBROUTER_CAPACITY_SIM=1 to run the capacity simulation")
	}
	policies := []simPolicy{
		{name: "flat-2m", flat: true, taintEpisodes: 1 << 20, taintRate: 2, taintHoldout: time.Minute, capFraction: 0.25},
		{name: "persistent-only", taintEpisodes: 1 << 20, taintRate: 2, taintHoldout: time.Minute, capFraction: 0.25},
		{name: "taint-4ep-50%-30m-cap25", taintEpisodes: 4, taintRate: 0.5, taintHoldout: 30 * time.Minute, capFraction: 0.25},
		{name: "taint-4ep-50%-30m-nocap", taintEpisodes: 4, taintRate: 0.5, taintHoldout: 30 * time.Minute, capFraction: 0},
		{name: "taint-3ep-30%-30m-cap25", taintEpisodes: 3, taintRate: 0.3, taintHoldout: 30 * time.Minute, capFraction: 0.25},
		{name: "taint-4ep-50%-10m-cap25", taintEpisodes: 4, taintRate: 0.5, taintHoldout: 10 * time.Minute, capFraction: 0.25},
	}
	for _, scenario := range []string{"today", "bad-half", "regional-blip-15m", "global-overload-20m", "flapper"} {
		fmt.Printf("\n== %s (40 accounts, 60 sessions, 3h, mean of 3 seeds)\n", scenario)
		fmt.Printf("%-26s %7s %8s %8s %6s %6s %7s %7s %6s %6s %6s\n", "policy", "turns", "cliFail", "upFail", "p50", "p95", ">60s", "heldAvg", "hldMx", "taintT", "taintF")
		for _, policy := range policies {
			var agg simResult
			var p50, p95 time.Duration
			var over60 int
			var heldAvg, heldMax float64
			seeds := []int64{1, 2, 3}
			for _, seed := range seeds {
				name, accts := simScenario(scenario, 40)
				_ = name
				r := runSim(scenario, accts, policy, seed)
				agg.turns += r.turns
				agg.clientFailures += r.clientFailures
				agg.upstreamFailures += r.upstreamFailures
				agg.noAlternate += r.noAlternate
				agg.trueTaints += r.trueTaints
				agg.falseTaints += r.falseTaints
				p50 += pct(r.waits, 0.5)
				p95 += pct(r.waits, 0.95)
				for _, w := range r.waits {
					if w > time.Minute {
						over60++
					}
				}
				sum, mx := 0.0, 0.0
				for _, h := range r.heldSamples {
					sum += h
					if h > mx {
						mx = h
					}
				}
				heldAvg += sum / float64(len(r.heldSamples))
				heldMax += mx
			}
			k := float64(len(seeds))
			fmt.Printf("%-26s %7d %8d %8d %6s %6s %7d %6.1f%% %5.0f%% %6d %6d\n", policy.name,
				agg.turns/len(seeds), agg.clientFailures/len(seeds), agg.upstreamFailures/len(seeds),
				(p50 / time.Duration(len(seeds))).Truncate(time.Second), (p95 / time.Duration(len(seeds))).Truncate(time.Second),
				over60/len(seeds), 100*heldAvg/k, 100*heldMax/k, agg.trueTaints/len(seeds), agg.falseTaints/len(seeds))
		}
	}
}
