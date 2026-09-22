package proxy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A budget is one lifetime allowance, shared by all regions and overlapping
// workers on this host. Every physical upstream attempt first commits its full
// reservation to disk. Crashes and uncertain outcomes never refund money.
// It does not measure AWS charges incurred outside this gateway.
type bedrockBudgetGuard struct {
	path, account string
	limit         int64
}

type bedrockBudgetPolicy struct {
	MaxInput  int64 `json:"max_input_tokens"`
	MaxOutput int64 `json:"max_output_tokens"`
	// InputMicros must cover the MOST expensive input category (including 1h
	// cache writes and regional premiums). All inputs settle at that rate, so
	// cache hits deliberately consume more allowance than their AWS bill.
	InputMicros  int64     `json:"input_microusd_per_token"`
	OutputMicros int64     `json:"output_microusd_per_token"`
	Expires      time.Time `json:"pricing_valid_until"`
}
type bedrockBudgetState struct {
	Version  int                            `json:"version"`
	Account  string                         `json:"account_id"`
	Limit    int64                          `json:"limit_microusd"`
	Spent    int64                          `json:"spent_microusd"`
	Pending  map[string]int64               `json:"pending_microusd"`
	Policies map[string]bedrockBudgetPolicy `json:"models"`
	Blocked  bool                           `json:"blocked"`
}
type bedrockBudgetReservation struct {
	guard  *bedrockBudgetGuard
	id     string
	amount int64
	policy bedrockBudgetPolicy
	once   sync.Once
}

func defaultBedrockBudgetPolicies() map[string]bedrockBudgetPolicy {
	validUntil := time.Now().UTC().Add(30 * 24 * time.Hour)
	policy := func() bedrockBudgetPolicy {
		return bedrockBudgetPolicy{MaxInput: 1_000_000, MaxOutput: 128_000, InputMicros: 20, OutputMicros: 50, Expires: validUntil}
	}
	return map[string]bedrockBudgetPolicy{bedrockFableModelID: policy(), bedrockOpus55ModelID: policy()}
}

// InitializeBedrockBudgetState is an explicit one-time provisioning step. The
// serving process refuses to create a missing state file, because silently
// treating deletion as a fresh lifetime allowance would defeat the cap.
func InitializeBedrockBudgetState(path, account string, limitUSD float64) error {
	if !filepath.IsAbs(path) || len(account) != 12 || strings.Trim(account, "0123456789") != "" {
		return errors.New("absolute budget path and 12-digit AWS account required")
	}
	if math.IsNaN(limitUSD) || math.IsInf(limitUSD, 0) || limitUSD <= 0 || limitUSD > 1e9 {
		return errors.New("invalid Bedrock allowance")
	}
	limit := int64(math.Floor(limitUSD * 1e6))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	state := bedrockBudgetState{
		Version:  2,
		Account:  account,
		Limit:    limit,
		Pending:  map[string]int64{},
		Policies: defaultBedrockBudgetPolicies(),
	}
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create budget state: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(body); err != nil {
		return err
	}
	return f.Sync()
}

func NewBedrockBudgetGuard(path, account string, limitUSD float64) (*bedrockBudgetGuard, error) {
	if limitUSD == 0 {
		return nil, nil
	}
	if math.IsNaN(limitUSD) || math.IsInf(limitUSD, 0) || limitUSD < 0 || limitUSD > 1e9 {
		return nil, errors.New("invalid Bedrock allowance")
	}
	if !filepath.IsAbs(path) || len(account) != 12 || strings.Trim(account, "0123456789") != "" {
		return nil, errors.New("absolute budget path and 12-digit AWS account required")
	}
	g := &bedrockBudgetGuard{path: path, account: account, limit: int64(math.Floor(limitUSD * 1e6))}
	_, err := g.snapshot()
	return g, err
}

