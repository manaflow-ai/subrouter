package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

const (
	usageCalibrationQueueSize      = 64
	usageCalibrationRecordMaxBytes = 64 << 10
	usageCalibrationFileMaxBytes   = 32 << 20
)

type calibrationCounterKey struct{ account, model string }

// These counters are process-local and monotonic, not hourly ledger rows.
type calibrationCounters struct {
	Model                 string `json:"model"`
	Requests              int64  `json:"requests"`
	RequestsWithoutUsage  int64  `json:"requests_without_usage"`
	InputTokens           int64  `json:"input_tokens"`
	CachedInputTokens     int64  `json:"cached_input_tokens"`
	CacheWriteInputTokens int64  `json:"cache_write_input_tokens"`
	OutputTokens          int64  `json:"output_tokens"`
}

type calibrationWindow struct {
	Name               string    `json:"name"`
	UsedPercent        float64   `json:"used_percent"`
	LimitWindowSeconds int64     `json:"limit_window_seconds"`
	ResetAt            time.Time `json:"reset_at"`
}

type calibrationObservation struct {
	SchemaVersion       int                   `json:"schema_version"`
	Purpose             string                `json:"purpose"`
	WorkerEpoch         string                `json:"worker_epoch"`
	Sequence            uint64                `json:"sequence"`
	AccountKey          string                `json:"account_key"`
	AccountGeneration   string                `json:"account_generation"`
	Provider            string                `json:"provider"`
	PlanType            string                `json:"plan_type"`
	ProviderObservedAt  time.Time             `json:"provider_observed_at"`
	CountersSampledAt   time.Time             `json:"counters_sampled_at"`
	DroppedObservations uint64                `json:"dropped_observations"`
	Limitations         []string              `json:"limitations"`
	Windows             []calibrationWindow   `json:"windows"`
	Counters            []calibrationCounters `json:"counters"`
}

// UsageCalibrationRecorder collects private evidence only. It never changes
// routing and exposes no HTTP endpoint. Call Run once for the worker lifetime.
type UsageCalibrationRecorder struct {
	dir        string
	epoch      string
	startedAt  time.Time
	mu         sync.Mutex
	counters   map[calibrationCounterKey]calibrationCounters
	seen       map[string]time.Time
	sequence   uint64
	dropped    uint64
	incomplete bool
	queue      chan []byte
	runOnce    sync.Once
}

func NewUsageCalibrationRecorder(dir string) *UsageCalibrationRecorder {
	return &UsageCalibrationRecorder{dir: dir, epoch: rand.Text(), startedAt: time.Now(), counters: map[calibrationCounterKey]calibrationCounters{}, seen: map[string]time.Time{}, queue: make(chan []byte, usageCalibrationQueueSize)}
}

func (r *AccountRef) SetUsageCalibrationRecorder(recorder *UsageCalibrationRecorder) {
	if r != nil {
		r.calibrationRecorder.Store(recorder)
	}
}

func WithUsageCalibrationRecorder(recorder *UsageCalibrationRecorder) TokenUsageRecorderOption {
	return func(r *TokenUsageRecorder) { r.calibration = recorder }
}

func calibrationAccountKey(provider, id string) string {
	sum := sha256.Sum256([]byte(provider + "\x00" + id))
	return hex.EncodeToString(sum[:])
}

// A merged last-good status can contain old account-wide windows alongside a
// new model-only reading. Use only the exact cache entry that produced this
// observation, bound to the current credential, rather than the merged view.
func (r *AccountRef) observeUsageCalibration(c *UsageCalibrationRecorder, status AccountUsageStatus, account accounts.Account, generation string) {
	key := account.ID + "\x00" + string(accountProviderOrCodex(account))
	credentialKey := usageWindowsCredentialKey(account)
	r.usageWindowsMu.Lock()
	entry, ok := r.usageWindows[key]
	if ok {
		entry.windows = append([]accounts.UsageWindow(nil), entry.windows...)
	}
	r.usageWindowsMu.Unlock()
	if !ok || entry.credentialKey != credentialKey || !entry.at.Equal(status.UsageFetchedAt) {
		return
	}
	status.Windows = entry.windows
	c.observe(status, generation)
}

// record is called with already-normalized accounting, never response content.
// It uses bounded memory and performs no I/O on the request path.
func (c *UsageCalibrationRecorder) record(key tokenUsageKey, counts tokenUsageCounts) {
	if c == nil || counts.Requests == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if key.Model == tokenUsageOverflowLabel {
		c.incomplete = true
	}
	k := calibrationCounterKey{calibrationAccountKey(key.Provider, key.AccountID), key.Model}
	value, exists := c.counters[k]
	if !exists && len(c.counters) >= tokenUsageMaxKeysPerHour {
		c.incomplete = true
		return
	}
	value.Model = key.Model
	for _, pair := range []struct {
		dst *int64
		src int64
	}{
		{&value.Requests, counts.Requests}, {&value.RequestsWithoutUsage, counts.RequestsWithoutUsage},
		{&value.InputTokens, counts.InputTokens}, {&value.CachedInputTokens, counts.CachedInputTokens},
		{&value.CacheWriteInputTokens, counts.CacheWriteInputTokens}, {&value.OutputTokens, counts.OutputTokens},
	} {
		if pair.src < 0 || pair.src > math.MaxInt64-*pair.dst {
			c.incomplete = true
			return
		}
		*pair.dst += pair.src
	}
	c.counters[k] = value
}

