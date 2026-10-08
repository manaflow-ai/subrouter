package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

const usageStatusSnapshotSchemaVersion = 1

const usageStatusSnapshotFile = ".usage-status-snapshot.json"

func (r *AccountRef) invalidateUsageStatusSnapshotPersistence() {
	if r == nil {
		return
	}
	if path := usageStatusSnapshotPath(r.store); path != "" {
		_ = os.Remove(path)
	}
	r.InvalidateUsageStatusCache()
}

// durableUsageStatusSnapshot is the controller's last-known usage view. The
// credential keys are one-way hashes so a local status cache never writes
// bearer or API-key material to disk. AccountKey fences the whole snapshot
// against additions, removals, and credential rotations.
type durableUsageStatusSnapshot struct {
	SchemaVersion  int                             `json:"schema_version"`
	SavedAt        time.Time                       `json:"saved_at"`
	DiskGeneration string                          `json:"disk_generation,omitempty"`
	AccountKey     string                          `json:"account_key"`
	Rows           []durableUsageStatusSnapshotRow `json:"rows"`
}

type durableUsageStatusSnapshotRow struct {
	ID            string             `json:"id"`
	Provider      accounts.Provider  `json:"provider"`
	CredentialKey string             `json:"credential_key"`
	Status        AccountUsageStatus `json:"status"`
}

func usageStatusSnapshotPath(store accounts.CodexStore) string {
	if strings.TrimSpace(store.Dir) == "" {
		return ""
	}
	return filepath.Join(store.StoreDir(), usageStatusSnapshotFile)
}

func usageStatusCredentialKey(account accounts.Account) string {
	sum := sha256.Sum256([]byte(account.CredentialIdentity()))
	return hex.EncodeToString(sum[:])
}

