package main

import (
	"errors"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func claudePickRow(email string, headroom, short float64) srUsageRow {
	return srUsageRow{
		email:    email,
		provider: accounts.ProviderClaude,
		authMode: accounts.AuthModeOAuth,
		planType: "claude",
		score:    selectacct.Score{AccountID: email, Headroom: headroom, ShortHeadroom: short},
	}
}

func TestBestClaudeUsageRowPicksMostHeadroom(t *testing.T) {
	rows := []srUsageRow{
		claudePickRow("half@example.com", 0.5, 0.5),
		claudePickRow("fresh@example.com", 0.99, 0.97),
		claudePickRow("weekly-tight@example.com", 0.1, 0.9),
	}
	target := bestClaudeUsageRow(rows)
	if target == nil || target.email != "fresh@example.com" {
		t.Fatalf("target = %+v, want fresh@example.com", target)
	}
}

func TestBestClaudeUsageRowSkipsCookedErroredExhausted(t *testing.T) {
	cooked := claudePickRow("cooked@example.com", 0.9, 0.9)
	cooked.tempCooked = true
	errored := claudePickRow("error@example.com", 1, 1)
	errored.err = errors.New("usage fetch failed: 429")
	exhausted := claudePickRow("exhausted@example.com", 0.8, 0)
	ok := claudePickRow("ok@example.com", 0.3, 0.4)
	target := bestClaudeUsageRow([]srUsageRow{cooked, errored, exhausted, ok})
	if target == nil || target.email != "ok@example.com" {
		t.Fatalf("target = %+v, want ok@example.com", target)
	}
}

func TestBestClaudeUsageRowNilWhenAllUnusable(t *testing.T) {
	cooked := claudePickRow("cooked@example.com", 0, 0)
	if target := bestClaudeUsageRow([]srUsageRow{cooked}); target != nil {
		t.Fatalf("target = %+v, want nil", target)
	}
}
