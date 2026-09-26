package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

const scoreSnapshotSchemaVersion = 1

type srUsageScoreSnapshot struct {
	SchemaVersion int                `json:"schema_version"`
	GenerationKey string             `json:"generation_key"`
	FetchedAt     time.Time          `json:"fetched_at"`
	Failed        bool               `json:"failed,omitempty"`
	Scores        []selectacct.Score `json:"scores"`
}

// srUsageScoreStore coordinates the one startup sweep shared by overlapping
// supervisor workers. The lock represents work in progress; only the atomic
// snapshot represents the shared outcome. A failed winner publishes that
// outcome before releasing the lock so overlapping workers do not repeat the
// same external sweep.
type srUsageScoreStore struct {
	path     string
	lockPath string
	now      func() time.Time
}

func newSRUsageScoreStore(stateDir string) *srUsageScoreStore {
	path := filepath.Join(stateDir, "codex-usage-scores.json")
	return &srUsageScoreStore{
		path:     path,
		lockPath: path + ".lock",
	}
}

func (s *srUsageScoreStore) clock() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

func codexScoreGenerationKey(_ uint64, candidates []accounts.Account) string {
	type entry struct {
		provider       string
		id             string
		authMode       string
		credentialHash string
	}
	entries := make([]entry, 0, len(candidates))
	for _, candidate := range candidates {
		credential := sha256.Sum256([]byte(candidate.CredentialIdentity()))
		provider := candidate.Provider
		if provider == "" {
			provider = accounts.ProviderCodex
		}
		entries = append(entries, entry{
			provider:       string(provider),
			id:             candidate.ID,
			authMode:       string(candidate.AuthMode),
			credentialHash: hex.EncodeToString(credential[:]),
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].provider != entries[j].provider {
			return entries[i].provider < entries[j].provider
		}
		return entries[i].id < entries[j].id
	})
	hash := sha256.New()
	writePart := func(value string) {
		_, _ = io.WriteString(hash, strconv.Itoa(len(value)))
		_, _ = io.WriteString(hash, ":")
		_, _ = io.WriteString(hash, value)
		_, _ = io.WriteString(hash, ";")
	}
	for _, item := range entries {
		writePart(item.provider)
		writePart(item.id)
		writePart(item.authMode)
		writePart(item.credentialHash)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (s *srUsageScoreStore) read(generationKey string, maxAge time.Duration) (srUsageScoreSnapshot, bool, error) {
	if s == nil || s.path == "" {
		return srUsageScoreSnapshot{}, false, nil
	}
	body, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return srUsageScoreSnapshot{}, false, nil
	}
	if err != nil {
		return srUsageScoreSnapshot{}, false, fmt.Errorf("read usage score snapshot: %w", err)
	}
	var snapshot srUsageScoreSnapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		return srUsageScoreSnapshot{}, false, fmt.Errorf("decode usage score snapshot: %w", err)
	}
	if snapshot.SchemaVersion != scoreSnapshotSchemaVersion || snapshot.GenerationKey != generationKey {
		return srUsageScoreSnapshot{}, false, nil
	}
	if maxAge > 0 && (snapshot.FetchedAt.IsZero() || s.clock().Sub(snapshot.FetchedAt) >= maxAge) {
		return srUsageScoreSnapshot{}, false, nil
	}
	return snapshot, true, nil
}

func (s *srUsageScoreStore) write(snapshot srUsageScoreSnapshot) (err error) {
	if s == nil || s.path == "" {
		return errors.New("usage score snapshot store is not configured")
	}
	if snapshot.SchemaVersion != scoreSnapshotSchemaVersion || snapshot.GenerationKey == "" {
		return errors.New("usage score snapshot is incomplete")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("prepare usage score snapshot dir: %w", err)
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode usage score snapshot: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(s.path), ".codex-usage-scores-*.tmp")
	if err != nil {
		return fmt.Errorf("create usage score snapshot: %w", err)
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("protect usage score snapshot: %w", err)
	}
	if _, err := temp.Write(body); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write usage score snapshot: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync usage score snapshot: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close usage score snapshot: %w", err)
	}
	if err := os.Rename(tempPath, s.path); err != nil {
		return fmt.Errorf("publish usage score snapshot: %w", err)
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return fmt.Errorf("open usage score snapshot dir: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync usage score snapshot dir: %w", err)
	}
	return nil
}

