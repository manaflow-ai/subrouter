package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/session"
)

// fakeBedrockPools answers each "region profile" pair with a status the test
// sets (200 when unset) and records the pairs called, in order.
type fakeBedrockPools struct {
	mu     sync.Mutex
	status map[string]int
	calls  []string
}

func (f *fakeBedrockPools) RoundTrip(req *http.Request) (*http.Response, error) {
	region := strings.TrimSuffix(strings.TrimPrefix(req.Host, "bedrock-runtime."), ".amazonaws.com")
	pair := region + " " + bedrockModelFromPath(req.URL.Path)
	f.mu.Lock()
	f.calls = append(f.calls, pair)
	status, ok := f.status[pair]
	f.mu.Unlock()
	if !ok {
		status = http.StatusOK
	}
	body := `{"message":"Too many requests, please wait before trying again."}`
	if status == http.StatusOK {
		body = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":3,"output_tokens":1}}`
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{"Content-Type": []string{"application/json"}}}, nil
}

func (f *fakeBedrockPools) takeCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

// A throttled us.* pool must not stop Claude: the request goes on to the eu.*
// profile in an EU region, the cost log names the profile that served it, and
// the next request starts on the pool that answered.
func TestClaudeBedrockTargetsServeFromAnotherPoolWhenOneThrottles(t *testing.T) {
	fake := &fakeBedrockPools{status: map[string]int{
		"us-east-1 us.anthropic.claude-opus-5-5": http.StatusTooManyRequests,
		"us-west-2 us.anthropic.claude-opus-5-5": http.StatusTooManyRequests,
	}}
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Claude subscription upstream called: %s", r.URL.Path)
		http.Error(w, "blocked", http.StatusBadGateway)
	}))
	t.Cleanup(blocked.Close)
	blockedURL, _ := url.Parse(blocked.URL)
	dir := t.TempDir()
	store, err := session.NewStore(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	costLog := filepath.Join(dir, "bedrock-cost.jsonl")
	router := httptest.NewServer(Server{
		ClaudeUpstream: blockedURL,
		Accounts: []accounts.Account{{
			ID: "claude-a@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-a",
		}},
		Sessions:             store,
		MaxBodyBytes:         1 << 20,
		ClaudeBedrockPrimary: true,
		ClaudeBedrockModelID: "us.anthropic.claude-opus-5-5",
		Bedrock: &BedrockConfig{
			Regions: []string{"us-east-1"},
			ClaudeTargets: []BedrockTarget{
				{Region: "us-east-1", Model: "us.anthropic.claude-opus-5-5"},
				{Region: "us-west-2", Model: "us.anthropic.claude-opus-5-5"},
				{Region: "eu-west-1", Model: "eu.anthropic.claude-opus-5-5"},
				{Region: "us-east-1", Model: "global.anthropic.claude-opus-5-5"},
			},
			Sources:     []BedrockCredentialSource{{Name: "aw1", Credentials: staticBedrockCreds()}},
			Transport:   fake,
			CostLogPath: costLog,
		},
	}.Handler())
	t.Cleanup(router.Close)

	status, body := postClaudeMessages(t, router, false)
	if status != http.StatusOK || !strings.Contains(body, `"text":"ok"`) {
		t.Fatalf("status=%d body=%s", status, body)
	}
	want := []string{
		"us-east-1 us.anthropic.claude-opus-5-5",
		"us-west-2 us.anthropic.claude-opus-5-5",
		"eu-west-1 eu.anthropic.claude-opus-5-5",
	}
	if got := fake.takeCalls(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	logged, err := os.ReadFile(costLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logged), `"model":"eu.anthropic.claude-opus-5-5"`) || !strings.Contains(string(logged), `"region":"eu-west-1"`) {
		t.Fatalf("cost log does not name the serving pool: %s", logged)
	}

	// Both us.* pairs are cooling down, so the next request starts in the EU.
	status, _ = postClaudeMessages(t, router, false)
	if status != http.StatusOK {
		t.Fatalf("second status=%d", status)
	}
	if got := fake.takeCalls(); len(got) != 1 || got[0] != "eu-west-1 eu.anthropic.claude-opus-5-5" {
		t.Fatalf("second request calls = %v, want only eu-west-1 eu.*", got)
	}
}
