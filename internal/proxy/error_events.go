package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// ErrorEventsPath reports recent upstream failures with the account pool
// state captured when each one happened.
const ErrorEventsPath = "/_subrouter/errors"

// AccountStatePath reports the account pool state right now, in the same
// shape the error log captures.
const AccountStatePath = "/_subrouter/account-state"

const (
	errorEventMemoryLimit   = 2000
	errorEventFileMaxBytes  = 32 << 20
	errorEventQueueSize     = 1024
	errorEventMessageMax    = 240
	errorEventBodyPeekBytes = 8 << 10
	// errorSnapshotReuse bounds how often a burst of failures captures a new
	// pool snapshot. Failures inside this window share one, so a capacity
	// storm costs one snapshot per provider and pool every few seconds.
	errorSnapshotReuse = 5 * time.Second
)

// ErrorEvent is one failed physical upstream attempt: a non-2xx answer or a
// transport error. Each attempt of a retried request is its own event, so the
// log shows which account failed, which one the retry moved to, and what the
// pool looked like when the router chose.
type ErrorEvent struct {
	ID         uint64            `json:"id"`
	At         time.Time         `json:"at"`
	Provider   accounts.Provider `json:"provider"`
	Agent      string            `json:"agent,omitempty"`
	Session    string            `json:"session,omitempty"`
	Account    string            `json:"account"`
	PoolModel  string            `json:"pool_model,omitempty"`
	Method     string            `json:"method,omitempty"`
	Path       string            `json:"path,omitempty"`
	Upstream   string            `json:"upstream,omitempty"`
	Status     int               `json:"status,omitempty"`
	ErrorClass string            `json:"error_class"`
	// ErrorType and Message come from the upstream error envelope. Message is
	// bounded and whitespace-collapsed; for a non-JSON body (a gateway HTML
	// page) it is the page title or first text, never the whole page.
	ErrorType   string            `json:"error_type,omitempty"`
	Message     string            `json:"message,omitempty"`
	ContentType string            `json:"content_type,omitempty"`
	RetryAfter  string            `json:"retry_after,omitempty"`
	RateLimit   map[string]string `json:"rate_limit,omitempty"`
	SnapshotID  uint64            `json:"snapshot_id,omitempty"`
}

// AccountPoolSnapshot is every account of one provider as the scheduler saw
// it at one moment.
type AccountPoolSnapshot struct {
	ID        uint64            `json:"id"`
	At        time.Time         `json:"at"`
	Provider  accounts.Provider `json:"provider"`
	PoolModel string            `json:"pool_model,omitempty"`
	Total     int               `json:"total"`
	// UsableNew counts accounts whose measured quota has room for a new
	// session and that carry no active block. Usable adds accounts below that
	// floor that are not exhausted. Both read measured quota, not the
	// placement score: the live-load debit that steers picks away from a busy
	// account is not evidence that its quota is gone.
	UsableNew int            `json:"usable_new"`
	Usable    int            `json:"usable"`
	Accounts  []AccountState `json:"accounts"`
}

// AccountState is one account's routing state. Headroom fields are fractions
// of quota left (1 = untouched); ModelHeadroom is the request's pool model.
type AccountState struct {
	Account        string            `json:"account"`
	AuthMode       accounts.AuthMode `json:"auth_mode,omitempty"`
	Tier           string            `json:"tier"`
	Known          bool              `json:"known"`
	Fresh          bool              `json:"fresh,omitempty"`
	Headroom       float64           `json:"headroom"`
	ShortHeadroom  float64           `json:"short_headroom"`
	WeeklyHeadroom float64           `json:"weekly_headroom"`
	ModelHeadroom  *float64          `json:"model_headroom,omitempty"`
	UsableNew      bool              `json:"usable_new"`
	Exhausted      bool              `json:"exhausted"`
	ResetAt        *time.Time        `json:"reset_at,omitempty"`
	Blocked        bool              `json:"blocked,omitempty"`
	BlockedUntil   *time.Time        `json:"blocked_until,omitempty"`
	Inflight       int               `json:"inflight,omitempty"`
	Sessions       int               `json:"sessions,omitempty"`
}