func (s *srUsageScoreStore) tryLock() (func(), bool, error) {
	if s == nil || s.lockPath == "" {
		return func() {}, true, nil
	}
	if err := os.MkdirAll(filepath.Dir(s.lockPath), 0o700); err != nil {
		return nil, false, fmt.Errorf("prepare usage score lock dir: %w", err)
	}
	lock, claimed, err := tryLockStartupScoreFile(s.lockPath)
	if err != nil {
		return nil, false, fmt.Errorf("claim usage score lock: %w", err)
	}
	if !claimed {
		return nil, false, nil
	}
	return func() { _ = lock.Close() }, true, nil
}

type startupScoreReadiness struct {
	required bool
	ready    atomic.Bool
}

func (r *startupScoreReadiness) check() error {
	if r == nil || !r.required || r.ready.Load() {
		return nil
	}
	return errors.New("fresh Codex usage scores are not loaded")
}

type startupScoreConfig struct {
	Interval         time.Duration
	AccountsSnapshot func() ([]accounts.Account, uint64)
	// RefreshAccounts reloads the durable credential set into this worker before
	// a newly claimed contender decides the prior winner published nothing. It
	// prevents a retiring worker's stale in-memory token generation from
	// repeating the winner's already-completed sweep.
	RefreshAccounts func() error
	SchedulerRef    *selectacct.SchedulerRef
	FetchScores     func(context.Context, []accounts.Account) ([]selectacct.Score, int)
	Store           *srUsageScoreStore
	PollInterval    time.Duration
	// RetryFailedSweep keeps the readiness gate alive when a provider is
	// temporarily unavailable. A failed snapshot still coordinates overlapping
	// workers, but the owner retries it after RetryInterval instead of treating
	// the first outage as permanent for this process.
	RetryFailedSweep bool
	RetryInterval    time.Duration
}

const defaultStartupScoreRetryInterval = 5 * time.Second

func startupScoreRetryInterval(cfg startupScoreConfig) time.Duration {
	if cfg.RetryInterval > 0 {
		return cfg.RetryInterval
	}
	return defaultStartupScoreRetryInterval
}

