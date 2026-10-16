package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/session"
)

// fakeBedrockRegions answers each Bedrock region with a status the test sets,
// and records which region served each upstream call.
type fakeBedrockRegions struct {
	t      *testing.T
	mu     sync.Mutex
	status map[string]int
	stream map[string][]byte
	calls  []string
}

func (f *fakeBedrockRegions) set(region string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[region] = status
}

func (f *fakeBedrockRegions) takeCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

func (f *fakeBedrockRegions) RoundTrip(req *http.Request) (*http.Response, error) {
	region := strings.TrimSuffix(strings.TrimPrefix(req.Host, "bedrock-runtime."), ".amazonaws.com")
	f.mu.Lock()
	f.calls = append(f.calls, region)
	status := f.status[region]
	streamBody := f.stream[region]
	f.mu.Unlock()
	if strings.HasSuffix(req.URL.Path, "/invoke-with-response-stream") && status == http.StatusOK && streamBody != nil {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(streamBody)),
			Header:     http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
		}, nil
	}
	body := `{"message":"Too many requests, please wait before trying again."}`
	if status == http.StatusOK {
		body = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":3,"output_tokens":1}}`
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// bedrockPrimaryRouter runs the router's real HTTP handler with the live
// Bedrock-primary shape: Claude subscriptions blocked, every Claude call to
// Bedrock first, three regions and one profile.
func bedrockPrimaryRouter(t *testing.T, fake *fakeBedrockRegions) *httptest.Server {
	t.Helper()
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Claude subscription upstream called: %s", r.URL.Path)
		http.Error(w, "blocked", http.StatusBadGateway)
	}))
	t.Cleanup(blocked.Close)
	blockedURL, _ := url.Parse(blocked.URL)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	router := httptest.NewServer(Server{
		ClaudeUpstream: blockedURL,
		Accounts: []accounts.Account{{
			ID: "claude-a@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-a",
		}},
		Sessions:             store,
		MaxBodyBytes:         1 << 20,
		ClaudeBedrockPrimary: true,
		ClaudeBedrockModelID: "global.anthropic.claude-opus-5-5",
		Bedrock: &BedrockConfig{
			Regions:   []string{"us-east-1", "us-west-2", "us-east-2"},
			Sources:   []BedrockCredentialSource{{Name: "aw1", Credentials: staticBedrockCreds()}},
			Transport: fake,
		},
	}.Handler())
	t.Cleanup(router.Close)
	return router
}

func postClaudeMessages(t *testing.T, router *httptest.Server, stream bool) (int, string) {
	t.Helper()
	body := `{"model":"claude-opus-5-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	if stream {
		body = `{"model":"claude-opus-5-5","stream":true,"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	}
	req, err := http.NewRequest(http.MethodPost, router.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func equalRegions(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// A throttled region must not reach the client: the request moves on to the
// next region, the throttled region cools down so the next request starts
// on a region that answers, and the client sees 429 only when every region
// throttled.
func TestBedrockPrimaryFailsOverThrottledRegionAndCoolsItDown(t *testing.T) {
	fake := &fakeBedrockRegions{t: t, status: map[string]int{
		"us-east-1": http.StatusTooManyRequests,
		"us-west-2": http.StatusOK,
		"us-east-2": http.StatusOK,
	}}
	router := bedrockPrimaryRouter(t, fake)

	if status, body := postClaudeMessages(t, router, false); status != http.StatusOK || !strings.Contains(body, `"ok"`) {
		t.Fatalf("first request = %d %q, want 200 from the next region", status, body)
	}
	if calls := fake.takeCalls(); !equalRegions(calls, "us-east-1", "us-west-2") {
		t.Fatalf("first request regions = %v, want us-east-1 (429) then us-west-2", calls)
	}

	if status, _ := postClaudeMessages(t, router, false); status != http.StatusOK {
		t.Fatalf("second request = %d, want 200", status)
	}
	if calls := fake.takeCalls(); !equalRegions(calls, "us-west-2") {
		t.Fatalf("second request regions = %v, want us-west-2 only while us-east-1 cools down", calls)
	}

	fake.set("us-west-2", http.StatusTooManyRequests)
	fake.set("us-east-2", http.StatusTooManyRequests)
	if status, _ := postClaudeMessages(t, router, false); status != http.StatusTooManyRequests {
		t.Fatalf("all-throttled request = %d, want 429", status)
	}
	if calls := fake.takeCalls(); len(calls) != 3 {
		t.Fatalf("all-throttled request regions = %v, want one pass over all 3 regions", calls)
	}
}

// A stream whose first frame is a throttlingException has sent no bytes to
// the client, so the router replays it in another region; that region then
// serves the next request first.
func TestBedrockPrimaryStreamThrottleBeforeContentFailsOver(t *testing.T) {
	good := append(
		buildEventStreamFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":4}}}`),
		append(
			buildEventStreamFrame(t, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`),
			buildEventStreamFrame(t, `{"type":"message_stop"}`)...,
		)...,
	)
	throttled := buildBedrockExceptionFrame(t, "throttlingException", `{"message":"Too many requests"}`)
	fake := &fakeBedrockRegions{t: t,
		status: map[string]int{"us-east-1": http.StatusOK, "us-west-2": http.StatusOK, "us-east-2": http.StatusOK},
		stream: map[string][]byte{"us-east-1": throttled, "us-west-2": good, "us-east-2": good},
	}
	router := bedrockPrimaryRouter(t, fake)

	status, body := postClaudeMessages(t, router, true)
	if status != http.StatusOK || !strings.Contains(body, "event: message_stop") || strings.Contains(body, "throttlingException") {
		t.Fatalf("stream = %d %q, want the clean stream from the next region", status, body)
	}
	if calls := fake.takeCalls(); !equalRegions(calls, "us-east-1", "us-west-2") {
		t.Fatalf("stream regions = %v, want us-east-1 (throttled) then us-west-2", calls)
	}

	if status, _ := postClaudeMessages(t, router, true); status != http.StatusOK {
		t.Fatalf("second stream = %d, want 200", status)
	}
	if calls := fake.takeCalls(); !equalRegions(calls, "us-west-2") {
		t.Fatalf("second stream regions = %v, want us-west-2 first while us-east-1 cools down", calls)
	}
}
