package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// bedrockBudgetGuard is a process-wide, persistent spend guard. It is an
// intentionally conservative second line of defense behind AWS Budgets: a
// request reserves its worst-case estimated cost before it is signed, and the
// reservation is committed or released when the request finishes.
type bedrockBudgetGuard struct {
	mu       sync.Mutex
	limitUSD float64
	spentUSD float64
	reserved float64
	path     string
	faulted  bool
}

type bedrockBudgetState struct {
	LimitUSD float64 `json:"limit_usd"`
	SpentUSD float64 `json:"spent_usd"`
	Updated  string  `json:"updated_at"`
}

type bedrockBudgetReservation struct {
	guard    *bedrockBudgetGuard
	amount   float64
	unit     float64
	maxTries int
	once     sync.Once
}

func newBedrockBudgetGuard(path, costLogPath string, limitUSD float64) (*bedrockBudgetGuard, error) {
	if limitUSD <= 0 {
		return nil, nil
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("bedrock budget state path is required when a budget is configured")
	}
	g := &bedrockBudgetGuard{limitUSD: limitUSD, path: path}
	if body, err := os.ReadFile(path); err == nil {
		var state bedrockBudgetState
		if err := json.Unmarshal(body, &state); err != nil {
			return nil, fmt.Errorf("read bedrock budget state: %w", err)
		}
		if state.LimitUSD > 0 && state.LimitUSD != limitUSD {
			return nil, fmt.Errorf("bedrock budget limit changed from %.2f to %.2f; refuse to reset spend", state.LimitUSD, limitUSD)
		}
		g.spentUSD = state.SpentUSD
		return g, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read bedrock budget state: %w", err)
	}
	// A first enablement must account for historical spend already recorded by
	// the router. Unknown or malformed records are ignored because the AWS
	// account-level budget remains authoritative for those requests.
	if costLogPath != "" {
		if body, err := os.ReadFile(costLogPath); err == nil {
			for _, line := range strings.Split(string(body), "\n") {
				var record struct {
					CostUSD float64 `json:"cost_usd_estimate"`
				}
				if json.Unmarshal([]byte(line), &record) == nil && record.CostUSD > 0 {
					g.spentUSD += record.CostUSD
				}
			}
		}
	}
	if err := g.persistLocked(); err != nil {
		return nil, err
	}
	return g, nil
}

// NewBedrockBudgetGuard constructs the persistent local spend guard used by
// the command package. The concrete type stays private so callers can only
// configure it through BedrockConfig and cannot mutate its accounting.
func NewBedrockBudgetGuard(path, costLogPath string, limitUSD float64) (*bedrockBudgetGuard, error) {
	return newBedrockBudgetGuard(path, costLogPath, limitUSD)
}

func (g *bedrockBudgetGuard) reserve(body []byte, model string, maxTries int) (*bedrockBudgetReservation, error) {
	if g == nil {
		return nil, nil
	}
	if maxTries < 1 {
		maxTries = 1
	}
	unit := estimateBedrockRequestUSD(body, model)
	amount := unit * float64(maxTries)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.faulted {
		return nil, errors.New("bedrock budget state is not writable; refusing new requests")
	}
	if g.spentUSD+g.reserved+amount > g.limitUSD {
		return nil, fmt.Errorf("bedrock budget exhausted: spent %.2f, reserved %.2f, request reserve %.2f, limit %.2f", g.spentUSD, g.reserved, amount, g.limitUSD)
	}
	g.reserved += amount
	return &bedrockBudgetReservation{guard: g, amount: amount, unit: unit, maxTries: maxTries}, nil
}

func (r *bedrockBudgetReservation) settle(actualUSD float64, tries int) {
	if r == nil || r.guard == nil {
		return
	}
	r.once.Do(func() {
		if tries < 1 {
			tries = 1
		}
		charge := actualUSD
		if charge <= 0 {
			charge = r.amount
		} else if tries > 1 {
			// A retry may have been billed before the successful response. Add a
			// worst-case estimate for every earlier attempt.
			charge += r.unit * float64(tries-1)
		}
		if charge > r.amount {
			charge = r.amount
		}
		g := r.guard
		g.mu.Lock()
		g.reserved -= r.amount
		g.spentUSD += charge
		if err := g.persistLocked(); err != nil {
			// A memory-only balance cannot enforce a cap across a restart. Stop
			// accepting paid work until the state file is writable again.
			g.faulted = true
		}
		g.mu.Unlock()
	})
}

func (g *bedrockBudgetGuard) persistLocked() error {
	if g == nil || g.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(g.path), 0o700); err != nil {
		return fmt.Errorf("create bedrock budget state directory: %w", err)
	}
	body, err := json.Marshal(bedrockBudgetState{LimitUSD: g.limitUSD, SpentUSD: g.spentUSD, Updated: time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(g.path), ".bedrock-budget-*.tmp")
	if err != nil {
		return fmt.Errorf("create bedrock budget state temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
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
	if err := os.Rename(tmpName, g.path); err != nil {
		return fmt.Errorf("replace bedrock budget state: %w", err)
	}
	return nil
}

// The byte count is deliberately treated as a token upper bound. It greatly
// overestimates normal JSON tokenization, but makes the gate fail closed for
// unusually large prompts and multimodal payloads. Fable's cache-write rate is
// the largest applicable rate, so it is included for every input byte.
func estimateBedrockRequestUSD(body []byte, model string) float64 {
	p := bedrockPriceFor(model)
	if p.input == 0 && p.output == 0 {
		return 0
	}
	maxTokens := 65536
	var payload struct {
		MaxTokens int `json:"max_tokens"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.MaxTokens > 0 {
		maxTokens = payload.MaxTokens
	}
	input := float64(len(body))
	return (input*p.input + input*p.cacheWrite1h + float64(maxTokens)*p.output) / 1_000_000
}
