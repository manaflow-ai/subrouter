package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func budgetFixture(t *testing.T, limit int64) *bedrockBudgetGuard {
	t.Helper()
	path := filepath.Join(t.TempDir(), "budget.json")
	s := bedrockBudgetState{Version: 2, Account: "123456789012", Limit: limit, Pending: map[string]int64{}, Policies: map[string]bedrockBudgetPolicy{bedrockFableModelID: {MaxInput: 1000000, MaxOutput: 128000, InputMicros: 20, OutputMicros: 50, Expires: time.Now().Add(time.Hour)}}}
	b, _ := json.Marshal(s)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	g, err := NewBedrockBudgetGuard(path, s.Account, float64(limit)/1e6)
	if err != nil {
		t.Fatal(err)
	}
	return g
}
func reserveFixture(g *bedrockBudgetGuard) (*bedrockBudgetReservation, error) {
	return g.reserve("POST", "/model/"+bedrockFableModelID+"/invoke", "", nil, []byte(`{"max_tokens":100}`))
}
func TestBedrockBudgetDurableReservation(t *testing.T) {
	g := budgetFixture(t, 30000000)
	if _, err := reserveFixture(g); err != nil {
		t.Fatal(err)
	}
	g2, err := NewBedrockBudgetGuard(g.path, g.account, 30)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reserveFixture(g2); err == nil {
		t.Fatal("pending reservation was lost")
	}
}
func TestBedrockBudgetConcurrentWorkers(t *testing.T) {
	g := budgetFixture(t, 60000000)
	var wg sync.WaitGroup
	var ok int
	var mu sync.Mutex
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			x, e := NewBedrockBudgetGuard(g.path, g.account, 60)
			if e == nil {
				if _, e = reserveFixture(x); e == nil {
					mu.Lock()
					ok++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if ok != 2 {
		t.Fatalf("admitted %d reservations, want 2", ok)
	}
}
func TestBedrockBudgetRefusesBadState(t *testing.T) {
	g := budgetFixture(t, 30000000)
	os.WriteFile(g.path, []byte(`{"version":2,"account_id":"123456789012","limit_microusd":30000000,"spent_microusd":0,"pending_microusd":{},"models":{}}`), 0600)
	if _, err := reserveFixture(g); err == nil {
		t.Fatal("bad pricing policy admitted request")
	}
	if _, err := NewBedrockBudgetGuard(g.path, g.account, 31); err == nil {
		t.Fatal("limit change accepted")
	}
}
func TestBedrockBudgetRejectsUnpricedRequest(t *testing.T) {
	g := budgetFixture(t, 30000000)
	for _, path := range []string{"/async-invoke", "/model/unknown/invoke", "/model/" + bedrockFableModelID + "/converse"} {
		if _, err := g.reserve("POST", path, "", nil, []byte(`{"max_tokens":1}`)); err == nil {
			t.Errorf("accepted %s", path)
		}
	}
}
