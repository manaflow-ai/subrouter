package selectacct

import (
	"fmt"
	"math"
	"testing"

	"github.com/manaflow-ai/subrouter/account"
)

// This simulation reproduces the production shape behind the 2026-08-18
// incident on cmux-lawrence, where one Codex account served 100% of traffic
// for five consecutive hours while six healthy accounts sat idle, and its
// eventual weekly-cap exhaustion rerouted every session on it at once
// (26,165 account moves in one day, peaking at 8,043/hour), re-billing each
// moved session's whole cached prefix.
//
// The modeled mechanics, each taken from the live system:
//   - Accounts expose only a WEEKLY window (no refill within the sim), so
//     ExpiryPressure is 0 and there is no short-window signal.
//   - The usage snapshot the scheduler sees is integer-percent and lags true
//     consumption by lagTicks (used_percent "lags actual usage
//     substantially" and is integer-only for Codex accounts).
//   - Live debits are wiped by every usage refresh (30s cadence), and
//     placements are minutes-to-hours apart, so placements never benefit
//     from debits. The sim therefore builds each placement's scheduler from
//     the lagging snapshot alone; TestPickSpreadSurvivesRefreshWipedDebits
//     covers the wipe directly.
//   - Sessions are long-lived and sticky: one placement commits hours of
//     consumption to the picked account. A session moves only when its
//     account hits the cap (upstream rejects; production then reroutes every
//     session on the account).
type stampedeSimResult struct {
	peakShare     float64 // max fraction of active sessions on one account (sampled at >=12 active)
	largestMove   int     // most sessions rerouted in a single tick (the cap-hit stampede)
	accountsEver  int     // accounts that ever hosted a session
	unservedTicks int     // session-ticks with every account capped (equal for all policies)
}

func runStampedeSim(t *testing.T) stampedeSimResult {
	t.Helper()
	const (
		accounts        = 7
		ticks           = 24 * 60 // one tick per minute, 24h
		lagTicks        = 30      // usage endpoint lag
		arrivalEvery    = 15      // one new session every 15 minutes for the first 10h
		arrivals        = 40
		perSessionBurn  = 0.0004 // fraction of a weekly window per session per tick
		sampleThreshold = 12
	)
	initialUsed := []float64{0.04, 0.12, 0.12, 0.13, 0.15, 0.19, 0.20}

	ids := make([]string, accounts)
	candidates := make([]account.Account, accounts)
	for i := range ids {
		ids[i] = fmt.Sprintf("acct-%d@example.com", i)
		candidates[i] = account.Account{ID: ids[i], Provider: account.ProviderCodex, AuthMode: account.AuthModeOAuth}
	}

	used := append([]float64(nil), initialUsed...)
	history := make([][]float64, 0, ticks+1) // used[] per tick, for the lagging snapshot
	sessionAccount := map[int]int{}          // session -> account index
	nextSession := 0
	hosted := make(map[int]bool)

	var result stampedeSimResult
	for tick := 0; tick < ticks; tick++ {
		history = append(history, append([]float64(nil), used...))

		// Build the scheduler the way production sees the pool: integer
		// used_percent from lagTicks ago, exhaustion known immediately
		// (upstream 429 marks the account at request time).
		lagged := history[0]
		if len(history) > lagTicks {
			lagged = history[len(history)-1-lagTicks]
		}
		scores := make([]Score, accounts)
		sessionCounts := map[string]int{}
		for _, idx := range sessionAccount {
			sessionCounts[ids[idx]]++
		}
		usableAccounts := 0
		for i := range scores {
			headroom := 1 - math.Floor(lagged[i]*100)/100
			if used[i] >= 1 {
				headroom = 0
			}
			if headroom >= MinNewSessionHeadroom {
				usableAccounts++
			}
			scores[i] = Score{AccountID: ids[i], Provider: account.ProviderCodex, Headroom: headroom, ShortHeadroom: headroom, Fresh: true}
		}
		scheduler := NewScheduler(scores).WithSessionCounts(sessionCounts)

		place := func(session int) {
			picked, err := scheduler.Pick(candidates)
			if err != nil {
				return
			}
			for i, id := range ids {
				if id == picked.ID {
					if used[i] >= 1 {
						// Whole pool capped: the request is unservable no
						// matter the policy.
						delete(sessionAccount, session)
						result.unservedTicks++
						return
					}
					sessionAccount[session] = i
					hosted[i] = true
					return
				}
			}
		}

		// New arrival.
		if tick%arrivalEvery == 0 && nextSession < arrivals {
			place(nextSession)
			nextSession++
		}

		// Sticky consumption.
		for _, idx := range sessionAccount {
			used[idx] += perSessionBurn
		}

		// Cap hits: every session on a newly capped account reroutes at once.
		moved := 0
		for session, idx := range sessionAccount {
			if used[idx] >= 1 {
				moved++
				delete(sessionAccount, session)
				place(session)
			}
		}
		// Metrics cover the phase where spreading is possible at all: once
		// demand has drained the pool to fewer than three usable accounts,
		// every policy is forced to pile the survivors onto whatever is
		// left, so the endgame says nothing about placement quality.
		if moved > result.largestMove && usableAccounts >= 3 {
			result.largestMove = moved
		}

		// Concentration sample.
		if usableAccounts >= 3 && len(sessionAccount) >= sampleThreshold {
			perAccount := map[int]int{}
			for _, idx := range sessionAccount {
				perAccount[idx]++
			}
			for _, count := range perAccount {
				share := float64(count) / float64(len(sessionAccount))
				if share > result.peakShare {
					result.peakShare = share
				}
			}
		}
	}
	result.accountsEver = len(hosted)
	return result
}

// A healthy pool must absorb a day of arriving sticky sessions without
// herding them onto one account, and a weekly-cap hit must not stampede a
// large batch of sessions at once. The deterministic-argmax policy fails
// both: it concentrates every placement on the momentarily-best account
// (peak share well above one half) and then moves that whole herd together
// when the account caps.
func TestStampedeSimSpreadsSessionsAndBoundsMassMoves(t *testing.T) {
	const runs = 6
	var peakShare float64
	var largestMove float64
	accountsEver := 0
	for i := 0; i < runs; i++ {
		result := runStampedeSim(t)
		peakShare += result.peakShare
		largestMove += float64(result.largestMove)
		if result.accountsEver > accountsEver {
			accountsEver = result.accountsEver
		}
	}
	peakShare /= runs
	largestMove /= runs

	t.Logf("avg peak share %.2f, avg largest mass move %.1f, accounts ever hosting %d", peakShare, largestMove, accountsEver)
	if peakShare > 0.50 {
		t.Fatalf("average peak concentration %.2f: more than half of all active sessions sat on one account", peakShare)
	}
	if largestMove > 9 {
		t.Fatalf("average largest single-tick mass move %.1f sessions: a cap hit stampedes the herd", largestMove)
	}
}
