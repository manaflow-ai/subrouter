package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/selectacct"
)

// codexVisibilityServer is a two-account Codex pool whose account IDs are
// emails, so the tests can assert no email leaks into the payloads.
func codexVisibilityServer(t *testing.T, failToken func(string) bool, failover bool) (Server, *httptest.Server) {
	t.Helper()
	poolURL, _ := codexCapacityPool(t, func(token string, _ int) bool { return failToken(token) })
	server := codexOverloadServer(t, poolURL, 2, failover)
	if failover {
		server.CodexOverloadFailover.MarkTTL = 20 * time.Second
		fastCapacityGaps(server.CodexOverloadFailover, 5*time.Millisecond)
	}
	server.Accounts[0].ID, server.Accounts[0].Email = "alice@example.com", "alice@example.com"
	server.Accounts[1].ID, server.Accounts[1].Email = "bob@example.com", "bob@example.com"
	server.SchedulerRef = selectacct.NewSchedulerRef(server.Scheduler)
	server.AdminToken = "visibility-admin"
	if _, err := server.Sessions.Put("codex", "session-visibility", "alice@example.com", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	t.Cleanup(proxy.Close)
	return server, proxy
}

func codexVisibilityPost(t *testing.T, proxyURL string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/responses",
		strings.NewReader(`{"model":"gpt-6-astra","session_id":"session-visibility","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(AgentRetryPolicyHeader, "bounded")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	return response
}

func codexVisibilityGet(t *testing.T, proxyURL, path, token string) []byte {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, proxyURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", path, response.StatusCode, body)
	}
	return body
}

// /_subrouter/traffic needs no admin token, so its capacity marks carry an
// account hash, never the account's email.
func TestCodexCapacityTrafficShowsLiveMarksWithoutEmails(t *testing.T) {
	_, proxy := codexVisibilityServer(t, func(token string) bool { return token == "oauth-token-0" }, true)
	if response := codexVisibilityPost(t, proxy.URL); response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want the switch to serve the turn", response.StatusCode)
	}
	body := codexVisibilityGet(t, proxy.URL, "/_subrouter/traffic", "")
	if strings.Contains(string(body), "example.com") || strings.Contains(string(body), "alice") {
		t.Fatalf("traffic payload leaked an account email: %s", body)
	}
	var snapshot struct {
		CodexCapacity struct {
			Marks []struct {
				AccountHash string `json:"account_hash"`
				Pool        string `json:"pool"`
				Failures    int    `json:"failures"`
				ExpiresAt   string `json:"expires_at"`
				Reason      string `json:"reason"`
			} `json:"marks"`
			Counters map[string]uint64 `json:"counters"`
		} `json:"codex_capacity"`
	}
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatal(err)
	}
	marks := snapshot.CodexCapacity.Marks
	if len(marks) != 1 {
		t.Fatalf("marks=%+v, want one mark on the failing account; body=%s", marks, body)
	}
	mark := marks[0]
	if mark.AccountHash != admissionAccountHash("alice@example.com") || mark.Pool != selectacct.CapacityPoolKey("gpt-6-astra", "") ||
		mark.Reason != "pool_stream_failed" || mark.Failures < 2 {
		t.Fatalf("mark=%+v", mark)
	}
	expires, err := time.Parse(time.RFC3339, mark.ExpiresAt)
	if err != nil || !expires.After(time.Now()) || expires.After(time.Now().Add(time.Minute)) || !strings.HasSuffix(mark.ExpiresAt, "Z") {
		t.Fatalf("expires_at=%q err=%v, want a near future UTC time", mark.ExpiresAt, err)
	}
	counters := snapshot.CodexCapacity.Counters
	if counters["marks"] < 2 || counters["same_account_retries"] < 1 || counters["account_switches"] != 1 || counters["exhausted"] != 0 {
		t.Fatalf("counters=%v", counters)
	}
}

// usage-status lists each Codex account's own live capacity marks.
func TestCodexCapacityUsageStatusShowsAccountMarks(t *testing.T) {
	_, proxy := codexVisibilityServer(t, func(token string) bool { return token == "oauth-token-0" }, true)
	codexVisibilityPost(t, proxy.URL)
	body := codexVisibilityGet(t, proxy.URL, "/_subrouter/usage-status", "visibility-admin")
	var statuses []struct {
		ID            string `json:"id"`
		CapacityMarks []struct {
			Pool      string `json:"pool"`
			Failures  int    `json:"failures"`
			ExpiresAt string `json:"expires_at"`
			Reason    string `json:"reason"`
		} `json:"capacity_marks"`
	}
	if err := json.Unmarshal(body, &statuses); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, status := range statuses {
		switch status.ID {
		case "alice@example.com":
			seen++
			if len(status.CapacityMarks) != 1 {
				t.Fatalf("alice capacity_marks=%+v", status.CapacityMarks)
			}
			mark := status.CapacityMarks[0]
			if mark.Pool != selectacct.CapacityPoolKey("gpt-6-astra", "") || mark.Reason != "pool_stream_failed" || mark.Failures < 2 {
				t.Fatalf("alice mark=%+v", mark)
			}
			if _, err := time.Parse(time.RFC3339, mark.ExpiresAt); err != nil {
				t.Fatalf("expires_at=%q: %v", mark.ExpiresAt, err)
			}
		case "bob@example.com":
			seen++
			if status.CapacityMarks != nil {
				t.Fatalf("bob capacity_marks=%+v, want omitted", status.CapacityMarks)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("statuses=%s", body)
	}
	if strings.Count(string(body), "capacity_marks") != 1 {
		t.Fatalf("capacity_marks should be omitted for unmarked accounts: %s", body)
	}
}

// A capacity failure that outlasts every retry reaches the client labeled,
// with a Retry-After from the pool's soonest mark expiry.
func TestCodexCapacityExhaustionIsLabeled(t *testing.T) {
	_, proxy := codexVisibilityServer(t, func(string) bool { return true }, true)
	response := codexVisibilityPost(t, proxy.URL)
	if got := response.Header.Get(subrouterReasonHeader); got != "model_capacity" {
		t.Fatalf("%s=%q, want model_capacity (status %d)", subrouterReasonHeader, got, response.StatusCode)
	}
	seconds, err := strconv.Atoi(response.Header.Get("Retry-After"))
	// Marks last MarkTTL (20s) +-20%, so the soonest expiry is under 25s.
	if err != nil || seconds < 1 || seconds > 25 {
		t.Fatalf("Retry-After=%q, want the soonest mark expiry in seconds", response.Header.Get("Retry-After"))
	}
	body := codexVisibilityGet(t, proxy.URL, "/_subrouter/traffic", "")
	var snapshot struct {
		CodexCapacity struct {
			Counters map[string]uint64 `json:"counters"`
		} `json:"codex_capacity"`
	}
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.CodexCapacity.Counters["exhausted"] != 1 {
		t.Fatalf("counters=%v, want one exhausted", snapshot.CodexCapacity.Counters)
	}
}

// Without the failover there are no marks: Retry-After falls back to 5s.
func TestCodexCapacityExhaustionDefaultsRetryAfterWithoutMarks(t *testing.T) {
	_, proxy := codexVisibilityServer(t, func(string) bool { return true }, false)
	response := codexVisibilityPost(t, proxy.URL)
	if got := response.Header.Get(subrouterReasonHeader); got != "model_capacity" {
		t.Fatalf("%s=%q, want model_capacity", subrouterReasonHeader, got)
	}
	if got := response.Header.Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After=%q, want 5", got)
	}
}
func TestCodexCapacityRetryAfterSecondsClamps(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		until time.Time
		want  int
	}{
		{time.Time{}, 5},
		{now.Add(200 * time.Millisecond), 1},
		{now.Add(12500 * time.Millisecond), 13},
		{now.Add(10 * time.Minute), 60},
	} {
		if got := codexCapacityRetryAfterSeconds(tc.until, now); got != tc.want {
			t.Fatalf("until=%v got=%d want=%d", tc.until.Sub(now), got, tc.want)
		}
	}
}
