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
	// Include both provider shapes because the shared transport is used by
	// Claude and Codex account fixtures. Codex must expose real quota windows;
	// an empty rate_limit is now treated as unknown rather than 100% available.
	body := `{"five_hour":{"utilization":10.0,"resets_at":"2030-01-01T00:00:00+00:00"},"seven_day":{"utilization":5.0,"resets_at":"2030-01-02T00:00:00+00:00"},"rate_limit":{"primary_window":{"used_percent":10.0,"limit_window_seconds":18000},"secondary_window":{"used_percent":5.0,"limit_window_seconds":604800}}}`
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

func TestUsageStatusesPreservesProviderObservationTimeOnWindowCacheHit(t *testing.T) {
	transport := &usageRoundTripper{responses: []*http.Response{usageOKResponse()}}
	ref := cacheTestAccountRef(t, transport)
	first := ref.UsageStatuses(context.Background())
	if len(first) != 1 || first[0].UsageFetchedAt.IsZero() {
		t.Fatalf("first sweep missing usage observation time: %+v", first)
	}
	callsAfterFirst := transport.calls
	ref.InvalidateUsageStatusCache()
	second := ref.UsageStatuses(context.Background())
	if transport.calls != callsAfterFirst {
		t.Fatalf("window cache hit contacted upstream (%d -> %d calls)", callsAfterFirst, transport.calls)
	}
	if !second[0].UsageFresh {
		t.Fatal("window cache hit was not marked fresh")
	}
	if !second[0].UsageFetchedAt.Equal(first[0].UsageFetchedAt) {
		t.Fatalf("cache-hit observation time = %s, want original %s", second[0].UsageFetchedAt, first[0].UsageFetchedAt)
	}
}

func TestUsageStatusRefreshQueryKeepsAccountWindowCache(t *testing.T) {
	transport := &usageRoundTripper{responses: []*http.Response{usageOKResponse(), usageOKResponse(), usageOKResponse(), usageOKResponse()}}
	ref := cacheTestAccountRef(t, transport)
	handler := Server{AccountRef: ref}.Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/_subrouter/usage-status", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("initial usage status = %d: %s", response.Code, response.Body.String())
	}
	callsAfterInitial := transport.calls
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/_subrouter/usage-status?refresh=1", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("refreshed usage status = %d: %s", response.Code, response.Body.String())
	}
	if transport.calls != callsAfterInitial {
		t.Fatalf("refresh query discarded account window cache and made %d new upstream calls", transport.calls-callsAfterInitial)
	}
	for _, target := range []string{"/_subrouter/usage-status?refresh=1", "/_subrouter/usage-status?refresh=1"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", target, response.Code, response.Body.String())
		}
	}
	if transport.calls != callsAfterInitial {
		t.Fatalf("repeated refresh queries made %d upstream calls after initial sweep", transport.calls-callsAfterInitial)
	}
}

func TestUsageStatusesRestoresLastGoodOnTransientFailure(t *testing.T) {
	transport := &usageRoundTripper{responses: []*http.Response{usageOKResponse(), usage429Response()}}
	ref := cacheTestAccountRef(t, transport)
	first := ref.UsageStatuses(context.Background())
	if len(first) != 1 || len(first[0].Windows) == 0 {
		t.Fatalf("first sweep should have windows, got %+v", first)
	}
	if first[0].UsageFetchedAt.IsZero() {
		t.Fatal("successful usage sweep did not record an observation time")
	}
	// Age the per-account cache past its fresh TTL without dropping it. The
	// next sweep then exercises the transient-failure fallback instead of
	// returning the cache before the provider is contacted.
	ref.usageWindowsMu.Lock()
	key := "claude@example.com\x00claude"
	entry := ref.usageWindows[key]
	entry.at = time.Now().Add(-usageWindowsTTL - time.Second)
	ref.usageWindows[key] = entry
	ref.usageWindowsMu.Unlock()
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
	if second[0].UsageFresh {
		t.Fatal("transient failure was marked as fresh")
	}
	if !second[0].UsageFetchedAt.Equal(first[0].UsageFetchedAt) {
		t.Fatalf("stale fallback observation time = %s, want original %s", second[0].UsageFetchedAt, first[0].UsageFetchedAt)
	}
}

func TestMergeUsageStatusesExpiresFromProviderObservationTime(t *testing.T) {
	ref := &AccountRef{}
	observedAt := time.Now().Add(-usageStatusLastGoodTTL - time.Minute)
	live := AccountUsageStatus{
		AccountStatus:  AccountStatus{ID: "claude@example.com", Provider: accounts.ProviderClaude},
		UsageFresh:     true,
		UsageFetchedAt: observedAt,
		Windows: []accounts.UsageWindow{{
			Name: "7d", UsedPercent: 10, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second),
		}},
	}
	ref.mergeUsageStatusesLocked([]AccountUsageStatus{live}, ref.usageStatusEpoch)

	// A later failed sweep has no windows of its own. If the snapshot age were
	// measured from the merge time instead of the provider observation, this
	// would incorrectly resurrect the old quota.
	stale := live
	stale.UsageFresh = false
	stale.Windows = nil
	got := ref.mergeUsageStatusesLocked([]AccountUsageStatus{stale}, ref.usageStatusEpoch)
	if len(got) != 1 || len(got[0].Windows) != 0 {
		t.Fatalf("expired last-good quota was restored: %+v", got)
	}
}

func TestInvalidatedUsageSweepCannotReviveLastGoodQuota(t *testing.T) {
	ref := &AccountRef{}
	oldEpoch := ref.usageStatusEpoch
	old := AccountUsageStatus{
		AccountStatus:  AccountStatus{ID: "same-account", Provider: accounts.ProviderClaude},
		UsageFresh:     true,
		UsageFetchedAt: time.Now(),
		Windows:        []accounts.UsageWindow{{
			Name: "7d", UsedPercent: 20,
		}},
	}
	// A newer refresh invalidates an in-flight sweep before it completes.
	ref.InvalidateUsageStatusCache()
	_ = ref.mergeUsageStatusesLocked([]AccountUsageStatus{old}, oldEpoch)
	if len(ref.lastGoodUsage) != 0 || len(ref.usageStatusCache) != 0 {
		t.Fatal("invalidated sweep published outdated quota history")
	}
	failed := old
	failed.Windows = nil
	failed.UsageFresh = false
	got := ref.mergeUsageStatusesLocked([]AccountUsageStatus{failed}, ref.usageStatusEpoch)
	if len(got) != 1 || len(got[0].Windows) != 0 {
		t.Fatalf("new sweep inherited superseded credential's quota: %+v", got)
	}
}
