package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func capacityClock(start time.Time) (*codexCapacityStats, func(time.Duration)) {
	now := start
	stats := newCodexCapacityStats()
	stats.now = func() time.Time { return now }
	return stats, func(d time.Duration) { now = now.Add(d) }
}

// A session that retries the same account inside the retry window is one
// episode, however many times it fires; a fresh session is a new episode.
func TestCodexCapacityRetriesCollapseIntoOneEpisode(t *testing.T) {
	stats, advance := capacityClock(time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	base := 2 * time.Minute
	first := stats.noteFailure("a", "s1", "server_is_overloaded", base)
	if first.Retry || first.Streak != 1 || first.MarkTTL != base {
		t.Fatalf("first failure verdict %+v", first)
	}
	for i := 0; i < 5; i++ {
		advance(2 * time.Second)
		if v := stats.noteFailure("a", "s1", "server_is_overloaded", base); !v.Retry {
			t.Fatalf("retry %d not recognised: %+v", i+1, v)
		}
	}
	second := stats.noteFailure("a", "s2", "server_is_overloaded", base)
	if second.Retry {
		t.Fatalf("new session counted as retry: %+v", second)
	}
	report := stats.snapshot(nil)
	if len(report.Accounts) != 1 {
		t.Fatalf("accounts %d", len(report.Accounts))
	}
	row := report.Accounts[0]
	if row.Last15m.Failures != 7 || row.Last15m.Episodes != 2 || row.Last15m.Sessions != 2 {
		t.Fatalf("window %+v", row.Last15m)
	}
	if row.Last15m.Rate != 1 {
		t.Fatalf("rate %v with no successes", row.Last15m.Rate)
	}
	if row.ByReason["server_is_overloaded"] != 7 {
		t.Fatalf("by_reason %+v", row.ByReason)
	}
}

// Three quick retries from one session are not persistent; the same streak
// spread over two sessions is, and the mark escalates per episode up to the
// cap. A success clears everything.
func TestCodexCapacityPersistentRuleAndEscalation(t *testing.T) {
	stats, advance := capacityClock(time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	base := 2 * time.Minute
	for i := 0; i < 3; i++ {
		v := stats.noteFailure("a", "s1", "server_is_overloaded", base)
		if v.Persistent {
			t.Fatalf("one session's retry burst marked persistent at %d: %+v", i+1, v)
		}
		advance(time.Second)
	}
	v := stats.noteFailure("a", "s2", "server_is_overloaded", base)
	if !v.Persistent || v.Streak != 4 {
		t.Fatalf("second session should make the streak persistent: %+v", v)
	}
	// episodes: s1 (1), s2 (2). Escalation starts doubling from the third.
	if v.MarkTTL != base {
		t.Fatalf("second episode ttl %s, want base %s", v.MarkTTL, base)
	}
	advance(codexCapacityRetryWindow + time.Second)
	v = stats.noteFailure("a", "s1", "server_is_overloaded", base)
	if v.Retry || v.MarkTTL != 2*base {
		t.Fatalf("third episode ttl %s retry=%v, want %s", v.MarkTTL, v.Retry, 2*base)
	}
	for i := 0; i < 6; i++ {
		advance(codexCapacityRetryWindow + time.Second)
		v = stats.noteFailure("a", "s3", "server_is_overloaded", base)
	}
	if v.MarkTTL != codexCapacityMaxMarkTTL {
		t.Fatalf("escalated ttl %s, want cap %s", v.MarkTTL, codexCapacityMaxMarkTTL)
	}
	if !stats.persistent("a") {
		t.Fatal("persistent flag lost")
	}
	stats.noteSuccess("a", "s3")
	if stats.persistent("a") {
		t.Fatal("success must end the persistent state")
	}
	v = stats.noteFailure("a", "s3", "server_is_overloaded", base)
	if v.Retry || v.Streak != 1 || v.MarkTTL != base {
		t.Fatalf("after success the next failure must start over: %+v", v)
	}
}

// Age alone also makes a streak persistent: one session failing for over a
// minute with no success is a bad account, not an unlucky turn.
func TestCodexCapacityPersistentByAge(t *testing.T) {
	stats, advance := capacityClock(time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	stats.noteFailure("a", "s1", "server_is_overloaded", 0)
	advance(30 * time.Second)
	stats.noteFailure("a", "s1", "server_is_overloaded", 0)
	advance(31 * time.Second)
	v := stats.noteFailure("a", "s1", "server_is_overloaded", 0)
	if !v.Persistent {
		t.Fatalf("streak older than %s not persistent: %+v", codexCapacityPersistentAge, v)
	}
	if v.MarkTTL != codexOverloadDefaultMarkTTL {
		t.Fatalf("zero base must fall back to the default ttl, got %s", v.MarkTTL)
	}
}

// Windows and rate: successes count as turns, old outcomes age out, and the
// sort puts the worst account first.
func TestCodexCapacitySnapshotWindowsAndOrder(t *testing.T) {
	stats, advance := capacityClock(time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	stats.noteFailure("old", "s", "server_is_overloaded", 0)
	advance(2 * time.Hour)
	stats.noteFailure("bad", "s1", "server_is_overloaded", 0)
	stats.noteSuccess("bad", "s1")
	stats.noteFailure("bad", "s2", "server_is_overloaded", 0)
	stats.noteSuccess("good", "s3")
	stats.noteSuccess("good", "s4")
	labels := map[string]accounts.Account{"bad": {ID: "bad", Label: "bad@example.com [pro]", Email: "bad@example.com"}}
	report := stats.snapshot(labels)
	if len(report.Accounts) != 3 || report.Accounts[0].ID != "bad" {
		t.Fatalf("order %+v", report.Accounts)
	}
	bad := report.Accounts[0]
	if bad.Label != "bad@example.com [pro]" {
		t.Fatalf("label %q", bad.Label)
	}
	if bad.Last1h.Episodes != 2 || bad.Last1h.Successes != 1 || bad.Last1h.Rate < 0.66 || bad.Last1h.Rate > 0.67 {
		t.Fatalf("bad 1h %+v", bad.Last1h)
	}
	if bad.Streak != 1 || bad.StreakSince == "" {
		t.Fatalf("bad streak %+v", bad)
	}
	var old CodexCapacityAccountStats
	for _, row := range report.Accounts {
		if row.ID == "old" {
			old = row
		}
	}
	if old.Last1h.Failures != 0 || old.Last24h.Failures != 1 || old.Last1h.Rate != -1 {
		t.Fatalf("old windows 1h=%+v 24h=%+v", old.Last1h, old.Last24h)
	}
	var good CodexCapacityAccountStats
	for _, row := range report.Accounts {
		if row.ID == "good" {
			good = row
		}
	}
	if good.Last15m.Rate != 0 || good.Last15m.Successes != 2 {
		t.Fatalf("good window %+v", good.Last15m)
	}
}

// End to end: the HTTP failover records the overloaded account's failure and
// the serving account's success, and the admin endpoint reports both.
func TestCodexCapacityEndpointReflectsFailover(t *testing.T) {
	pool, _ := codexOverloadPool(t, "oauth-token-0")
	poolURL, _ := url.Parse(pool.URL)
	server := codexOverloadServer(t, poolURL, 2, true)
	server.AdminToken = "admin"
	if _, err := server.Sessions.Put("codex", "session-a", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()
	if status, body := codexEgressPost(t, proxy.URL, "session-a"); status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") {
		t.Fatalf("status=%d body=%s", status, body)
	}
	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/_subrouter/codex-capacity", nil)
	req.Header.Set("Authorization", "Bearer admin")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("capacity endpoint status %d", res.StatusCode)
	}
	var report CodexCapacityReport
	if err := json.NewDecoder(res.Body).Decode(&report); err != nil {
		t.Fatal(err)
	}
	rows := map[string]CodexCapacityAccountStats{}
	for _, row := range report.Accounts {
		rows[row.ID] = row
	}
	failed := rows["codex-account-0"]
	if failed.Last15m.Failures != 1 || failed.Last15m.Episodes != 1 || failed.Streak != 1 || failed.MarkedUntil == "" {
		t.Fatalf("overloaded account row %+v", failed)
	}
	if failed.ByReason["server_is_overloaded"] != 1 {
		t.Fatalf("reason breakdown %+v", failed.ByReason)
	}
	served := rows["codex-account-1"]
	if served.Last15m.Successes != 1 || served.Last15m.Failures != 0 {
		t.Fatalf("serving account row %+v", served)
	}
	if !strings.Contains(report.PersistentRule, "streak >= 3") {
		t.Fatalf("rule text %q", report.PersistentRule)
	}
}

// A session past its reroute budget still leaves a persistently constrained
// account, but at most once per pacing interval.
func TestCodexOverloadRerouteAllowsPersistentPastBudget(t *testing.T) {
	counts := newCodexOverloadReroutes()
	for i := 0; i < codexOverloadMaxWebSocketReroutes; i++ {
		counts.allow("s", codexOverloadMaxWebSocketReroutes)
	}
	if counts.allow("s", codexOverloadMaxWebSocketReroutes) {
		t.Fatal("budget should be spent")
	}
	if counts.allowPersistent("s") {
		t.Fatal("persistent reroute must be paced from the last reroute")
	}
	counts.mu.Lock()
	entry := counts.entries["s"]
	entry.lastAllowed = time.Now().Add(-codexCapacityPersistentRerouteInterval - time.Second)
	counts.entries["s"] = entry
	counts.mu.Unlock()
	if !counts.allowPersistent("s") {
		t.Fatal("persistent reroute refused after the pacing interval")
	}
	if counts.allowPersistent("s") {
		t.Fatal("second persistent reroute inside the interval must be refused")
	}
}

func TestCodexCapacityReasonFromEvent(t *testing.T) {
	cases := map[string]string{
		`{"type":"response.failed","response":{"error":{"code":"server_is_overloaded","message":"Selected model is at capacity."}}}`: "server_is_overloaded",
		`{"type":"error","message":"Selected model is at capacity. Please try a different model."}`:                                  "server_is_overloaded",
		`{"type":"response.failed","response":{"error":{"code":"internal_error"}}}`:                                                  "internal_error",
		`not json`: "stream_failed",
	}
	for body, want := range cases {
		if got := codexCapacityReason([]byte(body), "stream_failed"); got != want {
			t.Errorf("%s: got %q want %q", body, got, want)
		}
	}
}
