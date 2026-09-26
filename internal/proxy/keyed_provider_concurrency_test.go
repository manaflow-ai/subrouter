package proxy

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

type keyedProbeConcurrencyTransport struct {
	mu      sync.Mutex
	current int
	maximum int
	calls   int
}

func (t *keyedProbeConcurrencyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.current++
	t.calls++
	if t.current > t.maximum {
		t.maximum = t.current
	}
	t.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	t.mu.Lock()
	t.current--
	t.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       http.NoBody,
		Request:    request,
	}, nil
}

func (t *keyedProbeConcurrencyTransport) counts() (int, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls, t.maximum
}

func TestUsageStatusesLiveBoundsKeyedProviderProbeConcurrency(t *testing.T) {
	store := accounts.CodexStore{Dir: t.TempDir()}
	for i := 0; i < 8; i++ {
		if _, _, err := store.AddAPIKeyForProvider(
			"key-"+string(rune('a'+i)),
			"secret-"+string(rune('a'+i)),
			accounts.ProviderOpenRouter,
		); err != nil {
			t.Fatal(err)
		}
	}
	transport := &keyedProbeConcurrencyTransport{}
	ref := NewAccountRef(store, nil, &http.Client{Transport: transport})
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	ref.apiKeyUpstreams = map[accounts.Provider]string{
		accounts.ProviderOpenRouter: "https://openrouter.test/api/v1",
	}

	statuses := ref.usageStatusesLive(context.Background())
	if len(statuses) != 8 {
		t.Fatalf("statuses = %d, want 8", len(statuses))
	}
	calls, maximum := transport.counts()
	// OpenRouter performs one key probe and one optional account-credits probe
	// per account; both remain bounded by the same concurrency gate.
	if calls != 16 || maximum != accountFetchConcurrency {
		t.Fatalf("key/credits probe calls/max = %d/%d, want 16/%d", calls, maximum, accountFetchConcurrency)
	}
}
