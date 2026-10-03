package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/storepath"
	"github.com/manaflow-ai/subrouter/wake"
	"strconv"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestSyncRecoveryAlarmsBindsRecentCMUXSession(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", stateRoot)
	now := time.Now().UTC().Truncate(time.Second)
	sessionID := "session-recent"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_subrouter/recovery-status" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode([]recoveryWireState{{Agent: "claude", SessionID: sessionID, Kind: wake.KindClaudeQuota, LastActivityAt: now.Add(-time.Minute), LastFailureAt: now.Add(-time.Minute), ResetAt: now.Add(time.Minute)}})
	}))
	defer server.Close()
	cmux := filepath.Join(stateRoot, "cmux-fake")
	sessions := `{"sessions":[{"agent":"claude","session_id":"session-recent","surface_id":"surface-1","updated_at":"` + now.Format(time.RFC3339) + `"}]}`
	script := "#!/bin/sh\nif [ \"$1\" = sessions ]; then printf '%s'; else printf 'prompt'; fi\n"
	script = strings.Replace(script, "%s", sessions, 1)
	if err := os.WriteFile(cmux, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	store := wake.NewStore(filepath.Join(storepath.StateDir(), "wake.json"))
	if err := wake.NewConfig(filepath.Join(storepath.StateDir(), "wake-config.json")).SetEnabled("claude", true); err != nil {
		t.Fatal(err)
	}
	if err := syncRecoveryAlarms(store, server.URL, cmux, now.Add(-time.Minute), true); err != nil {
		t.Fatal(err)
	}
	alarms, err := store.List(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(alarms) != 1 || alarms[0].SurfaceID != "surface-1" || alarms[0].Action != "continue" {
		t.Fatalf("alarms=%+v", alarms)
	}
}

func TestResumeActionForSurfaceChoosesGoalOrRegularSession(t *testing.T) {
	if got := resumeActionForSurface("ready for the next prompt"); got != "continue" {
		t.Fatalf("regular session action=%q, want continue", got)
	}
	for _, screen := range []string{"Goal paused", "Goal stalled (/goal resume)", "pursuing goal (idle)"} {
		if got := resumeActionForSurface(screen); got != "/goal resume" {
			t.Fatalf("screen %q action=%q, want /goal resume", screen, got)
		}
	}
}

func TestSyncRecoveryAlarmsUsesGoalResumeForCodexQuota(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", stateRoot)
	now := time.Now().UTC().Truncate(time.Second)
	sessionID := "codex-quota-session"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_subrouter/recovery-status" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode([]recoveryWireState{{Agent: "codex", SessionID: sessionID, Kind: wake.KindCodexQuota, LastActivityAt: now.Add(-time.Minute), LastFailureAt: now.Add(-time.Minute), ResetAt: now.Add(time.Minute)}})
	}))
	defer server.Close()
	cmux := filepath.Join(stateRoot, "cmux-fake")
	sessions := `{"sessions":[{"agent":"codex","session_id":"` + sessionID + `","surface_id":"surface-codex","updated_at":"` + now.Format(time.RFC3339) + `"}]}`
	script := "#!/bin/sh\nif [ \"$1\" = sessions ]; then printf '%s'; else printf 'Goal stalled (/goal resume)'; fi\n"
	script = strings.Replace(script, "%s", sessions, 1)
	if err := os.WriteFile(cmux, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	store := wake.NewStore(filepath.Join(storepath.StateDir(), "wake.json"))
	if err := wake.NewConfig(filepath.Join(storepath.StateDir(), "wake-config.json")).SetEnabled("codex", true); err != nil {
		t.Fatal(err)
	}
	if err := syncRecoveryAlarms(store, server.URL, cmux, now.Add(-time.Minute), true); err != nil {
		t.Fatal(err)
	}
	alarms, err := store.List(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(alarms) != 1 || alarms[0].Action != "/goal resume" {
		t.Fatalf("alarms=%+v", alarms)
	}
}

func TestSyncRecoveryAlarmsDoesNotEnqueueWhenDisabled(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", stateRoot)
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]recoveryWireState{{Agent: "claude", SessionID: "disabled", Kind: wake.KindClaudeQuota, LastActivityAt: now, LastFailureAt: now}})
	}))
	defer server.Close()
	cmux := filepath.Join(stateRoot, "cmux-fake")
	if err := os.WriteFile(cmux, []byte("#!/bin/sh\nprintf '{\"sessions\":[]}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	store := wake.NewStore(filepath.Join(storepath.StateDir(), "wake.json"))
	if err := syncRecoveryAlarms(store, server.URL, cmux, now, true); err != nil {
		t.Fatal(err)
	}
	alarms, err := store.List(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(alarms) != 0 {
		t.Fatalf("disabled automatic recovery enqueued alarms=%+v", alarms)
	}
}

func TestDispatchManualAlarmIgnoresAutomaticDisabledSetting(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", stateRoot)
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/_subrouter/recovery-status" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	cmux := filepath.Join(stateRoot, "cmux-fake")
	if err := os.WriteFile(cmux, []byte("#!/bin/sh\nif [ \"$1\" = sessions ]; then printf '{\"sessions\":[]}'; elif [ \"$1\" = read-screen ]; then printf prompt; else exit 0; fi\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	store := wake.NewStore(filepath.Join(storepath.StateDir(), "wake.json"))
	if _, err := store.Put(wake.Alarm{Kind: wake.KindClaudeQuota, Agent: "claude", SessionID: "manual", SurfaceID: "surface", Action: "continue", WakeAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Hour), SessionLastActiveAt: now}, now); err != nil {
		t.Fatal(err)
	}
	if err := dispatchDueWakeAlarms(store, server.URL, cmux, 0, now.Add(-time.Minute), &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	alarms, err := store.List(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(alarms) != 1 || alarms[0].Status != wake.StatusCompleted {
		t.Fatalf("manual alarm was not dispatched while disabled: %+v", alarms)
	}
}

func TestSyncRecoveryAlarmsRejectsStaleInitialSession(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", stateRoot)
	now := time.Now().UTC()
	stale := now.Add(-9 * time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]recoveryWireState{{Agent: "codex", SessionID: "stale", Kind: wake.KindCodexQuota, LastActivityAt: stale, LastFailureAt: stale}})
	}))
	defer server.Close()
	cmux := filepath.Join(stateRoot, "cmux-fake")
	if err := os.WriteFile(cmux, []byte("#!/bin/sh\nprintf '{\"sessions\":[]}\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	store := wake.NewStore(filepath.Join(storepath.StateDir(), "wake.json"))
	if err := syncRecoveryAlarms(store, server.URL, cmux, now, true); err != nil {
		t.Fatal(err)
	}
	alarms, err := store.List(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(alarms) != 0 {
		t.Fatalf("stale alarms=%+v", alarms)
	}
}

func TestAlarmDueAtUsesStableBoundedJitter(t *testing.T) {
	alarm := wake.Alarm{ID: "w_jitter", WakeAt: time.Unix(100, 0).UTC(), JitterSeconds: 30}
	first, second := alarmDueAt(alarm), alarmDueAt(alarm)
	if !first.Equal(second) {
		t.Fatalf("jitter is not deterministic: %v vs %v", first, second)
	}
	if first.Before(alarm.WakeAt) || first.After(alarm.WakeAt.Add(30*time.Second)) {
		t.Fatalf("jitter outside bound: %v", first)
	}
}

func TestWakeLaunchdPlistPinsStateRoot(t *testing.T) {
	plist := wakeLaunchdPlist("/tmp/subrouter&candidate", "/tmp/state&root", "/tmp/logs", "/tmp/cmux&bin", "http://100.92.167.122:31415")
	for _, want := range []string{
		"<key>SUBROUTER_STATE_DIR</key><string>/tmp/state&amp;root</string>",
		"<string>/tmp/subrouter&amp;candidate</string>",
		"<string>/tmp/cmux&amp;bin</string>",
		// A client follows its pool server, not a loopback proxy it lacks.
		"<string>--server</string><string>http://100.92.167.122:31415</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist missing %q:\n%s", want, plist)
		}
	}
}

func TestEarlyRecoveryAdvancesOnlyMatchingAutomaticQuotaAlarm(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", root)
	now := time.Now().UTC()
	store := wake.NewStore(filepath.Join(root, "wake.json"))
	cfg := wake.NewConfig(filepath.Join(root, "wake-config.json"))
	for _, agent := range []string{"codex", "claude"} {
		if err := cfg.SetEnabled(agent, true); err != nil {
			t.Fatal(err)
		}
	}
	alarms := []wake.Alarm{
		{Agent: "codex", Kind: wake.KindCodexQuota, SessionID: "codex-quota", SurfaceID: "c1", Action: "/goal resume", Automatic: true},
		{Agent: "codex", Kind: wake.KindCodexQuota, SessionID: "codex-spark", SurfaceID: "c4", Pool: "gpt-5.3-codex-spark", Action: "/goal resume", Automatic: true},
		{Agent: "claude", Kind: wake.KindClaudeQuota, SessionID: "claude-quota", SurfaceID: "a1", Action: "continue", Automatic: true},
		{Agent: "codex", Kind: wake.KindCodexProvider, SessionID: "provider", SurfaceID: "c2", Action: "/goal resume", Automatic: true},
		{Agent: "codex", Kind: wake.KindCodexQuota, SessionID: "manual", SurfaceID: "c3", Action: "/goal resume"},
	}
	for _, alarm := range alarms {
		alarm.WakeAt = now.Add(5 * 24 * time.Hour)
		alarm.ExpiresAt = now.Add(6 * 24 * time.Hour)
		alarm.SessionLastActiveAt = now
		if _, err := store.Put(alarm, now); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_subrouter/recovery-readiness" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"ready": r.URL.Query().Get("agent") == "codex" && r.URL.Query().Get("pool") == ""})
	}))
	defer server.Close()
	if err := accelerateRecoveredQuotaAlarms(store, server.URL, now); err != nil {
		t.Fatal(err)
	}
	got, err := store.List(now)
	if err != nil {
		t.Fatal(err)
	}
	for _, alarm := range got {
		wantEarly := alarm.SessionID == "codex-quota"
		if wantEarly != !alarm.AcceleratedAt.IsZero() {
			t.Fatalf("alarm %s accelerated=%v", alarm.SessionID, !alarm.AcceleratedAt.IsZero())
		}
		if wantEarly && !alarm.WakeAt.Equal(now.Add(time.Minute)) {
			t.Fatalf("accelerated wake=%s", alarm.WakeAt)
		}
	}
	if err := cfg.SetEarlyOnRecovery("codex", false); err != nil {
		t.Fatal(err)
	}
	if err := accelerateRecoveredQuotaAlarms(store, server.URL, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func TestOldScheduledAlarmCanFireWhenExactSessionStillExists(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", root)
	now := time.Now().UTC()
	store := wake.NewStore(filepath.Join(root, "wake.json"))
	if err := wake.NewConfig(filepath.Join(root, "wake-config.json")).SetEnabled("claude", true); err != nil {
		t.Fatal(err)
	}
	_, err := store.Put(wake.Alarm{Agent: "claude", Kind: wake.KindClaudeQuota, SessionID: "old-session", SurfaceID: "old-surface", Action: "continue", Automatic: true, SessionLastActiveAt: now.Add(-5 * 24 * time.Hour), ObservedAt: now.Add(-5 * 24 * time.Hour), WakeAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Hour)}, now)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	cmux := filepath.Join(root, "cmux-fake")
	sessions := `{"sessions":[{"agent":"claude","session_id":"old-session","surface_id":"old-surface","updated_at":"` + now.Add(-5*24*time.Hour).Format(time.RFC3339Nano) + `"}]}`
	script := "#!/bin/sh\nif [ \"$1\" = sessions ]; then printf '%s'; elif [ \"$1\" = read-screen ]; then printf prompt; fi\n"
	script = strings.Replace(script, "%s", sessions, 1)
	if err := os.WriteFile(cmux, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := dispatchDueWakeAlarms(store, server.URL, cmux, 0, now, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	alarms, err := store.List(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(alarms) != 1 || alarms[0].Status != wake.StatusCompleted {
		t.Fatalf("old but bound alarm=%+v", alarms)
	}
}

func TestOldScheduledAlarmDoesNotInterruptManuallyResumedSession(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", root)
	now := time.Now().UTC()
	observedAt := now.Add(-5 * 24 * time.Hour)
	store := wake.NewStore(filepath.Join(root, "wake.json"))
	if err := wake.NewConfig(filepath.Join(root, "wake-config.json")).SetEnabled("claude", true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(wake.Alarm{Agent: "claude", Kind: wake.KindClaudeQuota, SessionID: "old-session", SurfaceID: "old-surface", Action: "continue", Automatic: true, SessionLastActiveAt: observedAt, ObservedAt: observedAt, WakeAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Hour)}, now); err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	cmux := filepath.Join(root, "cmux-fake")
	sessions := `{"sessions":[{"agent":"claude","session_id":"old-session","surface_id":"old-surface","updated_at":"` + observedAt.Add(2*time.Minute).Format(time.RFC3339Nano) + `"}]}`
	script := "#!/bin/sh\nif [ \"$1\" = sessions ]; then printf '%s'; elif [ \"$1\" = read-screen ]; then printf prompt; else exit 0; fi\n"
	if err := os.WriteFile(cmux, []byte(strings.Replace(script, "%s", sessions, 1)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := dispatchDueWakeAlarms(store, server.URL, cmux, 0, now, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	alarms, err := store.List(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(alarms) != 1 || alarms[0].Status != wake.StatusStale || calls != 0 {
		t.Fatalf("manually resumed session alarm=%+v posts=%d", alarms, calls)
	}
}

func TestOldProviderAlarmDoesNotReplayAfterEightHours(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", root)
	now := time.Now().UTC()
	store := wake.NewStore(filepath.Join(root, "wake.json"))
	if err := wake.NewConfig(filepath.Join(root, "wake-config.json")).SetEnabled("codex", true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(wake.Alarm{Agent: "codex", Kind: wake.KindCodexProvider, SessionID: "old-provider", SurfaceID: "old-surface", Action: "/goal resume", Automatic: true, SessionLastActiveAt: now.Add(-9 * time.Hour), ObservedAt: now.Add(-9 * time.Hour), WakeAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Hour)}, now); err != nil {
		t.Fatal(err)
	}
	cmux := filepath.Join(root, "cmux-fake")
	if err := os.WriteFile(cmux, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := dispatchDueWakeAlarms(store, "http://127.0.0.1:1", cmux, 0, now, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	alarms, err := store.List(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(alarms) != 1 || alarms[0].Status != wake.StatusStale {
		t.Fatalf("old provider alarm=%+v", alarms)
	}
}

func TestLaterSuccessfulRequestCancelsQuotaAlarmWithinFirstMinute(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", root)
	now := time.Now().UTC()
	observedAt := now.Add(-5 * time.Minute)
	store := wake.NewStore(filepath.Join(root, "wake.json"))
	if err := wake.NewConfig(filepath.Join(root, "wake-config.json")).SetEnabled("claude", true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(wake.Alarm{Agent: "claude", Kind: wake.KindClaudeQuota, SessionID: "resumed", SurfaceID: "surface", Action: "continue", Automatic: true, SessionLastActiveAt: observedAt, ObservedAt: observedAt, WakeAt: now.Add(5 * 24 * time.Hour), ExpiresAt: now.Add(6 * 24 * time.Hour)}, now); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]recoveryWireState{{Agent: "claude", SessionID: "resumed", Kind: wake.KindClaudeQuota, LastActivityAt: observedAt.Add(30 * time.Second), LastFailureAt: observedAt, LastSuccessAt: observedAt.Add(30 * time.Second)}})
	}))
	defer server.Close()
	cmux := filepath.Join(root, "cmux-fake")
	if err := os.WriteFile(cmux, []byte("#!/bin/sh\nprintf '{\"sessions\":[]}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syncRecoveryAlarms(store, server.URL, cmux, now, true); err != nil {
		t.Fatal(err)
	}
	alarms, err := store.List(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(alarms) != 1 || alarms[0].Status != wake.StatusStale {
		t.Fatalf("resumed session alarm=%+v", alarms)
	}
}

// launchctl print and bootout take one gui/<uid>/<label> argument; passing
// the label separately makes bootout fail and leaves a disabled worker running.
func TestWakeLaunchdServiceTargetIsOneArgument(t *testing.T) {
	want := "gui/" + strconv.Itoa(os.Getuid()) + "/" + wakeLaunchdLabel
	if got := wakeLaunchdServiceTarget(); got != want {
		t.Fatalf("service target = %q, want %q", got, want)
	}
}

// A configured pool server that cannot be resolved is an error, not a reason
// to install a worker that waits on a loopback proxy this machine lacks.
func TestWakeServerURLRefusesUnresolvableServer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SUBROUTER_STATE_DIR", t.TempDir())
	t.Setenv("SUBROUTER_SERVER", "no-such-pool")
	runner := srRunner{program: "sr", store: accounts.DefaultCodexStore()}
	if url, err := runner.wakeServerURL(); err == nil {
		t.Fatalf("wakeServerURL = %q, want an error for an unknown server", url)
	}
	t.Setenv("SUBROUTER_SERVER", "")
	t.Setenv("SUBROUTER_CODEX_SERVER", "")
	if url, err := runner.wakeServerURL(); err != nil || url != defaultWakeServerURL {
		t.Fatalf("with no server configured: url=%q err=%v, want the local proxy", url, err)
	}
}