type errorEventRecord struct {
	Event    *ErrorEvent          `json:"event,omitempty"`
	Snapshot *AccountPoolSnapshot `json:"snapshot,omitempty"`
}

// ErrorEventLog keeps recent upstream failures in memory and appends them to
// a JSONL file so a worker restart or swap keeps the history. Recording never
// blocks a request: a full write queue drops the file write and counts it.
type ErrorEventLog struct {
	path   string
	logger *slog.Logger

	mu sync.Mutex
	// lastID is the newest ID handed out. IDs are wall-clock nanoseconds,
	// bumped to stay strictly increasing, so the old and new worker that
	// append to one file during a swap, and events recorded while load()
	// still reads the file, never reuse an ID already on disk.
	lastID    uint64
	events    []ErrorEvent
	snapshots map[uint64]AccountPoolSnapshot
	recent    map[string]AccountPoolSnapshot
	dropped   uint64

	queue chan errorEventRecord
}

// NewErrorEventLog returns a log persisted at path. An empty path keeps the
// log in memory only.
func NewErrorEventLog(path string, logger *slog.Logger) *ErrorEventLog {
	return &ErrorEventLog{
		path:      path,
		logger:    logger,
		snapshots: map[uint64]AccountPoolSnapshot{},
		recent:    map[string]AccountPoolSnapshot{},
		queue:     make(chan errorEventRecord, errorEventQueueSize),
	}
}

// Run loads the persisted tail and then writes queued records until ctx ends.
func (l *ErrorEventLog) Run(ctx context.Context) {
	if l == nil {
		return
	}
	l.load()
	if l.path == "" {
		<-ctx.Done()
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case record := <-l.queue:
			batch := []errorEventRecord{record}
		drain:
			for len(batch) < 256 {
				select {
				case next := <-l.queue:
					batch = append(batch, next)
				default:
					break drain
				}
			}
			l.write(batch)
		}
	}
}

func (l *ErrorEventLog) write(batch []errorEventRecord) {
	if info, err := os.Stat(l.path); err == nil && info.Size() >= errorEventFileMaxBytes {
		_ = os.Rename(l.path, l.path+".1")
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return
	}
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		if l.logger != nil {
			l.logger.Warn("error event log write failed", "path", l.path, "error", err)
		}
		return
	}
	defer file.Close()
	// One write per record: during a worker swap the old and new worker
	// append to the same file, and O_APPEND keeps whole writes from
	// interleaving where a buffered writer's partial flushes would not.
	for _, record := range batch {
		line, err := json.Marshal(record)
		if err != nil {
			continue
		}
		_, _ = file.Write(append(line, '\n'))
	}
}

// load restores the newest persisted events so the endpoint keeps answering
// across a worker swap. A snapshot is kept only while an event refers to it.
func (l *ErrorEventLog) load() {
	if l.path == "" {
		return
	}
	var events []ErrorEvent
	snapshots := map[uint64]AccountPoolSnapshot{}
	for _, path := range []string{l.path + ".1", l.path} {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64<<10), 8<<20)
		for scanner.Scan() {
			var record errorEventRecord
			if json.Unmarshal(scanner.Bytes(), &record) != nil {
				continue
			}
			if record.Event != nil {
				events = append(events, *record.Event)
				if len(events) > 2*errorEventMemoryLimit {
					events = append(events[:0:0], events[len(events)-errorEventMemoryLimit:]...)
				}
			}
			if record.Snapshot != nil {
				snapshots[record.Snapshot.ID] = *record.Snapshot
			}
		}
		_ = file.Close()
	}
	if len(events) > errorEventMemoryLimit {
		events = events[len(events)-errorEventMemoryLimit:]
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// Events recorded while the file loaded are newer than every loaded one.
	merged := append(events, l.events...)
	if len(merged) > errorEventMemoryLimit {
		merged = merged[len(merged)-errorEventMemoryLimit:]
	}
	for _, event := range merged {
		l.lastID = max(l.lastID, event.ID, event.SnapshotID)
		if snapshot, ok := snapshots[event.SnapshotID]; ok {
			if _, have := l.snapshots[event.SnapshotID]; !have {
				l.snapshots[event.SnapshotID] = snapshot
			}
		}
	}
	l.events = merged
	l.pruneSnapshotsLocked()
}