func ensureStartupScores(ctx context.Context, cfg startupScoreConfig) error {
	if cfg.Interval <= 0 {
		return nil
	}
	if cfg.AccountsSnapshot == nil || cfg.SchedulerRef == nil || cfg.FetchScores == nil || cfg.Store == nil {
		return errors.New("startup score coordinator is incomplete")
	}
	pollInterval := cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = 25 * time.Millisecond
	}
	retryFailedSnapshot := false
	for {
		all, generation := cfg.AccountsSnapshot()
		candidates := codexOAuthAccounts(all)
		if len(candidates) == 0 {
			return nil
		}
		generationKey := codexScoreGenerationKey(generation, candidates)
		if loaded, err := loadStartupScoreSnapshot(cfg, generation, generationKey, retryFailedSnapshot); err != nil {
			if errors.Is(err, errStartupScoreSweepFailed) && cfg.RetryFailedSweep {
				if err := waitStartupScorePoll(ctx, startupScoreRetryInterval(cfg)); err != nil {
					return err
				}
				retryFailedSnapshot = true
				continue
			}
			return err
		} else if loaded {
			return nil
		}

		release, claimed, err := cfg.Store.tryLock()
		if err != nil {
			return err
		}
		if !claimed {
			if err := waitStartupScorePoll(ctx, pollInterval); err != nil {
				return err
			}
			continue
		}

		resultErr := func() error {
			defer release()
			if cfg.RefreshAccounts != nil {
				if err := cfg.RefreshAccounts(); err != nil {
					return fmt.Errorf("reload startup score accounts: %w", err)
				}
				all, generation = cfg.AccountsSnapshot()
				candidates = codexOAuthAccounts(all)
				if len(candidates) == 0 {
					return nil
				}
				generationKey = codexScoreGenerationKey(generation, candidates)
			}
			if loaded, err := loadStartupScoreSnapshot(cfg, generation, generationKey, retryFailedSnapshot); err != nil || loaded {
				return err
			}
			scores, successful := cfg.FetchScores(ctx, candidates)
			if successful == 0 {
				currentAll, currentGeneration := cfg.AccountsSnapshot()
				currentCandidates := codexOAuthAccounts(currentAll)
				if codexScoreGenerationKey(currentGeneration, currentCandidates) != generationKey {
					return errStartupScoreGenerationChanged
				}
				if err := cfg.Store.write(srUsageScoreSnapshot{
					SchemaVersion: scoreSnapshotSchemaVersion,
					GenerationKey: generationKey,
					FetchedAt:     cfg.Store.clock().UTC(),
					Failed:        true,
				}); err != nil {
					return fmt.Errorf("record failed startup usage score sweep: %w", err)
				}
				return errStartupScoreSweepFailed
			}
			currentAll, currentGeneration := cfg.AccountsSnapshot()
			currentCandidates := codexOAuthAccounts(currentAll)
			if !sameCodexScoreAccounts(candidates, currentCandidates) {
				return errStartupScoreGenerationChanged
			}
			currentKey := codexScoreGenerationKey(currentGeneration, currentCandidates)
			snapshot := srUsageScoreSnapshot{
				SchemaVersion: scoreSnapshotSchemaVersion,
				GenerationKey: currentKey,
				FetchedAt:     cfg.Store.clock().UTC(),
				Scores:        scores,
			}
			if err := cfg.Store.write(snapshot); err != nil {
				return err
			}
			loaded, err := loadStartupScoreSnapshot(cfg, currentGeneration, currentKey, false)
			if err != nil {
				return err
			}
			if !loaded {
				return errStartupScoreGenerationChanged
			}
			return nil
		}()
		if resultErr == nil {
			return nil
		}
		if errors.Is(resultErr, errStartupScoreGenerationChanged) || errors.Is(resultErr, errStartupScorePublishConflict) {
			continue
		}
		if errors.Is(resultErr, errStartupScoreSweepFailed) && cfg.RetryFailedSweep {
			if err := waitStartupScorePoll(ctx, startupScoreRetryInterval(cfg)); err != nil {
				return err
			}
			retryFailedSnapshot = true
			continue
		}
		return fmt.Errorf("startup usage score sweep: %w", resultErr)
	}
}

var (
	errStartupScoreGenerationChanged = errors.New("startup score account generation changed")
	errStartupScorePublishConflict   = errors.New("startup score scheduler publication conflicted")
	errStartupScoreSweepFailed       = errors.New("no fresh OAuth usage scores available")
)

func loadStartupScoreSnapshot(cfg startupScoreConfig, generation uint64, generationKey string, ignoreFailed bool) (bool, error) {
	snapshot, ok, err := cfg.Store.read(generationKey, cfg.Interval)
	if err != nil || !ok {
		return false, err
	}
	if snapshot.Failed {
		if !ignoreFailed {
			return false, errStartupScoreSweepFailed
		}
		return false, nil
	}
	for attempt := 0; attempt < srAutoSwitchPublishAttempts; attempt++ {
		revision := cfg.SchedulerRef.ScoreRevision()
		if _, published := cfg.SchedulerRef.MergeScoresForAccountGenerationAtScoreRevision(snapshot.Scores, generation, revision); published {
			return true, nil
		}
		all, currentGeneration := cfg.AccountsSnapshot()
		if codexScoreGenerationKey(currentGeneration, codexOAuthAccounts(all)) != generationKey {
			return false, errStartupScoreGenerationChanged
		}
	}
	return false, errStartupScorePublishConflict
}

func sameCodexScoreAccounts(left, right []accounts.Account) bool {
	if len(left) != len(right) {
		return false
	}
	leftIDs := make([]string, len(left))
	rightIDs := make([]string, len(right))
	for i := range left {
		leftIDs[i] = string(left[i].Provider) + "\x00" + left[i].ID
	}
	for i := range right {
		rightIDs[i] = string(right[i].Provider) + "\x00" + right[i].ID
	}
	sort.Strings(leftIDs)
	sort.Strings(rightIDs)
	for i := range leftIDs {
		if leftIDs[i] != rightIDs[i] {
			return false
		}
	}
	return true
}

func waitStartupScorePoll(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
