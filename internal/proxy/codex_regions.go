package proxy

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Codex regions.
//
// OpenAI serves each model from regional pools, and which region has
// headroom changes minute to minute. The router's own uplink is the "local"
// region; every CodexEgressConfig proxy is another one. Health is kept per
// (model, region): a region that just answered a model with a capacity
// failure cools for codexRegionCooldown, and every new attempt for that model
// prefers a region that is not cooling. Cooling only reorders; when every
// region is cooling the request still goes out, starting with the local one.
//
// Slots: 0 is local, i+1 is egress proxy i.

const (
	codexRegionLocalSlot  = 0
	codexRegionLocalName  = "local"
	codexRegionCooldown   = time.Minute
	codexRegionMaxSamples = 512
)

type codexRegionSample struct {
	at     time.Time
	failed bool
}

type codexRegionState struct {
	lastFailure time.Time
	lastSuccess time.Time
	failures    uint64
	successes   uint64
	samples     []codexRegionSample // ring, newest at next-1
	next        int
}

func (st *codexRegionState) add(at time.Time, failed bool) {
	if failed {
		st.failures++
		st.lastFailure = at
	} else {
		st.successes++
		st.lastSuccess = at
	}
	sample := codexRegionSample{at: at, failed: failed}
	if len(st.samples) < codexRegionMaxSamples {
		st.samples = append(st.samples, sample)
		return
	}
	st.samples[st.next] = sample
	st.next = (st.next + 1) % codexRegionMaxSamples
}

// cooling is a capacity failure inside the cooldown with no success since.
func (st *codexRegionState) cooling(now time.Time) bool {
	return st != nil && !st.lastFailure.IsZero() && now.Sub(st.lastFailure) < codexRegionCooldown &&
		!st.lastSuccess.After(st.lastFailure)
}

func (st *codexRegionState) window(now time.Time, d time.Duration) (failures, successes int) {
	for _, sample := range st.samples {
		if now.Sub(sample.at) > d {
			continue
		}
		if sample.failed {
			failures++
		} else {
			successes++
		}
	}
	return failures, successes
}

// codexRegionHealth is shared by every copy of a Server. Nil-safe: a nil
// health treats every region as healthy.
type codexRegionHealth struct {
	mu     sync.Mutex
	now    func() time.Time
	names  []string // by slot
	models map[string][]*codexRegionState
}

func newCodexRegionHealth(config *CodexEgressConfig) *codexRegionHealth {
	names := append([]string{codexRegionLocalName}, config.names()...)
	return &codexRegionHealth{now: time.Now, names: names, models: map[string][]*codexRegionState{}}
}

func codexRegionModelKey(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

func (h *codexRegionHealth) stateLocked(model string, slot int) *codexRegionState {
	key := codexRegionModelKey(model)
	states := h.models[key]
	if states == nil {
		states = make([]*codexRegionState, len(h.names))
		h.models[key] = states
	}
	if states[slot] == nil {
		states[slot] = &codexRegionState{}
	}
	return states[slot]
}

func (h *codexRegionHealth) slots() int {
	if h == nil {
		return 0
	}
	return len(h.names)
}

func (h *codexRegionHealth) name(slot int) string {
	if h == nil || slot < 0 || slot >= len(h.names) {
		return ""
	}
	return h.names[slot]
}

// note records one attempt's outcome for model in slot.
func (h *codexRegionHealth) note(model string, slot int, failed bool) {
	if h == nil || slot < 0 || slot >= len(h.names) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stateLocked(model, slot).add(h.now(), failed)
}

func (h *codexRegionHealth) coolingLocked(model string, slot int, now time.Time) bool {
	states := h.models[codexRegionModelKey(model)]
	if slot < 0 || slot >= len(states) {
		return false
	}
	return states[slot].cooling(now)
}

// cooling reports whether slot just failed model.
func (h *codexRegionHealth) cooling(model string, slot int) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.coolingLocked(model, slot, h.now())
}

// localCooling reports whether the router's own uplink just failed model.
func (h *codexRegionHealth) localCooling(model string) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.coolingLocked(model, codexRegionLocalSlot, h.now())
}