func (l *ErrorEventLog) pruneSnapshotsLocked() {
	referenced := make(map[uint64]struct{}, len(l.snapshots))
	for _, event := range l.events {
		referenced[event.SnapshotID] = struct{}{}
	}
	for id := range l.snapshots {
		if _, ok := referenced[id]; !ok {
			delete(l.snapshots, id)
		}
	}
}

// record stores event, capturing a pool snapshot with capture unless a recent
// one for the same provider and pool can be shared.
func (l *ErrorEventLog) record(event ErrorEvent, capture func() AccountPoolSnapshot) {
	if l == nil {
		return
	}
	key := string(event.Provider) + "\x00" + event.PoolModel
	l.mu.Lock()
	snapshot, ok := l.recent[key]
	l.mu.Unlock()
	var newSnapshot *AccountPoolSnapshot
	if (!ok || event.At.Sub(snapshot.At) >= errorSnapshotReuse) && capture != nil {
		snapshot = capture()
		snapshot.At = event.At
		newSnapshot = &snapshot
	}
	l.mu.Lock()
	if newSnapshot != nil {
		newSnapshot.ID = l.newIDLocked()
		l.recent[key] = *newSnapshot
		l.snapshots[newSnapshot.ID] = *newSnapshot
		snapshot = *newSnapshot
	}
	if snapshot.ID != 0 {
		event.SnapshotID = snapshot.ID
	}
	event.ID = l.newIDLocked()
	l.events = append(l.events, event)
	if len(l.events) > errorEventMemoryLimit {
		l.events = append(l.events[:0:0], l.events[len(l.events)-errorEventMemoryLimit:]...)
		l.pruneSnapshotsLocked()
		for key, recent := range l.recent {
			if _, ok := l.snapshots[recent.ID]; !ok {
				delete(l.recent, key)
			}
		}
	}
	l.mu.Unlock()
	if l.path == "" {
		return
	}
	if newSnapshot != nil {
		l.enqueue(errorEventRecord{Snapshot: newSnapshot})
	}
	l.enqueue(errorEventRecord{Event: &event})
}

func (l *ErrorEventLog) newIDLocked() uint64 {
	id := uint64(time.Now().UnixNano())
	if id <= l.lastID {
		id = l.lastID + 1
	}
	l.lastID = id
	return id
}

func (l *ErrorEventLog) enqueue(record errorEventRecord) {
	select {
	case l.queue <- record:
	default:
		l.mu.Lock()
		l.dropped++
		l.mu.Unlock()
	}
}

// ErrorEventQuery filters ErrorEventLog.Query. Zero values match everything.
type ErrorEventQuery struct {
	Provider string
	Account  string
	Since    time.Time
	Limit    int
}

// ErrorEventReport is the /_subrouter/errors payload.
type ErrorEventReport struct {
	Events    []ErrorEvent                   `json:"events"`
	Snapshots map[string]AccountPoolSnapshot `json:"snapshots"`
	Summary   ErrorEventSummary              `json:"summary"`
	Dropped   uint64                         `json:"dropped_writes,omitempty"`
}

// ErrorEventSummary counts every matching event, not only the returned page.
type ErrorEventSummary struct {
	Total     int            `json:"total"`
	ByStatus  map[string]int `json:"by_status"`
	ByAccount map[string]int `json:"by_account"`
	ByType    map[string]int `json:"by_type"`
}

