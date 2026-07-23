package selectacct

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

const modelIncompatibilitySchemaVersion = 1

const ModelIncompatibilityCode = "codex_chatgpt_model_unsupported"

// ModelIncompatibility is durable evidence that one subscription account
// cannot serve one model. It is deliberately model-scoped so the account stays
// available for models it supports.
type ModelIncompatibility struct {
	Provider   accounts.Provider `json:"provider"`
	AccountID  string            `json:"account_id"`
	Model      string            `json:"model"`
	Code       string            `json:"code"`
	Message    string            `json:"message"`
	ObservedAt time.Time         `json:"observed_at"`
}

type modelIncompatibilityFile struct {
	Version int                  `json:"version"`
	Issue   ModelIncompatibility `json:"issue"`
}

// ModelIncompatibilityStore writes one atomic file per account/model pair.
// Separate files avoid lost updates while old and new supervised workers
// overlap during a zero-disruption deploy.
type ModelIncompatibilityStore struct {
	Dir string
}

// Get reads one deterministic account/model record directly from the durable
// store. Workers keep positive records in memory, but a cache miss must consult
// disk so a still-running generation observes evidence written by a peer
// immediately instead of waiting for its next restart.
func (s ModelIncompatibilityStore) Get(provider accounts.Provider, accountID, model string) (ModelIncompatibility, bool, error) {
	if strings.TrimSpace(s.Dir) == "" {
		return ModelIncompatibility{}, false, nil
	}
	query, err := normalizeModelIncompatibility(ModelIncompatibility{
		Provider:  provider,
		AccountID: accountID,
		Model:     model,
	})
	if err != nil {
		return ModelIncompatibility{}, false, err
	}
	path := filepath.Join(s.Dir, modelIncompatibilityFilename(query))
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ModelIncompatibility{}, false, nil
	}
	if err != nil {
		return ModelIncompatibility{}, false, fmt.Errorf("read model incompatibility %s: %w", path, err)
	}
	var file modelIncompatibilityFile
	if err := json.Unmarshal(body, &file); err != nil {
		return ModelIncompatibility{}, false, fmt.Errorf("read model incompatibility %s: %w", path, err)
	}
	if file.Version != modelIncompatibilitySchemaVersion {
		return ModelIncompatibility{}, false, fmt.Errorf("read model incompatibility %s: unsupported version %d", path, file.Version)
	}
	issue, err := normalizeModelIncompatibility(file.Issue)
	if err != nil {
		return ModelIncompatibility{}, false, fmt.Errorf("read model incompatibility %s: %w", path, err)
	}
	if modelIncompatibilityKey(issue.Provider, issue.AccountID, issue.Model) != modelIncompatibilityKey(query.Provider, query.AccountID, query.Model) {
		return ModelIncompatibility{}, false, fmt.Errorf("read model incompatibility %s: record identity mismatch", path)
	}
	return issue, true, nil
}

func (s ModelIncompatibilityStore) Load() ([]ModelIncompatibility, error) {
	if strings.TrimSpace(s.Dir) == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(s.Dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]ModelIncompatibility)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(s.Dir, entry.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var file modelIncompatibilityFile
		if err := json.Unmarshal(body, &file); err != nil {
			return nil, fmt.Errorf("read model incompatibility %s: %w", path, err)
		}
		if file.Version != modelIncompatibilitySchemaVersion {
			return nil, fmt.Errorf("read model incompatibility %s: unsupported version %d", path, file.Version)
		}
		issue, err := normalizeModelIncompatibility(file.Issue)
		if err != nil {
			return nil, fmt.Errorf("read model incompatibility %s: %w", path, err)
		}
		key := modelIncompatibilityKey(issue.Provider, issue.AccountID, issue.Model)
		if previous, ok := byKey[key]; !ok || issue.ObservedAt.After(previous.ObservedAt) {
			byKey[key] = issue
		}
	}
	issues := make([]ModelIncompatibility, 0, len(byKey))
	for _, issue := range byKey {
		issues = append(issues, issue)
	}
	sortModelIncompatibilities(issues)
	return issues, nil
}

func (s ModelIncompatibilityStore) Put(issue ModelIncompatibility) error {
	if strings.TrimSpace(s.Dir) == "" {
		return nil
	}
	issue, err := normalizeModelIncompatibility(issue)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(modelIncompatibilityFile{
		Version: modelIncompatibilitySchemaVersion,
		Issue:   issue,
	}, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tmp, err := os.CreateTemp(s.Dir, ".model-incompatibility-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, filepath.Join(s.Dir, modelIncompatibilityFilename(issue)))
}

func normalizeModelIncompatibility(issue ModelIncompatibility) (ModelIncompatibility, error) {
	if issue.Provider == "" {
		issue.Provider = accounts.ProviderCodex
	}
	issue.AccountID = strings.TrimSpace(issue.AccountID)
	issue.Model = strings.TrimSpace(issue.Model)
	issue.Code = strings.TrimSpace(issue.Code)
	issue.Message = strings.Join(strings.Fields(issue.Message), " ")
	if issue.Code == "" {
		issue.Code = ModelIncompatibilityCode
	}
	if issue.ObservedAt.IsZero() {
		issue.ObservedAt = time.Now().UTC()
	} else {
		issue.ObservedAt = issue.ObservedAt.UTC()
	}
	if issue.AccountID == "" {
		return ModelIncompatibility{}, fmt.Errorf("account id is required")
	}
	if ModelKey(issue.Model) == "" {
		return ModelIncompatibility{}, fmt.Errorf("model is required")
	}
	const maxMessageRunes = 512
	if utf8.RuneCountInString(issue.Message) > maxMessageRunes {
		issue.Message = string([]rune(issue.Message)[:maxMessageRunes])
	}
	return issue, nil
}

func modelIncompatibilityKey(provider accounts.Provider, accountID, model string) string {
	return poolScopedExhaustionKey(provider, strings.TrimSpace(accountID), model)
}

func modelIncompatibilityFilename(issue ModelIncompatibility) string {
	digest := sha256.Sum256([]byte(modelIncompatibilityKey(issue.Provider, issue.AccountID, issue.Model)))
	return hex.EncodeToString(digest[:]) + ".json"
}

func sortModelIncompatibilities(issues []ModelIncompatibility) {
	sort.Slice(issues, func(i, j int) bool {
		left, right := issues[i], issues[j]
		if left.Provider != right.Provider {
			return left.Provider < right.Provider
		}
		if left.AccountID != right.AccountID {
			return left.AccountID < right.AccountID
		}
		return ModelKey(left.Model) < ModelKey(right.Model)
	})
}
