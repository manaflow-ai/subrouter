package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

const (
	usageThrottleStateSchemaVersion = 1
	usageThrottleStateDir           = ".usage-throttle"
	usageThrottleStateSuffix        = ".json"
)

var errPersistedUsageThrottle = errors.New("usage fetch failed: 429 Too Many Requests")

type durableUsageThrottle struct {
	SchemaVersion int                    `json:"schema_version"`
	CacheKey      string                 `json:"cache_key"`
	RetryAt       time.Time              `json:"retry_at"`
	Windows       []accounts.UsageWindow `json:"windows,omitempty"`
	FetchedAt     time.Time              `json:"fetched_at,omitempty"`
}

// usageThrottleStatePath returns the private, per-account file used to share
// a provider Retry-After deadline between worker processes without putting an
// account ID or credential in a filename.
func usageThrottleStatePath(store accounts.CodexStore, cacheKey string) string {
	if store.StoreDir() == "" || cacheKey == "" {
		return ""
	}
	hash := sha256.Sum256([]byte(cacheKey))
	return filepath.Join(store.StoreDir(), usageThrottleStateDir, hex.EncodeToString(hash[:])+usageThrottleStateSuffix)
}

// restoreUsageThrottleState rehydrates active account throttles at worker
// startup. Expired entries are removed opportunistically; positive usage
// windows remain in the existing status snapshot and stay credential-fenced.
func (r *AccountRef) restoreUsageThrottleState() {
	if r == nil {
		return
	}
	all, _ := r.Snapshot()
	known := make(map[string]struct{}, len(all))
	for _, account := range all {
		known[account.ID+"\x00"+string(account.Provider)] = struct{}{}
	}
	dir := filepath.Join(r.store.StoreDir(), usageThrottleStateDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	now := time.Now()
	r.usageWindowsMu.Lock()
	defer r.usageWindowsMu.Unlock()
	if r.usageWindowsFailures == nil {
		r.usageWindowsFailures = make(map[string]usageWindowsFailure)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != usageThrottleStateSuffix {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			continue
		}
		var saved durableUsageThrottle
		if json.Unmarshal(body, &saved) != nil || saved.SchemaVersion != usageThrottleStateSchemaVersion ||
			saved.CacheKey == "" || !now.Before(saved.RetryAt) {
			_ = os.Remove(path)
			continue
		}
		if _, ok := known[saved.CacheKey]; !ok {
			_ = os.Remove(path)
			continue
		}
		if prior, ok := r.usageWindowsFailures[saved.CacheKey]; !ok || saved.RetryAt.After(prior.retryAt) {
			r.usageWindowsFailures[saved.CacheKey] = usageWindowsFailure{
				err: errPersistedUsageThrottle, retryAt: saved.RetryAt,
				windows: append([]accounts.UsageWindow(nil), saved.Windows...), fetchedAt: saved.FetchedAt,
			}
		}
	}
}

// persistUsageThrottleState records one active account throttle atomically so
// a second worker can reuse the same deadline without seeing partial JSON.
func (r *AccountRef) persistUsageThrottleState(cacheKey string, failure usageWindowsFailure) {
	if r == nil || cacheKey == "" || failure.retryAt.IsZero() {
		return
	}
	dir := filepath.Join(r.store.StoreDir(), usageThrottleStateDir)
	if dir == usageThrottleStateDir {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	saved := durableUsageThrottle{
		SchemaVersion: usageThrottleStateSchemaVersion,
		CacheKey:      cacheKey,
		RetryAt:       failure.retryAt,
		Windows:       append([]accounts.UsageWindow(nil), failure.windows...),
		FetchedAt:     failure.fetchedAt,
	}
	body, err := json.Marshal(saved)
	if err != nil {
		return
	}
	temp, err := os.CreateTemp(dir, ".usage-throttle-*")
	if err != nil {
		return
	}
	tempPath := temp.Name()
	ok := false
	defer func() {
		_ = temp.Close()
		if !ok {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return
	}
	if _, err := temp.Write(body); err != nil {
		return
	}
	if err := temp.Sync(); err != nil {
		return
	}
	if err := temp.Close(); err != nil {
		return
	}
	if err := os.Rename(tempPath, usageThrottleStatePath(r.store, cacheKey)); err != nil {
		return
	}
	_ = syncAccountStateDir(r.store.StoreDir())
	ok = true
}

// clearUsageThrottleState removes a persisted deadline when a true re-login
// replaces the account, allowing the replacement to make one fresh probe.
func (r *AccountRef) clearUsageThrottleState(cacheKey string) {
	if r == nil {
		return
	}
	if path := usageThrottleStatePath(r.store, cacheKey); path != "" {
		_ = os.Remove(path)
	}
}
