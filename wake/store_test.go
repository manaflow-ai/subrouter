package wake

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseDurationSupportsDaysAndRejectsGarbage(t *testing.T) {
	got, err := ParseDuration("2d4h15m")
	if err != nil {
		t.Fatal(err)
	}
	want := 2*24*time.Hour + 4*time.Hour + 15*time.Minute
	if got != want {
		t.Fatalf("duration = %s, want %s", got, want)
	}
	for _, input := range []string{"", "-1m", "tomorrow", "2d-1h", "1h30x"} {
		if _, err := ParseDuration(input); err == nil {
			t.Fatalf("ParseDuration(%q) unexpectedly succeeded", input)
		}
	}
}

func TestStoreDeduplicatesAndExpiresAlarms(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "wake.json"))
	now := time.Date(2026, 9, 26, 1, 0, 0, 0, time.UTC)
	alarm := Alarm{
		Agent: "codex", Kind: KindCodexProvider, SessionID: "session-1", SurfaceID: "surface-1",
		Action: "/goal resume", WakeAt: now.Add(time.Minute), ExpiresAt: now.Add(time.Hour),
	}
	first, err := store.Put(alarm, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put(alarm, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("duplicate alarm IDs = %q and %q", first.ID, second.ID)
	}
	alarms, err := store.List(now.Add(2 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(alarms) != 1 || alarms[0].Status != StatusExpired {
		t.Fatalf("alarms = %+v, want one expired alarm", alarms)
	}
}

func TestStoreCancelAgentLeavesOtherAgent(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "wake.json"))
	now := time.Now().UTC()
	for _, agent := range []string{"codex", "claude"} {
		_, err := store.Put(Alarm{
			Agent: agent, Kind: map[string]string{"codex": KindCodexQuota, "claude": KindClaudeQuota}[agent], SessionID: agent + "-session", SurfaceID: agent + "-surface",
			Action: "continue", WakeAt: now.Add(time.Hour), ExpiresAt: now.Add(2 * time.Hour),
		}, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	if count, err := store.CancelAgent("claude", now); err != nil || count != 1 {
		t.Fatalf("CancelAgent = %d, %v; want 1/nil", count, err)
	}
	alarms, err := store.List(now)
	if err != nil {
		t.Fatal(err)
	}
	for _, alarm := range alarms {
		if alarm.Agent == "claude" && alarm.Status != StatusCancelled {
			t.Fatalf("Claude alarm = %+v, want cancelled", alarm)
		}
		if alarm.Agent == "codex" && alarm.Status != StatusScheduled {
			t.Fatalf("Codex alarm = %+v, want scheduled", alarm)
		}
	}
}

func TestConfigDisabledByDefaultAndPersists(t *testing.T) {
	cfg := NewConfig(filepath.Join(t.TempDir(), "wake-config.json"))
	if early, err := cfg.EarlyOnRecovery("codex"); err != nil || !early {
		t.Fatalf("default early recovery=%v err=%v", early, err)
	}
	if enabled, err := cfg.Enabled("codex"); err != nil || enabled {
		t.Fatalf("default enabled=%v err=%v, want false", enabled, err)
	}
	if err := cfg.SetEarlyOnRecovery("codex", false); err != nil {
		t.Fatal(err)
	}
	if early, err := cfg.EarlyOnRecovery("codex"); err != nil || early {
		t.Fatalf("disabled early recovery=%v err=%v", early, err)
	}
	if early, err := cfg.EarlyOnRecovery("claude"); err != nil || !early {
		t.Fatalf("Claude early recovery=%v err=%v", early, err)
	}
	if err := cfg.SetEnabled("claude", true); err != nil {
		t.Fatal(err)
	}
	if enabled, err := cfg.Enabled("claude"); err != nil || !enabled {
		t.Fatalf("claude enabled=%v err=%v, want true", enabled, err)
	}
	if enabled, err := cfg.Enabled("codex"); err != nil || enabled {
		t.Fatalf("codex enabled=%v err=%v, want false", enabled, err)
	}
	policy, err := cfg.Policy("codex")
	if err != nil || policy.AllowContinue || policy.MaxGoalAttempts != 2 {
		t.Fatalf("default policy=%+v err=%v", policy, err)
	}
	policy.AllowContinue = true
	if err := cfg.SetPolicy("codex", policy); err != nil {
		t.Fatal(err)
	}
	if saved, err := cfg.Policy("codex"); err != nil || !saved.AllowContinue {
		t.Fatalf("saved policy=%+v err=%v", saved, err)
	}
}

func TestEarlyRecoveryDefaultsOnForExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wake-config.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"enabled":{"codex":true,"claude":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := NewConfig(path)
	for _, agent := range []string{"codex", "claude"} {
		if early, err := cfg.EarlyOnRecovery(agent); err != nil || !early {
			t.Fatalf("legacy config %s early=%v err=%v", agent, early, err)
		}
	}
}

func TestEligibleForAutomaticRejectsStaleInitialSessions(t *testing.T) {
	now := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	if !EligibleForAutomatic(now.Add(-7*time.Hour), now, true, false) {
		t.Fatal("recent initial session was rejected")
	}
	if EligibleForAutomatic(now.Add(-9*time.Hour), now, true, true) {
		t.Fatal("stale initial session was accepted")
	}
	if !EligibleForAutomatic(now.Add(-3*24*time.Hour), now, false, true) {
		t.Fatal("confirmed monitor event was rejected")
	}
	if EligibleForAutomatic(now.Add(-3*24*time.Hour), now, false, false) {
		t.Fatal("unconfirmed monitor event was accepted")
	}
}