// order lists up to limit slots to try for model, healthy ones first. The
// local slot leads the healthy group (it is the closest), then egress slots
// walk the ring from egressStart so sessions spread across regions. skip
// drops slots outright (already tried this round, or faulted for a test).
// keepFirst, when >= 0, leads the list regardless of health.
//
// egressCount is the number of egress proxies; a nil health (a Server not
// built through Handler) treats every slot as healthy.
func (h *codexRegionHealth) order(model string, egressCount, egressStart, limit, keepFirst int, skip func(int) bool) []int {
	count := egressCount + 1
	if egressCount < 0 || limit <= 0 {
		return nil
	}
	cooling := make([]bool, count)
	if h != nil {
		h.mu.Lock()
		now := h.now()
		for slot := range count {
			cooling[slot] = h.coolingLocked(model, slot, now)
		}
		h.mu.Unlock()
	}
	ring := make([]int, 0, count)
	ring = append(ring, codexRegionLocalSlot)
	if egressCount > 0 {
		egressStart = ((egressStart % egressCount) + egressCount) % egressCount
		for step := range egressCount {
			ring = append(ring, 1+(egressStart+step)%egressCount)
		}
	}
	order := make([]int, 0, min(limit, count))
	if keepFirst >= 0 && keepFirst < count && (skip == nil || !skip(keepFirst)) {
		order = append(order, keepFirst)
	}
	for _, wantCooling := range []bool{false, true} {
		for _, slot := range ring {
			if slot == keepFirst || (skip != nil && skip(slot)) || cooling[slot] != wantCooling {
				continue
			}
			order = append(order, slot)
		}
	}
	if len(order) > limit {
		order = order[:limit]
	}
	return order
}

// egressOrder is order() over count egress proxies only, as proxy indexes.
func (h *codexRegionHealth) egressOrder(model string, count, start, limit int, keepStart bool) []int {
	if count <= 0 {
		return nil
	}
	start = ((start % count) + count) % count
	keep := -1
	if keepStart {
		keep = start + 1
	}
	slots := h.order(model, count, start, limit, keep, func(slot int) bool { return slot == codexRegionLocalSlot })
	indexes := make([]int, 0, len(slots))
	for _, slot := range slots {
		indexes = append(indexes, slot-1)
	}
	return indexes
}

// CodexRegionReport is one (model, region) row of /_subrouter/codex-capacity.
type CodexRegionReport struct {
	Model         string    `json:"model"`
	Region        string    `json:"region"`
	Cooling       bool      `json:"cooling"`
	Failures15m   int       `json:"failures_15m"`
	Successes15m  int       `json:"successes_15m"`
	Failures1h    int       `json:"failures_1h"`
	Successes1h   int       `json:"successes_1h"`
	TotalFailures uint64    `json:"total_failures"`
	TotalServed   uint64    `json:"total_successes"`
	LastFailure   time.Time `json:"last_failure,omitzero"`
	LastSuccess   time.Time `json:"last_success,omitzero"`
}

