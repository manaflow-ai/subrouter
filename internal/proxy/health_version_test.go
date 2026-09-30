package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/buildversion"
)

func TestHealthReportsBuildVersion(t *testing.T) {
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	handler := Server{AccountRef: ref}.Handler()

	req := httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil)
	req.RemoteAddr = "127.0.0.1:4321"
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	var body struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
		Build   struct {
			Version   string `json:"version"`
			Commit    string `json:"commit"`
			BuildDate string `json:"build_date"`
			Mainline  string `json:"mainline"`
		} `json:"build"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health: %v (%s)", err, resp.Body.String())
	}
	if !body.OK {
		t.Fatalf("health ok = false: %s", resp.Body.String())
	}
	if want := buildversion.Version(); body.Version == "" || body.Version != want {
		t.Fatalf("health version = %q, want %q", body.Version, want)
	}
	if body.Build.Version == "" || body.Build.Commit == "" || body.Build.BuildDate == "" || body.Build.Mainline == "" {
		t.Fatalf("health build provenance incomplete: %+v", body.Build)
	}
}

func TestHealthReportsRecoveryCountersAndRolloutMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release-state.json")
	if err := os.WriteFile(path, []byte(`{"version":"v0.1.141","state":"canary","weight":25,"since":"2026-09-30T10:00:00Z","candidate_version":"v0.1.141","incumbent_version":"v0.1.140","last_action":"aborted","last_reason":"proxy 5xx","last_action_at":"2026-09-30T09:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &recoveryCounterStore{}
	store.add(accounts.ProviderCodex, "persistent", time.Now())
	server := Server{ReleaseStatePath: path, recoveryCounters: store}
	var body struct {
		Release  ReleaseState                `json:"release"`
		Recovery map[string]RecoveryCounters `json:"recovery_counters"`
	}
	recorder := httptest.NewRecorder()
	server.handleHealth(recorder, httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil))
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Release.CandidateVersion != "v0.1.141" || body.Release.IncumbentVersion != "v0.1.140" || body.Release.LastReason != "proxy 5xx" {
		t.Fatalf("release metadata = %+v", body.Release)
	}
	if body.Recovery[string(accounts.ProviderCodex)].PersistentRetries != 1 {
		t.Fatalf("recovery counters = %+v", body.Recovery)
	}
}
