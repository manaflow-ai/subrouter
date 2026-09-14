package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	cacheStatsFlushInterval = 5 * time.Second
	cacheStatsLockTimeout   = 2 * time.Second
	cacheStatsMaxModels     = 16
)

// CacheStats is a small, durable aggregate of provider prompt-cache results.
// It stores counters only. Request and response bodies never enter this type.
// The pending delta is merged into the shared snapshot, so overlapping worker
// generations do not overwrite each other's observations during a supervisor
// upgrade.
type CacheStats struct {
	mu        sync.Mutex
	flushMu   sync.Mutex
	path      string
	started   time.Time
	updated   time.Time
	totals    cacheStatsCounters
	models    map[string]cacheStatsCounters
	pending   cacheStatsDelta
	lastError string
}

type cacheStatsCounters struct {
	Responses           uint64 `json:"responses"`
	HitResponses        uint64 `json:"hit_responses"`
	FullHitResponses    uint64 `json:"full_hit_responses"`
	PartialHitResponses uint64 `json:"partial_hit_responses"`
	MissResponses       uint64 `json:"miss_responses"`
	InputTokens         uint64 `json:"input_tokens"`
	CachedInputTokens   uint64 `json:"cached_input_tokens"`
	UncachedInputTokens uint64 `json:"uncached_input_tokens"`
	ColdAccountMoves    uint64 `json:"cold_account_moves"`
}

type cacheStatsDelta struct {
	counters cacheStatsCounters
	models   map[string]cacheStatsCounters
}

// CacheStatsSnapshot is the endpoint and dashboard representation. Ratios are
// derived from integer counters so the persisted file stays compact and exact.
type CacheStatsSnapshot struct {
	StartedAt           string                    `json:"started_at,omitempty"`
	UpdatedAt           string                    `json:"updated_at,omitempty"`
	Responses           uint64                    `json:"responses"`
	HitResponses        uint64                    `json:"hit_responses"`
	FullHitResponses    uint64                    `json:"full_hit_responses"`
	PartialHitResponses uint64                    `json:"partial_hit_responses"`
	MissResponses       uint64                    `json:"miss_responses"`
	InputTokens         uint64                    `json:"input_tokens"`
	CachedInputTokens   uint64                    `json:"cached_input_tokens"`
	UncachedInputTokens uint64                    `json:"uncached_input_tokens"`
	ColdAccountMoves    uint64                    `json:"cold_account_moves"`
	RequestHitRate      float64                   `json:"request_hit_rate"`
	TokenHitRate        float64                   `json:"token_hit_rate"`
	ByModel             []CacheStatsModelSnapshot `json:"by_model,omitempty"`
	PersistenceError    string                    `json:"persistence_error,omitempty"`
}

type CacheStatsModelSnapshot struct {
	Model string `json:"model"`
	cacheStatsCounters
	RequestHitRate float64 `json:"request_hit_rate"`
	TokenHitRate   float64 `json:"token_hit_rate"`
}

type cacheStatsFile struct {
	StartedAt string                        `json:"started_at,omitempty"`
	UpdatedAt string                        `json:"updated_at,omitempty"`
	Totals    cacheStatsCounters            `json:"totals"`
	Models    map[string]cacheStatsCounters `json:"models,omitempty"`
}

func NewCacheStats(path string) *CacheStats {
	s := &CacheStats{path: strings.TrimSpace(path), models: make(map[string]cacheStatsCounters)}
	s.load()
	if s.started.IsZero() {
		s.started = time.Now().UTC()
	}
	return s
}

// Start periodically persists only the counters added by this worker.
func (s *CacheStats) Start(ctx context.Context) {
	if s == nil || s.path == "" {
		return
	}
	go func() {
		ticker := time.NewTicker(cacheStatsFlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = s.Flush()
			case <-ctx.Done():
				_ = s.Flush()
				return
			}
		}
	}()
}

func (s *CacheStats) ObserveUsage(model string, inputTokens, cachedTokens int64) {
	if s == nil || inputTokens <= 0 {
		return
	}
	input := uint64(inputTokens)
	cached := uint64(0)
	if cachedTokens > 0 {
		cached = uint64(cachedTokens)
	}
	if cached > input {
		cached = input
	}
	c := cacheStatsCounters{
		Responses:           1,
		InputTokens:         input,
		CachedInputTokens:   cached,
		UncachedInputTokens: input - cached,
	}
	if cached > 0 {
		c.HitResponses = 1
		if cached == input {
			c.FullHitResponses = 1
		} else {
			c.PartialHitResponses = 1
		}
	} else {
		c.MissResponses = 1
	}
	model = strings.TrimSpace(model)
	if len(model) > 128 {
		model = ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totals = addCacheStatsCounters(s.totals, c)
	s.pending.counters = addCacheStatsCounters(s.pending.counters, c)
	if model != "" {
		if _, ok := s.models[model]; ok || len(s.models) < cacheStatsMaxModels {
			s.models[model] = addCacheStatsCounters(s.models[model], c)
			if s.pending.models == nil {
				s.pending.models = make(map[string]cacheStatsCounters)
			}
			s.pending.models[model] = addCacheStatsCounters(s.pending.models[model], c)
		}
	}
	s.updated = time.Now().UTC()
}

func (s *CacheStats) ObserveColdAccountMove() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.totals.ColdAccountMoves++
	s.pending.counters.ColdAccountMoves++
	s.updated = time.Now().UTC()
	s.mu.Unlock()
}