func (h *codexRegionHealth) snapshot() []CodexRegionReport {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	var rows []CodexRegionReport
	for model, states := range h.models {
		for slot, st := range states {
			if st == nil {
				continue
			}
			f15, s15 := st.window(now, 15*time.Minute)
			f1h, s1h := st.window(now, time.Hour)
			rows = append(rows, CodexRegionReport{
				Model: model, Region: h.names[slot], Cooling: st.cooling(now),
				Failures15m: f15, Successes15m: s15, Failures1h: f1h, Successes1h: s1h,
				TotalFailures: st.failures, TotalServed: st.successes,
				LastFailure: st.lastFailure, LastSuccess: st.lastSuccess,
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Model != rows[j].Model {
			return rows[i].Model < rows[j].Model
		}
		return rows[i].Region < rows[j].Region
	})
	return rows
}

// Per-request region state, kept on the shared upstreamAttempt.
type codexRegionAttemptState struct {
	last int // slot of the last physical attempt, -1 before the first
	// rotate says the last attempt failed in a way its region owns (capacity
	// or an unreachable proxy), so the next one leaves from elsewhere.
	rotate bool
	// lastCapacity says the last attempt was a capacity failure.
	lastCapacity bool
	tried        map[int]struct{}
}

// errCodexRegionsFaulted answers a request whose test fault blocks every
// region. Nothing is sent upstream.
var errCodexRegionsFaulted = errors.New("subrouter test fault blocks every codex region")

// nextCodexRegion picks the slot for the request's next physical attempt.
// The first attempt goes local unless local is cooling for the model (or
// faulted); a replay after a capacity failure moves to the next untried
// region, healthy first; any other replay (transport, quota failover) stays
// where the last attempt went.
func (a *upstreamAttempt) nextCodexRegion(ctx context.Context) int {
	health := a.server.codexRegions
	if health.slots() <= 1 {
		return codexRegionLocalSlot
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	st := &a.region
	if st.tried == nil {
		st.tried = map[int]struct{}{}
		st.last = -1
	}
	if st.last >= 0 && !st.rotate {
		return st.last
	}
	fault := codexFaultFrom(ctx)
	start := azureCodexEndpointIndex(azureCodexSessionKeyFor(a.agent, a.session), health.slots()-1)
	pick := func() []int {
		return health.order(a.poolModel, health.slots()-1, start, 1, -1, func(slot int) bool {
			_, done := st.tried[slot]
			return done || fault.blocks(health.name(slot))
		})
	}
	order := pick()
	if len(order) == 0 {
		// Every region was tried this request: start another round.
		clear(st.tried)
		order = pick()
	}
	if len(order) == 0 {
		// Only a test fault can block every region.
		return -1
	}
	slot := order[0]
	st.tried[slot] = struct{}{}
	st.last, st.rotate, st.lastCapacity = slot, false, false
	return slot
}

func (a *upstreamAttempt) noteCodexRegionResult(slot int, rotate, capacity bool) {
	a.mu.Lock()
	a.region.last, a.region.rotate, a.region.lastCapacity = slot, rotate, capacity
	if capacity {
		a.capacityFailures++
	}
	a.mu.Unlock()
}

func (a *upstreamAttempt) lastCodexAttemptCapacity() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.region.lastCapacity
}

func (a *upstreamAttempt) codexCapacityFailures() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.capacityFailures
}

func (a *upstreamAttempt) lastCodexRegion() string {
	a.mu.Lock()
	slot := a.region.last
	a.mu.Unlock()
	return a.server.codexRegions.name(slot)
}

// codexRegionalTransport sends each physical Codex attempt out of the region
// nextCodexRegion picks. It sits at the bottom of the retry stack, so every
// retry above it (same-account ladder, account failover, transport replay)
// rotates regions on its own after a capacity failure. It wraps the in-flight
// accounting itself, so its stream peek is the outermost body wrapper and the
// layers above reuse its verdict instead of peeking again. It never pins a
// session: a region only steers while the local uplink is cooling, and the
// egress fallback layer keeps its own pin rule.
type codexRegionalTransport struct {
	base    http.RoundTripper // the local uplink
	attempt *upstreamAttempt
}

func (t codexRegionalTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	a := t.attempt
	s := a.server
	slot := a.nextCodexRegion(req.Context())
	if slot < 0 {
		return nil, errCodexRegionsFaulted
	}
	transport := t.base
	if slot != codexRegionLocalSlot && slot-1 < len(s.codexEgressTransports) {
		transport = s.codexEgressTransports[slot-1]
	}
	region := s.codexRegions.name(slot)
	response, err := inflightAttemptTransport{base: transport, attempt: a}.RoundTrip(req)
	if err != nil {
		// A dead proxy is the region's fault; a client that went away, or an
		// account paused by admission, is not.
		rotate := slot != codexRegionLocalSlot && req.Context().Err() == nil && !errors.Is(err, errAdmissionPaused)
		if rotate {
			s.codexRegions.note(a.poolModel, slot, true)
			s.recordCodexCapacityEvent(CodexCapacityEvent{
				Transport: "http", Region: region, Account: a.current().ID, Model: a.poolModel,
				Reason: "egress_unreachable", Agent: a.agent, Session: a.session, Outcome: codexCapacityOutcomeAttempt,
			})
		}
		a.noteCodexRegionResult(slot, rotate, false)
		return response, err
	}
	failed, reason, replaced := codexOverloadFailure(response)
	s.codexRegions.note(a.poolModel, slot, failed)
	a.noteCodexRegionResult(slot, failed, failed)
	if failed {
		s.recordCodexCapacityEvent(CodexCapacityEvent{
			Transport: "http", Region: region, Account: a.current().ID, Model: a.poolModel,
			Reason: reason, Agent: a.agent, Session: a.session, Outcome: codexCapacityOutcomeAttempt,
		})
	}
	if replaced.Header == nil {
		replaced.Header = make(http.Header)
	}
	replaced.Header.Set(codexRegionHeader, region)
	return replaced, nil
}

// codexRegionHeader names the region that answered, on HTTP responses and
// websocket upgrades.
const codexRegionHeader = "X-Subrouter-Region"

// codexCapacityOutcomeTransport is the outermost Codex layer: it sees what the
// client gets. It does not peek: the region transport below recorded whether
// the last physical attempt was a capacity failure. A request that hit
// capacity and still served is absorbed; one whose last attempt was a
// capacity failure reached the client; any other error after a capacity
// failure is failed_other.
type codexCapacityOutcomeTransport struct {
	base    http.RoundTripper
	attempt *upstreamAttempt
}

func (t codexCapacityOutcomeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(req)
	a := t.attempt
	failures := a.codexCapacityFailures()
	if failures == 0 || req.Context().Err() != nil {
		return response, err
	}
	outcome := codexCapacityOutcomeFailedOther
	switch {
	case err == nil && response != nil && a.lastCodexAttemptCapacity():
		outcome = codexCapacityOutcomeClientVisible
	case err == nil && response != nil && codexSuccessStatus(response.StatusCode):
		outcome = codexCapacityOutcomeAbsorbed
	}
	a.server.recordCodexCapacityEvent(CodexCapacityEvent{
		Transport: "http", Region: a.lastCodexRegion(), Account: a.current().ID, Model: a.poolModel,
		Agent: a.agent, Session: a.session, Outcome: outcome, Attempts: failures,
	})
	return response, err
}

// Fault injection for live tests. Off unless the operator sets
// SUBROUTER_CODEX_FAULT_INJECTION=1, and then honored only from loopback.
// "X-Subrouter-Test-Fault: capacity=local,fly-egress-ams:3128" keeps the
// request (HTTP attempts and websocket dials) out of those regions, as if
// they were at capacity. Nothing is synthesized: health, account marks and
// other users' routing are untouched, and a request whose fault blocks every
// region fails without reaching upstream.

const codexFaultHeader = "X-Subrouter-Test-Fault"

type codexFaultKey struct{}

type codexFault struct{ regions map[string]struct{} }

func parseCodexFault(raw string) *codexFault {
	raw = strings.TrimSpace(raw)
	value, ok := strings.CutPrefix(raw, "capacity=")
	if !ok {
		return nil
	}
	fault := &codexFault{regions: map[string]struct{}{}}
	for _, region := range strings.Split(value, ",") {
		if region = strings.TrimSpace(region); region != "" {
			fault.regions[region] = struct{}{}
		}
	}
	if len(fault.regions) == 0 {
		return nil
	}
	return fault
}

func (s *Server) withCodexFault(r *http.Request) *http.Request {
	raw := r.Header.Get(codexFaultHeader)
	r.Header.Del(codexFaultHeader)
	if raw == "" || s == nil {
		return r
	}
	fault := parseCodexFault(raw)
	if !s.CodexFaultInjection || !isLoopbackRemote(r.RemoteAddr) || fault == nil {
		if s.Logger != nil {
			s.Logger.Warn("codex test fault ignored", "enabled", s.CodexFaultInjection,
				"remote_addr", r.RemoteAddr, "parsed", fault != nil, "path", r.URL.Path)
		}
		return r
	}
	if s.Logger != nil {
		s.Logger.Info("codex test fault applied", "regions", raw, "path", r.URL.Path)
	}
	return r.WithContext(context.WithValue(r.Context(), codexFaultKey{}, fault))
}

func codexFaultFrom(ctx context.Context) *codexFault {
	if ctx == nil {
		return nil
	}
	fault, _ := ctx.Value(codexFaultKey{}).(*codexFault)
	return fault
}

// blocks keeps the region out of this request's routing.
func (f *codexFault) blocks(region string) bool {
	if f == nil {
		return false
	}
	_, ok := f.regions[region]
	return ok
}