func (g *bedrockBudgetGuard) snapshot() (bedrockBudgetState, error) {
	lock, err := lockBedrockBudget(g.path)
	if err != nil {
		return bedrockBudgetState{}, err
	}
	defer lock.Close()
	return g.readLocked()
}
func (g *bedrockBudgetGuard) readLocked() (bedrockBudgetState, error) {
	var s bedrockBudgetState
	f, err := os.Open(g.path)
	if err != nil {
		return s, err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, 16<<20))
	if err != nil {
		return s, err
	}
	if err = rejectDuplicateJSON(body); err != nil {
		return s, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&s); err != nil {
		return s, err
	}
	if s.Version != 2 || s.Account != g.account || s.Limit != g.limit || s.Limit <= 0 || s.Spent < 0 || s.Pending == nil || len(s.Policies) == 0 {
		return s, errors.New("budget state identity, version, or balance invalid")
	}
	total := s.Spent
	for _, v := range s.Pending {
		if v <= 0 || v > s.Limit || total > s.Limit-v {
			return s, errors.New("budget reservations exceed allowance")
		}
		total += v
	}
	if total > s.Limit {
		return s, errors.New("budget balance exceeds allowance")
	}
	return s, nil
}
func (g *bedrockBudgetGuard) writeLocked(s bedrockBudgetState) error {
	// Refuse to resurrect deleted state. Startup and request paths NEVER create
	// a fresh budget or infer an opening balance from incomplete telemetry.
	if _, err := os.Stat(g.path); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	dir := filepath.Dir(g.path)
	f, err := os.CreateTemp(dir, ".bedrock-budget-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), g.path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (g *bedrockBudgetGuard) reserve(method, path, query string, headers http.Header, body []byte) (*bedrockBudgetReservation, error) {
	if g == nil {
		return nil, nil
	}
	parts := strings.Split(path, "/")
	if method != "POST" || query != "" || len(parts) != 4 || parts[0] != "" || parts[1] != "model" || (parts[3] != "invoke" && parts[3] != "invoke-with-response-stream") {
		return nil, errors.New("unpriced Bedrock operation")
	}
	if err := rejectDuplicateJSON(body); err != nil {
		return nil, err
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	allowed := map[string]bool{"anthropic_version": true, "messages": true, "system": true, "tools": true, "tool_choice": true, "max_tokens": true, "thinking": true, "output_config": true, "metadata": true, "stop_sequences": true, "temperature": true, "top_p": true, "top_k": true, "service_tier": true}
	for k := range payload {
		if !allowed[k] {
			return nil, fmt.Errorf("unpriced Bedrock field %s", k)
		}
	}
	if raw, ok := payload["service_tier"]; ok && string(raw) != `"default"` {
		return nil, errors.New("unpriced service tier")
	}
	for k := range headers {
		if strings.HasPrefix(strings.ToLower(k), "x-amzn-bedrock-") {
			return nil, errors.New("unpriced Bedrock feature header")
		}
	}
	var output int64
	if err := json.Unmarshal(payload["max_tokens"], &output); err != nil || output <= 0 {
		return nil, errors.New("positive integer max_tokens required")
	}
	var toolList []struct {
		Type string `json:"type"`
	}
	if raw, ok := payload["tools"]; ok {
		if err := json.Unmarshal(raw, &toolList); err != nil {
			return nil, err
		}
	}
	for _, tool := range toolList {
		if tool.Type != "" && tool.Type != "custom" && !strings.HasPrefix(tool.Type, "bash_") && !strings.HasPrefix(tool.Type, "text_editor_") && !strings.HasPrefix(tool.Type, "computer_") {
			return nil, errors.New("unpriced server tool")
		}
	}
	lock, err := lockBedrockBudget(g.path)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	state, err := g.readLocked()
	if err != nil {
		return nil, err
	}
	if state.Blocked {
		return nil, errors.New("Bedrock budget blocked")
	}
	p, ok := state.Policies[parts[2]]
	if !ok || !time.Now().Before(p.Expires) || p.MaxInput <= 0 || p.MaxInput > 10000000 || p.MaxOutput <= 0 || p.MaxOutput > 1000000 || p.InputMicros <= 0 || p.InputMicros > 1000000 || p.OutputMicros <= 0 || p.OutputMicros > 1000000 || output > p.MaxOutput {
		return nil, errors.New("missing, expired, or invalid Bedrock pricing policy")
	}
	// Full context window, never a byte/token guess. Includes image tokens and
	// hidden prompt overhead. Caller max_tokens bounds output, including thinking.
	amount := p.MaxInput*p.InputMicros + output*p.OutputMicros
	total := state.Spent
	for _, v := range state.Pending {
		total += v
	}
	if amount > state.Limit-total {
		return nil, errors.New("Bedrock lifetime allowance exhausted")
	}
	var idBytes [16]byte
	if _, err = rand.Read(idBytes[:]); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(idBytes[:])
	state.Pending[id] = amount
	if err = g.writeLocked(state); err != nil {
		return nil, err
	}
	p.MaxOutput = output
	return &bedrockBudgetReservation{guard: g, id: id, amount: amount, policy: p}, nil
}
func (r *bedrockBudgetReservation) settle(input, output int64) {
	if r == nil {
		return
	}
	r.once.Do(func() {
		g := r.guard
		lock, err := lockBedrockBudget(g.path)
		if err != nil {
			return
		}
		defer lock.Close()
		state, err := g.readLocked()
		if err != nil {
			return
		}
		if state.Pending[r.id] != r.amount {
			return
		}
		if input < 0 || output < 0 || input > r.policy.MaxInput || output > r.policy.MaxOutput {
			state.Blocked = true
			_ = g.writeLocked(state)
			return
		}
		cost := input*r.policy.InputMicros + output*r.policy.OutputMicros
		delete(state.Pending, r.id)
		state.Spent += cost
		// Failed settlement leaves the larger persisted reservation in force.
		_ = g.writeLocked(state)
	})
}

// Reject duplicate keys (including nested ones) and trailing JSON. Accounting
// and the model must never interpret different max_tokens or feature settings.
func rejectDuplicateJSON(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 128 {
			return errors.New("JSON too deep")
		}
		t, e := d.Token()
		if e != nil {
			return e
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return e
				}
				s, ok := k.(string)
				if !ok || seen[s] {
					return errors.New("duplicate JSON key")
				}
				seen[s] = true
				if e = value(depth + 1); e != nil {
					return e
				}
			}
			_, e = d.Token()
			return e
		case '[':
			for d.More() {
				if e = value(depth + 1); e != nil {
					return e
				}
			}
			_, e = d.Token()
			return e
		default:
			return errors.New("invalid JSON delimiter")
		}
	}
	if e := value(0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