func (s *CacheStats) Snapshot() CacheStatsSnapshot {
	if s == nil {
		return CacheStatsSnapshot{}
	}
	s.mu.Lock()
	started, updated := s.started, s.updated
	lastError := s.lastError
	totals := s.totals
	models := make(map[string]cacheStatsCounters, len(s.models))
	for model, counters := range s.models {
		models[model] = counters
	}
	s.mu.Unlock()
	snapshot := makeCacheStatsSnapshot(started, updated, totals, models)
	snapshot.PersistenceError = lastError
	return snapshot
}

func (s *CacheStats) Flush() error {
	if s == nil || s.path == "" {
		return nil
	}
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	delta := s.pending
	if delta.counters == (cacheStatsCounters{}) && len(delta.models) == 0 {
		s.mu.Unlock()
		return nil
	}
	s.pending = cacheStatsDelta{}
	started := s.started
	updated := s.updated
	s.mu.Unlock()

	if err := mergeCacheStatsFile(s.path, started, updated, delta); err != nil {
		s.mu.Lock()
		s.lastError = err.Error()
		s.pending.counters = addCacheStatsCounters(s.pending.counters, delta.counters)
		if len(delta.models) > 0 {
			if s.pending.models == nil {
				s.pending.models = make(map[string]cacheStatsCounters)
			}
			for model, counters := range delta.models {
				s.pending.models[model] = addCacheStatsCounters(s.pending.models[model], counters)
			}
		}
		s.mu.Unlock()
		return err
	}
	s.mu.Lock()
	s.lastError = ""
	s.mu.Unlock()
	return nil
}

func (s *CacheStats) load() {
	if s.path == "" {
		return
	}
	body, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var file cacheStatsFile
	if json.Unmarshal(body, &file) != nil {
		return
	}
	s.totals = file.Totals
	for model, counters := range file.Models {
		if len(s.models) >= cacheStatsMaxModels {
			break
		}
		s.models[model] = counters
	}
	if file.StartedAt != "" {
		s.started, _ = time.Parse(time.RFC3339Nano, file.StartedAt)
	}
	if file.UpdatedAt != "" {
		s.updated, _ = time.Parse(time.RFC3339Nano, file.UpdatedAt)
	}
}

func (s Server) handleCacheStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.CacheStats.Snapshot())
}

func addCacheStatsCounters(left, right cacheStatsCounters) cacheStatsCounters {
	left.Responses += right.Responses
	left.HitResponses += right.HitResponses
	left.FullHitResponses += right.FullHitResponses
	left.PartialHitResponses += right.PartialHitResponses
	left.MissResponses += right.MissResponses
	left.InputTokens += right.InputTokens
	left.CachedInputTokens += right.CachedInputTokens
	left.UncachedInputTokens += right.UncachedInputTokens
	left.ColdAccountMoves += right.ColdAccountMoves
	return left
}

func makeCacheStatsSnapshot(started, updated time.Time, totals cacheStatsCounters, models map[string]cacheStatsCounters) CacheStatsSnapshot {
	snapshot := CacheStatsSnapshot{
		Responses: totals.Responses, HitResponses: totals.HitResponses,
		FullHitResponses: totals.FullHitResponses, PartialHitResponses: totals.PartialHitResponses,
		MissResponses: totals.MissResponses, InputTokens: totals.InputTokens,
		CachedInputTokens: totals.CachedInputTokens, UncachedInputTokens: totals.UncachedInputTokens,
		ColdAccountMoves: totals.ColdAccountMoves,
	}
	if !started.IsZero() {
		snapshot.StartedAt = started.UTC().Format(time.RFC3339Nano)
	}
	if !updated.IsZero() {
		snapshot.UpdatedAt = updated.UTC().Format(time.RFC3339Nano)
	}
	snapshot.RequestHitRate = ratio(totals.HitResponses, totals.Responses)
	snapshot.TokenHitRate = ratio(totals.CachedInputTokens, totals.InputTokens)
	for model, counters := range models {
		snapshot.ByModel = append(snapshot.ByModel, CacheStatsModelSnapshot{
			Model: model, cacheStatsCounters: counters,
			RequestHitRate: ratio(counters.HitResponses, counters.Responses),
			TokenHitRate:   ratio(counters.CachedInputTokens, counters.InputTokens),
		})
	}
	sort.Slice(snapshot.ByModel, func(i, j int) bool { return snapshot.ByModel[i].Model < snapshot.ByModel[j].Model })
	return snapshot
}

