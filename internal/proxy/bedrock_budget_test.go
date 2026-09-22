package proxy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBedrockBudgetGuardReservesAndPersists(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "budget.json")
	costs := filepath.Join(dir, "cost.jsonl")
	g, err := newBedrockBudgetGuard(state, costs, 1)
	if err != nil {
		t.Fatal(err)
	}
	r, err := g.reserve([]byte(`{"max_tokens":1000,"messages":[]}`), bedrockFableModelID, 1)
	if err != nil {
		t.Fatal(err)
	}
	r.settle(0, 1)
	if g.spentUSD <= 0 {
		t.Fatalf("spent = %v, want positive", g.spentUSD)
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("state was not persisted: %v", err)
	}
	g2, err := newBedrockBudgetGuard(state, costs, 1)
	if err != nil {
		t.Fatal(err)
	}
	if g2.spentUSD != g.spentUSD {
		t.Fatalf("reloaded spent = %v, want %v", g2.spentUSD, g.spentUSD)
	}
}

func TestBedrockBudgetGuardFailsClosed(t *testing.T) {
	g := &bedrockBudgetGuard{limitUSD: 1}
	r, err := g.reserve([]byte(`{"max_tokens":1}`), bedrockFableModelID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.reserve(make([]byte, 1<<20), bedrockFableModelID, 1); err == nil {
		t.Fatal("second reservation crossed the cap")
	}
	r.settle(0, 3)
	if g.spentUSD > g.limitUSD {
		t.Fatalf("spent %v crossed limit %v", g.spentUSD, g.limitUSD)
	}
}
