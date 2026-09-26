package selectacct

import (
	"sort"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

// Placement stats make the router's real distribution of work visible. Pick
// is tuned against simulations (gto_sim_test.go), but until these counters
// existed production exposed nothing that showed where sessions actually
// landed: the 2026-08-18 herd (one account serving 100% of Codex traffic for
// five hours) was only found after the fact. Counters are cumulative since
// process start; nothing here is persisted.
//
// Every note is one short critical section on a mutex private to the stats
// (never SchedulerRef.mu), with no I/O, so the request path pays a map
// increment and nothing else.

// FailoverReason classifies why a request left the account it was on.
type FailoverReason string

const (
	FailoverUsageLimit FailoverReason = "usage_limit"
	FailoverAuth       FailoverReason = "auth"
	// FailoverCapacity covers overload and capacity reroutes: the account was
	// shedding load, not out of quota.
	FailoverCapacity FailoverReason = "capacity"
	// FailoverModel is a reroute because the account cannot serve the model.
	FailoverModel FailoverReason = "model"
)

// HerdWindow is how far back the pool-level busiest-account share looks.
const HerdWindow = time.Hour

const herdBuckets = int(HerdWindow / time.Minute)

type accountCounters struct {
	placements    uint64
	routed        uint64
	evictions     uint64
	capacityMarks uint64
	failovers     map[FailoverReason]uint64
}

type poolKey struct {
	provider account.Provider
	pool     string
}

// herdRing holds per-minute placement counts per account for one pool. Slot i
// holds the minute stamped in minutes[i]; a slot whose stamp falls outside
// the window is stale and read as empty.
type herdRing struct {
	minutes [herdBuckets]int64
	counts  [herdBuckets]map[string]uint64
}

func (h *herdRing) add(minute int64, accountID string) {
	slot := int(minute % int64(herdBuckets))
	if h.minutes[slot] != minute || h.counts[slot] == nil {
		h.minutes[slot] = minute
		h.counts[slot] = make(map[string]uint64, 4)
	}
	h.counts[slot][accountID]++
}

func (h *herdRing) window(minute int64) map[string]uint64 {
	out := map[string]uint64{}
	for slot := range h.counts {
		if h.counts[slot] == nil || minute-h.minutes[slot] >= int64(herdBuckets) || h.minutes[slot] > minute {
			continue
		}
		for id, count := range h.counts[slot] {
			out[id] += count
		}
	}
	return out
}

type placementStats struct {
	mu sync.Mutex
	// since is when counting started: the ref's creation, which is process
	// start for the server's ref.
	since    time.Time
	accounts map[string]*accountCounters
	pools    map[poolKey]*herdRing
	// now is swapped by tests to move through the herd window.
	now func() time.Time
}

func (p *placementStats) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// countersLocked returns the account's counters, creating them on first use.
func (p *placementStats) countersLocked(provider account.Provider, accountID string) *accountCounters {
	if p.accounts == nil {
		p.accounts = make(map[string]*accountCounters)
	}
	if p.since.IsZero() {
		p.since = p.clock()
	}
	key := ScoreKey(provider, accountID)
	counters := p.accounts[key]
	if counters == nil {
		counters = &accountCounters{}
		p.accounts[key] = counters
	}
	return counters
}

func (p *placementStats) note(provider account.Provider, accountID string, apply func(*accountCounters)) {
	if accountID == "" {
		return
	}
	p.mu.Lock()
	apply(p.countersLocked(provider, accountID))
	p.mu.Unlock()
}

// NotePlacement records that a session with no assignment was placed on the
// account. pool is the model quota pool the placement was scored against
// (ModelKey), or "" for the account-wide pool.
func (r *SchedulerRef) NotePlacement(provider account.Provider, accountID, pool string) {
	if r == nil || accountID == "" {
		return
	}
	provider = normalizedStatsProvider(provider)
	p := &r.placement
	p.mu.Lock()
	defer p.mu.Unlock()
	p.countersLocked(provider, accountID).placements++
	if p.pools == nil {
		p.pools = make(map[poolKey]*herdRing)
	}
	key := poolKey{provider: provider, pool: pool}
	ring := p.pools[key]
	if ring == nil {
		ring = &herdRing{}
		p.pools[key] = ring
	}
	ring.add(p.clock().Unix()/60, accountID)
}

// NoteStickyEviction records that a session assigned to the account was
// moved off it because the account fell out of retention (below the sticky
// headroom floor, exhausted, or shedding the session's model).
func (r *SchedulerRef) NoteStickyEviction(provider account.Provider, accountID string) {
	if r == nil {
		return
	}
	r.placement.note(normalizedStatsProvider(provider), accountID, func(c *accountCounters) { c.evictions++ })
}

// NoteFailover records that a request failed over away from the account.
func (r *SchedulerRef) NoteFailover(provider account.Provider, accountID string, reason FailoverReason) {
	if r == nil {
		return
	}
	r.placement.note(normalizedStatsProvider(provider), accountID, func(c *accountCounters) {
		if c.failovers == nil {
			c.failovers = make(map[FailoverReason]uint64, 2)
		}
		c.failovers[reason]++
	})
}

func (r *SchedulerRef) noteRoutedStat(provider account.Provider, accountID string) {
	r.placement.note(normalizedStatsProvider(provider), accountID, func(c *accountCounters) { c.routed++ })
}

func (r *SchedulerRef) noteCapacityMarkStat(provider account.Provider, accountID string) {
	r.placement.note(normalizedStatsProvider(provider), accountID, func(c *accountCounters) { c.capacityMarks++ })
}

func normalizedStatsProvider(provider account.Provider) account.Provider {
	if provider == "" {
		return account.ProviderCodex
	}
	return provider
}

// AccountPlacementStats is one account's counters since process start.
type AccountPlacementStats struct {
	Provider      account.Provider          `json:"provider"`
	AccountID     string                    `json:"account_id"`
	Placements    uint64                    `json:"placements"`
	Routed        uint64                    `json:"routed_requests"`
	Evictions     uint64                    `json:"sticky_evictions"`
	Failovers     map[FailoverReason]uint64 `json:"failovers,omitempty"`
	CapacityMarks uint64                    `json:"capacity_marks"`
	// Sessions is the current number of sessions assigned to the account.
	Sessions int `json:"sessions"`
}

// FailoverTotal sums failovers across reasons.
func (a AccountPlacementStats) FailoverTotal() uint64 {
	var total uint64
	for _, count := range a.Failovers {
		total += count
	}
	return total
}

// PoolPlacementStats summarizes one provider's model pool over HerdWindow.
// BusiestShare near 1 with several usable accounts in the pool is herding.
type PoolPlacementStats struct {
	Provider         account.Provider `json:"provider"`
	Pool             string           `json:"pool"`
	WindowSeconds    int64            `json:"window_seconds"`
	Placements       uint64           `json:"placements"`
	BusiestAccountID string           `json:"busiest_account_id,omitempty"`
	BusiestShare     float64          `json:"busiest_share"`
	Accounts         int              `json:"accounts"`
}

// PlacementStatsSnapshot is the reportable view of the stats.
type PlacementStatsSnapshot struct {
	Since    time.Time               `json:"since,omitempty"`
	Accounts []AccountPlacementStats `json:"accounts"`
	Pools    []PoolPlacementStats    `json:"pools"`
}

// PlacementStats snapshots the counters. sessionCounts (keyed by ScoreKey,
// as SchedulerSessionCounts produces) fills Sessions; known lists accounts
// that must appear even with nothing counted yet, so an idle account in a
// herded pool shows up as zeros instead of missing.
func (r *SchedulerRef) PlacementStats(sessionCounts map[string]int, known []account.Account) PlacementStatsSnapshot {
	snapshot := PlacementStatsSnapshot{Accounts: []AccountPlacementStats{}, Pools: []PoolPlacementStats{}}
	if r == nil {
		return snapshot
	}
	p := &r.placement
	p.mu.Lock()
	snapshot.Since = p.since
	byKey := make(map[string]*AccountPlacementStats, len(p.accounts)+len(known))
	for key, counters := range p.accounts {
		_, provider, accountID := splitScoreKey(key)
		entry := &AccountPlacementStats{
			Provider:      provider,
			AccountID:     accountID,
			Placements:    counters.placements,
			Routed:        counters.routed,
			Evictions:     counters.evictions,
			CapacityMarks: counters.capacityMarks,
		}
		if len(counters.failovers) > 0 {
			entry.Failovers = make(map[FailoverReason]uint64, len(counters.failovers))
			for reason, count := range counters.failovers {
				entry.Failovers[reason] = count
			}
		}
		byKey[key] = entry
	}
	minute := p.clock().Unix() / 60
	for key, ring := range p.pools {
		counts := ring.window(minute)
		pool := PoolPlacementStats{
			Provider:      key.provider,
			Pool:          key.pool,
			WindowSeconds: int64(HerdWindow / time.Second),
		}
		var busiest uint64
		for id, count := range counts {
			pool.Placements += count
			if count > busiest || (count == busiest && id < pool.BusiestAccountID) {
				busiest = count
				pool.BusiestAccountID = id
			}
		}
		pool.Accounts = len(counts)
		if pool.Placements > 0 {
			pool.BusiestShare = float64(busiest) / float64(pool.Placements)
		} else {
			pool.BusiestAccountID = ""
		}
		snapshot.Pools = append(snapshot.Pools, pool)
	}
	p.mu.Unlock()

	for _, acct := range known {
		provider := normalizedStatsProvider(acct.Provider)
		key := ScoreKey(provider, acct.ID)
		if _, ok := byKey[key]; !ok && acct.ID != "" {
			byKey[key] = &AccountPlacementStats{Provider: provider, AccountID: acct.ID}
		}
	}
	for key, count := range sessionCounts {
		if count <= 0 {
			continue
		}
		entry, ok := byKey[key]
		if !ok {
			_, provider, accountID := splitScoreKey(key)
			if accountID == "" {
				continue
			}
			entry = &AccountPlacementStats{Provider: provider, AccountID: accountID}
			byKey[key] = entry
		}
		entry.Sessions = count
	}
	for _, entry := range byKey {
		snapshot.Accounts = append(snapshot.Accounts, *entry)
	}
	sort.Slice(snapshot.Accounts, func(i, j int) bool {
		left, right := snapshot.Accounts[i], snapshot.Accounts[j]
		if left.Provider != right.Provider {
			return left.Provider < right.Provider
		}
		return left.AccountID < right.AccountID
	})
	sort.Slice(snapshot.Pools, func(i, j int) bool {
		left, right := snapshot.Pools[i], snapshot.Pools[j]
		if left.Provider != right.Provider {
			return left.Provider < right.Provider
		}
		return left.Pool < right.Pool
	})
	return snapshot
}

// splitScoreKey reverses ScoreKey. A key without the provider separator is a
// bare account ID, which ScoreKey treats as Codex.
func splitScoreKey(key string) (string, account.Provider, string) {
	for i := 0; i < len(key); i++ {
		if key[i] == 0 {
			return key, account.Provider(key[:i]), key[i+1:]
		}
	}
	return key, account.ProviderCodex, key
}