func ratio(numerator, denominator uint64) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

func mergeCacheStatsFile(path string, started, updated time.Time, delta cacheStatsDelta) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lock, err := acquireCacheStatsLock(path)
	if err != nil {
		return err
	}
	defer lock.Close()

	file := cacheStatsFile{Models: make(map[string]cacheStatsCounters)}
	if body, readErr := os.ReadFile(path); readErr == nil {
		if err := json.Unmarshal(body, &file); err != nil {
			return fmt.Errorf("read cache stats snapshot: %w", err)
		}
		if file.Models == nil {
			file.Models = make(map[string]cacheStatsCounters)
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	file.Totals = addCacheStatsCounters(file.Totals, delta.counters)
	for model, counters := range delta.models {
		if _, ok := file.Models[model]; ok || len(file.Models) < cacheStatsMaxModels {
			file.Models[model] = addCacheStatsCounters(file.Models[model], counters)
		}
	}
	if file.StartedAt == "" {
		file.StartedAt = started.UTC().Format(time.RFC3339Nano)
	} else if existingStarted, parseErr := time.Parse(time.RFC3339Nano, file.StartedAt); parseErr == nil && !started.IsZero() && started.Before(existingStarted) {
		file.StartedAt = started.UTC().Format(time.RFC3339Nano)
	}
	if updated.IsZero() {
		updated = time.Now().UTC()
	}
	file.UpdatedAt = updated.UTC().Format(time.RFC3339Nano)
	body, err := json.Marshal(file)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cache-stats-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(body, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

const cacheUsageMaxLineBytes = 2 << 20

// cacheUsageObserver extracts only response usage envelopes from a streamed
// body. It keeps at most one incomplete SSE line, never the conversation.
type cacheUsageObserver struct {
	stats    *CacheStats
	model    string
	buffer   []byte
	observed bool
}

func newCacheUsageObserver(stats *CacheStats, model string) *cacheUsageObserver {
	if stats == nil {
		return nil
	}
	return &cacheUsageObserver{stats: stats, model: strings.TrimSpace(model)}
}

func (o *cacheUsageObserver) ObserveMessage(body []byte) {
	if o == nil || o.observed {
		return
	}
	o.observeJSON(body)
}

func (o *cacheUsageObserver) ObserveChunk(body []byte) {
	if o == nil || o.observed || len(body) == 0 {
		return
	}
	o.buffer = append(o.buffer, body...)
	for {
		index := -1
		for i, value := range o.buffer {
			if value == '\n' {
				index = i
				break
			}
		}
		if index < 0 {
			break
		}
		line := bytesTrimSpace(o.buffer[:index])
		o.buffer = o.buffer[index+1:]
		if len(line) >= 5 && string(line[:5]) == "data:" {
			line = bytesTrimSpace(line[5:])
		}
		if len(line) > 0 && string(line) != "[DONE]" {
			o.observeJSON(line)
			if o.observed {
				return
			}
		}
	}
	if len(o.buffer) > cacheUsageMaxLineBytes {
		// A normal response.completed event is much smaller. Discarding an
		// oversized incomplete line bounds memory on malformed or non-SSE data.
		o.buffer = o.buffer[len(o.buffer)-cacheUsageMaxLineBytes:]
	}
}

func (o *cacheUsageObserver) Finish() {
	if o == nil || o.observed || len(o.buffer) == 0 {
		return
	}
	line := bytesTrimSpace(o.buffer)
	if len(line) >= 5 && string(line[:5]) == "data:" {
		line = bytesTrimSpace(line[5:])
	}
	o.observeJSON(line)
}

func (o *cacheUsageObserver) observeJSON(body []byte) {
	if !bytes.Contains(body, []byte(`"usage"`)) {
		return
	}
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return
	}
	container := root
	model := stringValue(root["model"])
	if response, ok := root["response"].(map[string]any); ok {
		container = response
		if model == "" {
			model = stringValue(response["model"])
		}
	}
	usage, ok := container["usage"].(map[string]any)
	if !ok {
		return
	}
	input, ok := integerValue(usage["input_tokens"])
	if !ok || input <= 0 {
		input, ok = integerValue(usage["prompt_tokens"])
	}
	if !ok || input <= 0 {
		return
	}
	cached := int64(0)
	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		cached, _ = integerValue(details["cached_tokens"])
	}
	if cached < 0 {
		cached = 0
	}
	o.observed = true
	if model == "" {
		model = o.model
	}
	o.stats.ObserveUsage(model, input, cached)
}

func stringValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func integerValue(value any) (int64, bool) {
	switch value := value.(type) {
	case float64:
		return int64(value), value >= 0
	case int64:
		return value, value >= 0
	case int:
		return int64(value), value >= 0
	default:
		return 0, false
	}
}

func bytesTrimSpace(value []byte) []byte {
	return bytes.TrimSpace(value)
}
