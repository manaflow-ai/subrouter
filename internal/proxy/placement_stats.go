package proxy

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// PlacementStatsPath serves the per-account placement, routing, eviction and
// failover counters plus the per-pool herding summary as JSON.
const PlacementStatsPath = "/_subrouter/placement-stats"

// MetricsPath serves the same counters as Prometheus text.
const MetricsPath = "/_subrouter/metrics"

// placementPool names the quota pool a placement was scored against: the
// model's dedicated pool when the provider's scores expose one, otherwise ""
// for the account-wide pool.
func placementPool(base selectacct.Scheduler, provider accounts.Provider, poolModel string) string {
	if poolModel == "" || !base.HasModelPoolFor(schedulerAccountProvider(provider), poolModel) {
		return ""
	}
	return selectacct.ModelKey(poolModel)
}

// usageFailoverReason classifies a usage-limit transport failover.
func usageFailoverReason(credentialFailure, modelUnsupported bool) selectacct.FailoverReason {
	switch {
	case modelUnsupported:
		return selectacct.FailoverModel
	case credentialFailure:
		return selectacct.FailoverAuth
	default:
		return selectacct.FailoverUsageLimit
	}
}

func (s Server) placementStats(r *http.Request) selectacct.PlacementStatsSnapshot {
	var counts map[string]int
	if s.Sessions != nil {
		counts = SchedulerSessionCounts(s.Sessions)
	}
	return s.SchedulerRef.PlacementStats(counts, SchedulerAccounts(s.accountListContext(r.Context())))
}

func (s Server) handlePlacementStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, s.placementStats(r))
}

func (s Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(placementPrometheusText(s.placementStats(r))))
}

type promCounter struct {
	name, help, kind string
	value            func(selectacct.AccountPlacementStats) uint64
}

var placementAccountMetrics = []promCounter{
	{"subrouter_placements_total", "New sessions placed on the account.", "counter",
		func(a selectacct.AccountPlacementStats) uint64 { return a.Placements }},
	{"subrouter_routed_requests_total", "Requests routed to the account.", "counter",
		func(a selectacct.AccountPlacementStats) uint64 { return a.Routed }},
	{"subrouter_sticky_evictions_total", "Sessions moved off the account because it fell out of retention.", "counter",
		func(a selectacct.AccountPlacementStats) uint64 { return a.Evictions }},
	{"subrouter_capacity_marks_total", "Capacity (load-shedding) failures recorded against the account.", "counter",
		func(a selectacct.AccountPlacementStats) uint64 { return a.CapacityMarks }},
	{"subrouter_sessions", "Sessions currently assigned to the account.", "gauge",
		func(a selectacct.AccountPlacementStats) uint64 { return uint64(a.Sessions) }},
}

func placementPrometheusText(snapshot selectacct.PlacementStatsSnapshot) string {
	var b strings.Builder
	for _, metric := range placementAccountMetrics {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", metric.name, metric.help, metric.name, metric.kind)
		for _, acct := range snapshot.Accounts {
			fmt.Fprintf(&b, "%s{provider=%s,account=%s} %d\n", metric.name,
				promLabel(string(acct.Provider)), promLabel(acct.AccountID), metric.value(acct))
		}
	}
	b.WriteString("# HELP subrouter_failovers_total Requests that failed over away from the account, by reason.\n")
	b.WriteString("# TYPE subrouter_failovers_total counter\n")
	for _, acct := range snapshot.Accounts {
		reasons := make([]string, 0, len(acct.Failovers))
		for reason := range acct.Failovers {
			reasons = append(reasons, string(reason))
		}
		sort.Strings(reasons)
		for _, reason := range reasons {
			fmt.Fprintf(&b, "subrouter_failovers_total{provider=%s,account=%s,reason=%s} %d\n",
				promLabel(string(acct.Provider)), promLabel(acct.AccountID), promLabel(reason),
				acct.Failovers[selectacct.FailoverReason(reason)])
		}
	}
	b.WriteString("# HELP subrouter_pool_placements_1h New-session placements in the pool over the last hour.\n")
	b.WriteString("# TYPE subrouter_pool_placements_1h gauge\n")
	for _, pool := range snapshot.Pools {
		fmt.Fprintf(&b, "subrouter_pool_placements_1h{provider=%s,pool=%s} %d\n",
			promLabel(string(pool.Provider)), promLabel(pool.Pool), pool.Placements)
	}
	b.WriteString("# HELP subrouter_pool_busiest_share_1h Share of the pool's last-hour placements that landed on its busiest account.\n")
	b.WriteString("# TYPE subrouter_pool_busiest_share_1h gauge\n")
	for _, pool := range snapshot.Pools {
		fmt.Fprintf(&b, "subrouter_pool_busiest_share_1h{provider=%s,pool=%s} %g\n",
			promLabel(string(pool.Provider)), promLabel(pool.Pool), pool.BusiestShare)
	}
	return b.String()
}

var promLabelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func promLabel(value string) string {
	return `"` + promLabelEscaper.Replace(value) + `"`
}