// Query returns matching events newest first, the snapshots they refer to,
// and counts over every match.
func (l *ErrorEventLog) Query(query ErrorEventQuery) ErrorEventReport {
	report := ErrorEventReport{
		Events:    []ErrorEvent{},
		Snapshots: map[string]AccountPoolSnapshot{},
		Summary:   ErrorEventSummary{ByStatus: map[string]int{}, ByAccount: map[string]int{}, ByType: map[string]int{}},
	}
	if l == nil {
		return report
	}
	limit := query.Limit
	if limit <= 0 {
		limit = 100
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	report.Dropped = l.dropped
	for i := len(l.events) - 1; i >= 0; i-- {
		event := l.events[i]
		if query.Provider != "" && string(event.Provider) != query.Provider {
			continue
		}
		if query.Account != "" && event.Account != query.Account {
			continue
		}
		if !query.Since.IsZero() && event.At.Before(query.Since) {
			break
		}
		report.Summary.Total++
		status := event.ErrorClass
		if event.Status != 0 {
			status = strconv.Itoa(event.Status)
		}
		report.Summary.ByStatus[status]++
		report.Summary.ByAccount[event.Account]++
		if event.ErrorType != "" {
			report.Summary.ByType[event.ErrorType]++
		} else {
			report.Summary.ByType[event.ErrorClass]++
		}
		if len(report.Events) >= limit {
			continue
		}
		report.Events = append(report.Events, event)
		if snapshot, ok := l.snapshots[event.SnapshotID]; ok {
			report.Snapshots[strconv.FormatUint(snapshot.ID, 10)] = snapshot
		}
	}
	return report
}

// recordUpstreamFailure records a failed physical attempt. It peeks a bounded
// prefix of an error body to name the failure and restores it, so the caller
// still returns the response intact.
func (s Server) recordUpstreamFailure(req *http.Request, a *upstreamAttempt, account accounts.Account, response *http.Response, err error) {
	if s.ErrorEvents == nil || a == nil {
		return
	}
	if err == nil && (response == nil || response.StatusCode < 400) {
		return
	}
	// The client went away: nothing upstream failed, and these would bury
	// the real failures.
	if errors.Is(err, context.Canceled) || req.Context().Err() == context.Canceled {
		return
	}
	provider := account.Provider
	if provider == "" {
		provider = a.provider
	}
	event := ErrorEvent{
		At:        time.Now().UTC(),
		Provider:  provider,
		Agent:     a.agent,
		Session:   a.session,
		Account:   account.ID,
		PoolModel: a.poolModel,
		Method:    req.Method,
		Path:      a.path,
	}
	if event.Path == "" && req.URL != nil {
		event.Path = req.URL.Path
	}
	if req.URL != nil {
		event.Upstream = req.URL.Host
	}
	if err != nil {
		event.ErrorClass = trafficErrorClass(err)
		if event.ErrorClass == "proxy_error" {
			event.ErrorClass = "transport_error"
		}
		event.Message = boundedErrorText(err.Error())
	} else {
		event.Status = response.StatusCode
		event.ErrorClass = upstreamStatusClass(response.StatusCode)
		event.ContentType = response.Header.Get("Content-Type")
		event.RetryAfter = response.Header.Get("Retry-After")
		event.RateLimit = rateLimitHeaders(response.Header)
		// An event stream can stall between events; only a finished error
		// document is safe to read ahead of the layers above.
		if response.Body != nil && response.Body != http.NoBody && !strings.Contains(event.ContentType, "event-stream") {
			prefix, readErr := io.ReadAll(io.LimitReader(response.Body, errorEventBodyPeekBytes))
			response.Body = prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), response.Body), Closer: response.Body}
			if readErr == nil {
				event.ErrorType, event.Message = upstreamErrorSummary(prefix, event.ContentType)
			}
		}
	}
	s.ErrorEvents.record(event, func() AccountPoolSnapshot {
		return s.accountPoolSnapshot(req.Context(), provider, a.poolModel)
	})
}

// recordStreamFailure records a refusal that arrived as the first event of a
// 200 stream, which the status check in recordUpstreamFailure cannot see.
func (s Server) recordStreamFailure(req *http.Request, a *upstreamAttempt, account accounts.Account, errorType string) {
	if s.ErrorEvents == nil || a == nil {
		return
	}
	provider := account.Provider
	if provider == "" {
		provider = a.provider
	}
	event := ErrorEvent{
		At: time.Now().UTC(), Provider: provider, Agent: a.agent, Session: a.session,
		Account: account.ID, PoolModel: a.poolModel, Method: req.Method, Path: a.path,
		Status: http.StatusOK, ErrorClass: "stream_error", ErrorType: errorType,
	}
	if req.URL != nil {
		event.Upstream = req.URL.Host
	}
	s.ErrorEvents.record(event, func() AccountPoolSnapshot {
		return s.accountPoolSnapshot(req.Context(), provider, a.poolModel)
	})
}

