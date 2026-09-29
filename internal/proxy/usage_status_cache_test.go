package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

type usageRoundTripper struct {
	calls     int
	responses []*http.Response
}

func (u *usageRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	u.calls++
	if len(u.responses) == 0 {
		return &http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
	}
	res := u.responses[0]
	if len(u.responses) > 1 {
		u.responses = u.responses[1:]
	}
	return res, nil
}

func usageOKResponse() *http.Response {
	body := `{"five_hour":{"utilization":10.0,"resets_at":"2030-01-01T00:00:00+00:00"},"seven_day":{"utilization":5.0,"resets_at":"2030-01-02T00:00:00+00:00"}}`
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

func usage429Response() *http.Response {
	return &http.Response{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests", Body: io.NopCloser(strings.NewReader(`{"type":"error"}`)), Header: http.Header{}}
}

func cacheTestAccountRef(t *testing.T, transport http.RoundTripper) *AccountRef {
	t.Helper()
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".subrouter", "codex")
	profileDir := filepath.Join(claudeDir, "claude", "_ptest")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	profiles := map[string]any{
		"profiles": map[string]any{
			"claude@example.com": map[string]any{"name": "claude@example.com", "dir": "_ptest"},
		},
	}
	body, err := json.Marshal(profiles)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "claude.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour).UnixMilli()
	credential := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"tok","refreshToken":"ref","expiresAt":%d,"subscriptionType":"max"}}`, expires)
	if err := os.WriteFile(filepath.Join(profileDir, ".credentials.json"), []byte(credential), 0o600); err != nil {
		t.Fatal(err)
	}
	return &AccountRef{
		store:       accounts.CodexStore{Dir: filepath.Join(dir, "codex-accounts")},
		claudeStore: agentclaude.Store{Dir: claudeDir},
		client:      &http.Client{Transport: transport},
	}
}

func TestUsageStatusesCachesWithinTTL(t *testing.T) {
	transport := &usageRoundTripper{responses: []*http.Response{usageOKResponse()}}
	ref := cacheTestAccountRef(t, transport)
	first := ref.UsageStatuses(context.Background())
	callsAfterFirst := transport.calls
	second := ref.UsageStatuses(context.Background())
	if transport.calls != callsAfterFirst {
		t.Fatalf("second call within TTL hit upstream (%d -> %d calls)", callsAfterFirst, transport.calls)
	}
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("statuses = %d/%d, want 1/1", len(first), len(second))
	}
	if len(second[0].Windows) == 0 {
		t.Fatal("cached status lost usage windows")
	}
}

func TestUsageStatusRefreshQueryInvalidatesCache(t *testing.T) {
	transport := &usageRoundTripper{responses: []*http.Response{usageOKResponse(), usageOKResponse(), usageOKResponse(), usageOKResponse()}}
	ref := cacheTestAccountRef(t, transport)
	handler := Server{AccountRef: ref}.Handler()
	for _, target := range []string{"/_subrouter/usage-status", "/_subrouter/usage-status?refresh=1"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", target, response.Code, response.Body.String())
		}
	}
	if transport.calls != 4 {
		t.Fatalf("refresh query made %d upstream calls, want 4", transport.calls)
	}
}

func TestUsageStatusesRestoresLastGoodOnTransientFailure(t *testing.T) {
	transport := &usageRoundTripper{responses: []*http.Response{usageOKResponse(), usage429Response()}}
	ref := cacheTestAccountRef(t, transport)
	first := ref.UsageStatuses(context.Background())
	if len(first) != 1 || len(first[0].Windows) == 0 {
		t.Fatalf("first sweep should have windows, got %+v", first)
	}
	ref.InvalidateUsageStatusCache()
	second := ref.UsageStatuses(context.Background())
	if len(second) != 1 {
		t.Fatalf("statuses = %d, want 1", len(second))
	}
	if len(second[0].Windows) == 0 {
		t.Fatalf("429 sweep should serve last-known-good windows, got %+v", second[0])
	}
	if second[0].Error != "" {
		t.Fatalf("transient failure with last-good backfill should not surface an error, got %q", second[0].Error)
	}
}