// observe is fed by the existing usage persistence seam before freshness is
// cleared for the durable last-good cache. Cache hits deduplicate by observation
// timestamp. Partial model-only or paid-extra observations are not samples.
func (c *UsageCalibrationRecorder) observe(status AccountUsageStatus, generation string) {
	if c == nil || status.UsageThrottled || status.UsageFetchedAt.IsZero() || status.UsageFetchedAt.Before(c.startedAt) || (!status.UsageFresh && !status.AccountWideUsageFresh) {
		return
	}
	windows := []calibrationWindow{}
	for _, window := range status.Windows {
		if window.Feature != "" || window.ExtraUsage != nil {
			continue
		}
		if math.IsNaN(window.UsedPercent) || math.IsInf(window.UsedPercent, 0) {
			return
		}
		if len(windows) >= 32 {
			return
		}
		windows = append(windows, calibrationWindow{tokenUsageLabel(window.Name, "unknown"), window.UsedPercent, window.LimitWindowSeconds, window.ResetTime(status.UsageFetchedAt)})
	}
	if len(windows) == 0 {
		return
	}
	key := calibrationAccountKey(string(status.Provider), status.ID)
	c.mu.Lock()
	previous, exists := c.seen[key]
	if exists && !status.UsageFetchedAt.After(previous) {
		c.mu.Unlock()
		return
	}
	if !exists && len(c.seen) >= tokenUsageMaxKeysPerHour {
		c.dropped++
		c.mu.Unlock()
		return
	}
	c.seen[key] = status.UsageFetchedAt
	c.sequence++
	observation := calibrationObservation{
		SchemaVersion: 1, Purpose: "quota_token_calibration", WorkerEpoch: c.epoch, Sequence: c.sequence,
		AccountKey: key, AccountGeneration: generation, Provider: string(status.Provider), PlanType: tokenUsageLabel(status.PlanType, "unknown"),
		ProviderObservedAt: status.UsageFetchedAt.UTC(), CountersSampledAt: time.Now().UTC(), DroppedObservations: c.dropped,
		Limitations: []string{"worker_local_completion_counters", "outside_server_usage_unverified", "upstream_error_usage_unverified", "provider_and_counter_snapshots_not_atomic", "account_generation_capture_not_atomic", "not_request_order_replay"},
		Windows:     windows, Counters: []calibrationCounters{},
	}
	if c.incomplete {
		observation.Limitations = append(observation.Limitations, "counter_capacity_or_overflow")
	}
	for counterKey, value := range c.counters {
		if counterKey.account == key {
			observation.Counters = append(observation.Counters, value)
		}
	}
	c.mu.Unlock()
	sort.Slice(observation.Counters, func(i, j int) bool { return observation.Counters[i].Model < observation.Counters[j].Model })
	body, err := json.Marshal(observation)
	if err != nil || len(body)+1 > usageCalibrationRecordMaxBytes {
		c.dropObservation()
		return
	}
	select {
	case c.queue <- append(body, '\n'):
	default:
		c.dropObservation()
	}
}

// Run keeps filesystem work off request and usage-sweep paths. Write errors
// leave sequence gaps and increment dropped_observations in the next sample.
func (c *UsageCalibrationRecorder) Run(ctx context.Context) {
	if c == nil || c.dir == "" {
		return
	}
	c.runOnce.Do(func() {
		for {
			select {
			case body := <-c.queue:
				c.writeOrDrop(body)
			case <-ctx.Done():
				for {
					select {
					case body := <-c.queue:
						c.writeOrDrop(body)
					default:
						return
					}
				}
			}
		}
	})
}

func (c *UsageCalibrationRecorder) writeOrDrop(body []byte) {
	if err := c.write(body); err != nil {
		c.dropObservation()
	}
}

func (c *UsageCalibrationRecorder) dropObservation() {
	c.mu.Lock()
	c.dropped++
	c.mu.Unlock()
}

func (c *UsageCalibrationRecorder) write(body []byte) error {
	if len(body) > usageCalibrationRecordMaxBytes {
		return fmt.Errorf("calibration record too large")
	}
	// Reuse the cross-platform nonblocking file lock in this dedicated directory,
	// never the real account-store directory. Rotation is serialized across workers.
	lock, err := tryLockAccountImportTransaction(context.Background(), c.dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := os.Chmod(c.dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(c.dir, "observations.jsonl")
	if info, err := os.Stat(path); err == nil && info.Size()+int64(len(body)) > usageCalibrationFileMaxBytes {
		// Windows does not replace an existing destination with Rename.
		if err := os.Remove(path + ".1"); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Rename(path, path+".1"); err != nil {
			return err
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	n, err := file.Write(body)
	if err == nil && n != len(body) {
		return fmt.Errorf("short calibration write")
	}
	return err
}