func upstreamStatusClass(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "auth"
	case status == http.StatusForbidden:
		return "forbidden"
	case status == http.StatusTooManyRequests:
		return "rate_limited"
	case status == 529 || status == http.StatusServiceUnavailable:
		return "overloaded"
	case status >= 500:
		return "upstream_5xx"
	default:
		return "upstream_4xx"
	}
}

func rateLimitHeaders(header http.Header) map[string]string {
	var out map[string]string
	for key, values := range header {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "anthropic-ratelimit") || strings.HasPrefix(lower, "x-codex-") ||
			strings.HasPrefix(lower, "x-ratelimit-") {
			if out == nil {
				out = map[string]string{}
			}
			out[lower] = truncateLogValue(strings.Join(values, ","))
		}
	}
	return out
}

// upstreamErrorSummary names an upstream failure from its body prefix. It
// reads the provider error envelopes ({"error":{"type","message"}},
// {"error":{"code"}}, {"detail"}) and, for HTML, the page title.
func upstreamErrorSummary(body []byte, contentType string) (string, string) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return "", ""
	}
	var payload map[string]any
	if json.Unmarshal(trimmed, &payload) == nil {
		errorType, message := "", ""
		if nested, ok := payload["error"].(map[string]any); ok {
			errorType = firstString(nested, "type", "code")
			message = firstString(nested, "message")
		} else if text, ok := payload["error"].(string); ok {
			message = text
		}
		if errorType == "" {
			errorType = firstString(payload, "type", "code")
		}
		if message == "" {
			message = firstString(payload, "message", "detail")
		}
		return strings.ToLower(errorType), boundedErrorText(message)
	}
	text := string(trimmed)
	lower := strings.ToLower(text)
	if strings.Contains(strings.ToLower(contentType), "html") || strings.HasPrefix(lower, "<") {
		if start := strings.Index(lower, "<title>"); start >= 0 {
			rest := text[start+len("<title>"):]
			if end := strings.Index(strings.ToLower(rest), "</title>"); end >= 0 {
				return "non_json", boundedErrorText(rest[:end])
			}
		}
		return "non_json", "html page"
	}
	return "non_json", boundedErrorText(text)
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := values[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func boundedErrorText(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= errorEventMessageMax {
		return text
	}
	cut := errorEventMessageMax
	for cut > 0 && (text[cut]&0xC0) == 0x80 {
		cut--
	}
	return text[:cut] + "…"
}

// accountPoolSnapshot reads every account of provider through the same
// scheduler view failover uses, with request-time marks layered on.
func (s Server) accountPoolSnapshot(ctx context.Context, provider accounts.Provider, poolModel string) AccountPoolSnapshot {
	snapshot := AccountPoolSnapshot{At: time.Now().UTC(), Provider: provider, PoolModel: poolModel}
	candidates := filterAccountsForProvider(s.accountListContext(ctx), provider)
	scheduler := s.scheduler()
	if s.Sessions != nil {
		scheduler = scheduler.WithSessionCounts(SchedulerSessionCounts(s.Sessions))
	}
	scoreProvider := schedulerAccountProvider(provider)
	inflight := map[string]int{}
	if s.SchedulerRef != nil {
		inflight = s.SchedulerRef.InflightCounts()
	}
	sessions := map[string]int{}
	if s.Sessions != nil {
		sessions = SchedulerSessionCounts(s.Sessions)
	}
	now := time.Now()
	for _, account := range candidates {
		score, known := scheduler.KnownScore(scoreProvider, account.ID)
		key := selectacct.ScoreKey(scoreProvider, account.ID)
		tier := scheduler.SelectionTier(account)
		state := AccountState{
			Account:        account.ID,
			AuthMode:       account.AuthMode,
			Tier:           selectionTierName(tier),
			Known:          known,
			Fresh:          score.Fresh,
			Headroom:       roundHeadroom(score.Headroom),
			ShortHeadroom:  roundHeadroom(score.ShortHeadroom),
			WeeklyHeadroom: roundHeadroom(score.WeeklyHeadroom),
			UsableNew: tier == selectacct.SelectionTierPromotedCredits || tier == selectacct.SelectionTierPlan ||
				tier == selectacct.SelectionTierAPIKey,
			Exhausted: tier == selectacct.SelectionTierPlanExhausted || tier == selectacct.SelectionTierExhaustedAPIKey,
			Inflight:  inflight[key],
			Sessions:  sessions[key],
		}
		if poolModel != "" {
			if modelScore, ok := score.ModelScores[poolModel]; ok {
				headroom := roundHeadroom(modelScore.Headroom)
				state.ModelHeadroom = &headroom
			}
		}
		if !score.ExhaustedResetAt.IsZero() {
			reset := score.ExhaustedResetAt.UTC()
			state.ResetAt = &reset
		}
		if s.SchedulerRef != nil {
			if until, blocked := s.SchedulerRef.BlockedUntilFor(scoreProvider, account.ID, poolModel, now); blocked {
				state.Blocked = true
				if !until.IsZero() {
					until = until.UTC()
					state.BlockedUntil = &until
				}
			}
		}
		snapshot.Total++
		if !state.Blocked && !state.Exhausted {
			snapshot.Usable++
			if state.UsableNew {
				snapshot.UsableNew++
			}
		}
		snapshot.Accounts = append(snapshot.Accounts, state)
	}
	// Most room first, so the head of the list is where the next pick should go.
	sort.SliceStable(snapshot.Accounts, func(i, j int) bool {
		left, right := snapshot.Accounts[i], snapshot.Accounts[j]
		if left.Blocked != right.Blocked {
			return !left.Blocked
		}
		return left.Headroom > right.Headroom
	})
	return snapshot
}

func roundHeadroom(value float64) float64 {
	return float64(int(value*1000+0.5)) / 1000
}

func selectionTierName(tier int) string {
	switch tier {
	case selectacct.SelectionTierPromotedCredits:
		return "promoted_credits"
	case selectacct.SelectionTierPlan:
		return "plan"
	case selectacct.SelectionTierAPIKey:
		return "api_key"
	case selectacct.SelectionTierPlanConstrained:
		return "plan_constrained"
	case selectacct.SelectionTierPlanExhausted:
		return "plan_exhausted"
	case selectacct.SelectionTierExhaustedAPIKey:
		return "api_key_exhausted"
	default:
		return "other"
	}
}

func (s Server) handleErrorEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	values := r.URL.Query()
	query := ErrorEventQuery{Provider: values.Get("provider"), Account: values.Get("account")}
	if raw := values.Get("since"); raw != "" {
		since, err := parseSinceParam(raw, time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		query.Since = since
	}
	if raw := values.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > errorEventMemoryLimit {
			http.Error(w, "limit must be 1..2000", http.StatusBadRequest)
			return
		}
		query.Limit = limit
	}
	writeJSON(w, s.ErrorEvents.Query(query))
}

func (s Server) handleAccountState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	values := r.URL.Query()
	providers := []accounts.Provider{accounts.ProviderClaude, accounts.ProviderCodex}
	if raw := values.Get("provider"); raw != "" {
		providers = []accounts.Provider{accounts.Provider(raw)}
	}
	out := make([]AccountPoolSnapshot, 0, len(providers))
	for _, provider := range providers {
		out = append(out, s.accountPoolSnapshot(r.Context(), provider, values.Get("pool_model")))
	}
	writeJSON(w, out)
}

// parseSinceParam accepts an RFC 3339 time or a duration back from now.
func parseSinceParam(raw string, now time.Time) (time.Time, error) {
	if since, err := time.Parse(time.RFC3339, raw); err == nil {
		return since, nil
	}
	duration, err := time.ParseDuration(raw)
	if err != nil || duration <= 0 {
		return time.Time{}, errors.New("since must be RFC 3339 or a positive duration such as 1h")
	}
	return now.Add(-duration), nil
}
