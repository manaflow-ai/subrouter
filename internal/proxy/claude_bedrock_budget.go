package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ClaudeBedrockBudget gates the Claude Bedrock fallback on a spend ledger the
// router does not own. The Bedrock account can be shared with another service
// that enforces the account's cap (Chatmux's David route), so the router reads
// that ledger instead of keeping a second one: a second ledger would let the
// two routes together spend past the cap. The ledger counts the router's
// Bedrock calls itself from AWS metrics.
type ClaudeBedrockBudget struct {
	// URL answers GET with JSON carrying remainingUsd.
	URL string
	// Token returns the bearer token for each ledger read.
	Token func() (string, error)
	// ReserveUSD is the remaining balance below which the fallback stops.
	ReserveUSD float64
	// RefreshEvery is how long one ledger read is reused.
	RefreshEvery time.Duration
	// StaleAfter is how long the last good read still admits calls when the
	// ledger cannot be read. Past it the fallback fails closed.
	StaleAfter time.Duration
	Client     *http.Client
	now        func() time.Time

	mu sync.Mutex
	// recent is the router's own Bedrock spend the ledger may not show yet.
	recent      []claudeBedrockSpend
	remaining   float64
	readAt      time.Time
	attemptedAt time.Time
	lastErr     error
}

type claudeBedrockSpend struct {
	at  time.Time
	usd float64
}

const (
	// claudeBedrockLedgerLag is how long a Bedrock call can be missing from
	// the ledger. Chatmux prices AWS's counts by whole hour, ten minutes after
	// the hour ends, so a call can wait up to 70 minutes to show there.
	claudeBedrockLedgerLag     = 75 * time.Minute
	claudeBedrockBudgetRefresh = time.Minute
	claudeBedrockBudgetStale   = 5 * time.Minute
)

func (b *ClaudeBedrockBudget) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// Allow reports whether one more Bedrock call fits under the cap, and why not
// when it does not.
func (b *ClaudeBedrockBudget) Allow(ctx context.Context) (bool, string) {
	if b == nil {
		return true, ""
	}
	refresh := b.RefreshEvery
	if refresh <= 0 {
		refresh = claudeBedrockBudgetRefresh
	}
	stale := b.StaleAfter
	if stale <= 0 {
		stale = claudeBedrockBudgetStale
	}
	b.mu.Lock()
	now := b.clock()
	if now.Sub(b.attemptedAt) >= refresh {
		b.attemptedAt = now
		// Only one caller refreshes; the rest use the last read meanwhile,
		// and are refused until a first read has succeeded.
		b.mu.Unlock()
		remaining, err := b.read(ctx)
		b.mu.Lock()
		if err == nil {
			b.remaining, b.readAt, b.lastErr = remaining, b.clock(), nil
		} else {
			b.lastErr = err
		}
		now = b.clock()
	}
	defer b.mu.Unlock()
	if b.readAt.IsZero() || now.Sub(b.readAt) > stale {
		return false, fmt.Sprintf("budget ledger unreadable: %v", b.lastErr)
	}
	// The router's spend inside the lag window counts against the balance
	// too. Spend the ledger already shows is counted twice for a while,
	// which errs toward stopping early.
	remaining := b.remaining - b.recentSpend(now)
	if remaining < b.ReserveUSD {
		return false, fmt.Sprintf("budget remaining $%.2f (ledger $%.2f less recent router spend) is below the $%.2f reserve", remaining, b.remaining, b.ReserveUSD)
	}
	return true, ""
}

func (b *ClaudeBedrockBudget) read(ctx context.Context) (float64, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.URL, nil)
	if err != nil {
		return 0, err
	}
	if b.Token != nil {
		token, err := b.Token()
		if err != nil {
			return 0, err
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
	client := b.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("budget ledger answered %d", resp.StatusCode)
	}
	var payload struct {
		RemainingUSD *float64 `json:"remainingUsd"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0, err
	}
	if payload.RemainingUSD == nil {
		return 0, fmt.Errorf("budget ledger answer has no remainingUsd")
	}
	return *payload.RemainingUSD, nil
}

// Record adds one Bedrock call's estimated cost to the recent spend.
func (b *ClaudeBedrockBudget) Record(usd float64) {
	if b == nil || usd <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recent = append(b.recent, claudeBedrockSpend{at: b.clock(), usd: usd})
}

// recentSpend sums the spend inside the lag window and drops older entries.
// The caller holds mu.
func (b *ClaudeBedrockBudget) recentSpend(now time.Time) float64 {
	keep := b.recent[:0]
	total := 0.0
	for _, spend := range b.recent {
		if now.Sub(spend.at) <= claudeBedrockLedgerLag {
			keep = append(keep, spend)
			total += spend.usd
		}
	}
	b.recent = keep
	return total
}