func usageStatusAccountKey(all []accounts.Account) string {
	keys := make([]string, 0, len(all))
	for _, account := range all {
		keys = append(keys, string(accountProviderOrCodex(account))+"\x00"+account.ID+"\x00"+usageStatusCredentialKey(account))
	}
	sort.Strings(keys)
	hash := sha256.New()
	for _, key := range keys {
		_, _ = hash.Write([]byte(key))
		_, _ = hash.Write([]byte{'\n'})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (r *AccountRef) restoreUsageStatusSnapshot() {
	if r == nil {
		return
	}
	path := usageStatusSnapshotPath(r.store)
	if path == "" {
		return
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		return
	}
	var snapshot durableUsageStatusSnapshot
	if err := json.Unmarshal(body, &snapshot); err != nil || snapshot.SchemaVersion != usageStatusSnapshotSchemaVersion {
		return
	}
	if snapshot.SavedAt.IsZero() || snapshot.AccountKey == "" {
		return
	}
	all, _ := r.Snapshot()
	if usageStatusAccountKey(all) != snapshot.AccountKey {
		return
	}
	r.mu.RLock()
	diskGeneration := r.diskGeneration
	r.mu.RUnlock()
	if snapshot.DiskGeneration != "" && snapshot.DiskGeneration != diskGeneration {
		return
	}

	accountByKey := make(map[string]accounts.Account, len(all))
	for _, account := range all {
		key := string(accountProviderOrCodex(account)) + "\x00" + account.ID
		accountByKey[key] = account
	}
	rows := make([]AccountUsageStatus, 0, len(snapshot.Rows))
	activeCodex, _ := r.store.DetectActiveAccount()
	activeClaude := r.claudeStore.ActiveProfile()
	for _, saved := range snapshot.Rows {
		account, ok := accountByKey[string(saved.Provider)+"\x00"+saved.ID]
		if !ok || usageStatusCredentialKey(account) != saved.CredentialKey {
			return
		}
		status := saved.Status
		// Snapshots written before UsageThrottled was split from quota status
		// used quota_status=throttled for a plain telemetry 429. Normalize that
		// legacy spelling on restore so a restart cannot reintroduce the false
		// quota state.
		if status.QuotaStatus == "throttled" {
			status.UsageThrottled = true
			status.QuotaStatus = ""
		}
		// A restart restores observations, not a false claim of live freshness.
		// Keep the provider observation time so reset windows remain visible.
		status.UsageFresh = false
		status.Error = ""
		// Older durable snapshots did not persist UsageFetchedAt. Their rows
		// still contain the provider windows, so use the snapshot write time as
		// the best available observation time instead of dropping those windows
		// from last-good recovery (and rendering blank reset cells after a 429).
		if status.UsageFetchedAt.IsZero() && len(status.Windows) > 0 {
			status.UsageFetchedAt = snapshot.SavedAt
		}
		status.Active = (status.Provider == accounts.ProviderClaude && status.ID == activeClaude) ||
			(status.Provider != accounts.ProviderClaude && status.ID == activeCodex)
		rows = append(rows, status)
	}
	if len(rows) == 0 {
		return
	}

	r.usageStatusMu.Lock()
	r.usageStatusCache = rows
	r.usageStatusAt = time.Now()
	if r.lastGoodUsage == nil {
		r.lastGoodUsage = make(map[string]usageStatusSnapshot, len(rows))
	}
	for _, status := range rows {
		if status.UsageFetchedAt.IsZero() || len(status.Windows) == 0 {
			continue
		}
		key := status.ID + "\x00" + string(status.Provider)
		r.lastGoodUsage[key] = usageStatusSnapshot{status: status, at: status.UsageFetchedAt}
	}
	r.usageStatusMu.Unlock()
	// Rehydrate the per-account fallback cache as well as the rendered rows.
	// An explicit live refresh must be able to retain these windows when the
	// provider answers with a transient 429 after a restart.
	r.usageWindowsMu.Lock()
	if r.usageWindows == nil {
		r.usageWindows = make(map[string]usageWindowsEntry, len(rows))
	}
	for _, status := range rows {
		if status.UsageFetchedAt.IsZero() || len(status.Windows) == 0 {
			continue
		}
		key := status.ID + "\x00" + string(status.Provider)
		account, ok := accountByKey[string(status.Provider)+"\x00"+status.ID]
		if !ok {
			continue
		}
		r.usageWindows[key] = usageWindowsEntry{
			windows:       append([]accounts.UsageWindow(nil), status.Windows...),
			at:            status.UsageFetchedAt,
			credentialKey: usageWindowsCredentialKey(account),
		}
	}
	r.usageWindowsMu.Unlock()
}

func (r *AccountRef) persistUsageStatusSnapshot(statuses []AccountUsageStatus) {
	if r == nil || len(statuses) == 0 {
		return
	}
	path := usageStatusSnapshotPath(r.store)
	if path == "" {
		return
	}
	r.usageStatusPersistMu.Lock()
	defer r.usageStatusPersistMu.Unlock()
	all, _ := r.Snapshot()
	if len(all) == 0 {
		return
	}
	accountKey := usageStatusAccountKey(all)
	accountByKey := make(map[string]accounts.Account, len(all))
	for _, account := range all {
		key := string(accountProviderOrCodex(account)) + "\x00" + account.ID
		accountByKey[key] = account
	}
	rows := make([]durableUsageStatusSnapshotRow, 0, len(statuses))
	for _, status := range statuses {
		provider := accountProviderOrCodex(accounts.Account{Provider: status.Provider})
		key := string(provider) + "\x00" + status.ID
		account, ok := accountByKey[key]
		if !ok {
			continue
		}
		status.UsageFresh = false
		status.Error = ""
		rows = append(rows, durableUsageStatusSnapshotRow{
			ID:            status.ID,
			Provider:      provider,
			CredentialKey: usageStatusCredentialKey(account),
			Status:        status,
		})
	}
	if len(rows) == 0 {
		return
	}
	r.mu.RLock()
	diskGeneration := r.diskGeneration
	r.mu.RUnlock()
	snapshot := durableUsageStatusSnapshot{
		SchemaVersion:  usageStatusSnapshotSchemaVersion,
		SavedAt:        time.Now().UTC(),
		DiskGeneration: diskGeneration,
		AccountKey:     accountKey,
		Rows:           rows,
	}
	// A score refresh and a live status sweep can finish out of order. Keep
	// the newest persisted observation for each account when that happens.
	if existingBody, readErr := os.ReadFile(path); readErr == nil {
		var existing durableUsageStatusSnapshot
		if json.Unmarshal(existingBody, &existing) == nil &&
			existing.SchemaVersion == usageStatusSnapshotSchemaVersion &&
			existing.AccountKey == snapshot.AccountKey &&
			existing.DiskGeneration == snapshot.DiskGeneration {
			if existing.SavedAt.After(snapshot.SavedAt) {
				snapshot.SavedAt = existing.SavedAt
			}
			byKey := make(map[string]durableUsageStatusSnapshotRow, len(existing.Rows))
			for _, row := range existing.Rows {
				byKey[string(row.Provider)+"\x00"+row.ID] = row
			}
			seen := make(map[string]struct{}, len(snapshot.Rows))
			for i, row := range snapshot.Rows {
				key := string(row.Provider) + "\x00" + row.ID
				seen[key] = struct{}{}
				old, ok := byKey[key]
				if ok && old.Status.UsageFetchedAt.After(row.Status.UsageFetchedAt) {
					snapshot.Rows[i] = old
				}
			}
			for key, row := range byKey {
				if _, ok := seen[key]; !ok {
					snapshot.Rows = append(snapshot.Rows, row)
				}
			}
		}
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		return
	}
	storeDir := r.store.StoreDir()
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return
	}
	temp, err := os.CreateTemp(storeDir, ".usage-status-snapshot-*")
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
	if err := os.Rename(tempPath, path); err != nil {
		return
	}
	_ = syncAccountStateDir(storeDir)
	ok = true
}
