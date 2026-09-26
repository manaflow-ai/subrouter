//go:build !windows

package cutovercanary

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChallengeLeaseSIGKILLHelper(t *testing.T) {
	journalPath := os.Getenv("SUBROUTER_TEST_JOURNAL_LOCK")
	journalReady := os.Getenv("SUBROUTER_TEST_JOURNAL_READY")
	if journalPath != "" && journalReady != "" {
		lease, err := acquireJournalLease(journalPath)
		if err != nil {
			os.Exit(30)
		}
		defer lease.Close()
		if err := os.WriteFile(journalReady, []byte("ready\n"), 0o600); err != nil {
			os.Exit(31)
		}
		select {}
	}
	challengePath := os.Getenv("SUBROUTER_TEST_CHALLENGE_PATH")
	readyPath := os.Getenv("SUBROUTER_TEST_CHALLENGE_READY")
	if challengePath == "" || readyPath == "" {
		return
	}
	lease, err := publishChallenge(challengePath, challengePath+".witness", Challenge{Schema: ChallengeSchema, RunID: "killed", Nonce: strings.Repeat("a", 32)})
	if err != nil {
		os.Exit(20)
	}
	defer lease.Close()
	if err := os.WriteFile(readyPath, []byte("ready\n"), 0o600); err != nil {
		os.Exit(21)
	}
	select {}
}

func killJournalLeaseHolder(t *testing.T, journalPath string) {
	t.Helper()
	readyPath := journalPath + ".ready"
	command := exec.Command(os.Args[0], "-test.run=^TestChallengeLeaseSIGKILLHelper$")
	command.Env = append(os.Environ(), "SUBROUTER_TEST_JOURNAL_LOCK="+journalPath, "SUBROUTER_TEST_JOURNAL_READY="+readyPath)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatal("journal lease helper did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("SIGKILL journal helper unexpectedly exited successfully")
	}
}

func TestAuthenticatedRecoversStaleJournalAfterSIGKILL(t *testing.T) {
	dir := privateDir(t)
	server, turns := liveServer(t)
	defer server.Close()
	client, err := newAPIClient(HTTPConfig{BaseURL: server.URL, TimeoutSeconds: 2, MaxResponseBytes: 1 << 16}, false)
	if err != nil {
		t.Fatal(err)
	}
	staleSession := "cutover-canary-" + strings.Repeat("a", 32)
	if _, _, err := client.liveTurn(t.Context(), staleSession, "model", "STALE", "", true); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(dir, "journal.json")
	cfg := RoutedLegConfig{Schema: ConfigSchema, HTTP: HTTPConfig{BaseURL: server.URL, TimeoutSeconds: 2, MaxResponseBytes: 1 << 16}, ProofFile: filepath.Join(dir, "proof.json"), StateFile: filepath.Join(dir, "state.json"), Journal: journalPath, Model: "model"}
	writeConfig(t, dir, "journal.json", cleanupJournal{Schema: JournalSchema, SessionID: staleSession, AgentType: "codex", RunID: "prior-run", ConfigHash: routedHandoffConfigHash(cfg)})
	killJournalLeaseHolder(t, journalPath)
	if err := RunLeg(t.Context(), "authenticated-routed-codex", writeConfig(t, dir, "config.json", cfg), "replacement-run"); err != nil {
		t.Fatal(err)
	}
	if turns.Load() != 3 {
		t.Fatalf("turns=%d, want stale seed plus two replacement turns", turns.Load())
	}
	all, err := client.sessions(t.Context())
	if err != nil || len(all) != 0 {
		t.Fatalf("stale auth session remained: %v / %v", all, err)
	}
}

func TestFailoverRecoversStaleJournalAfterSIGKILL(t *testing.T) {
	dir := privateDir(t)
	fixture := newFailoverFixture(t, 0)
	defer fixture.close()
	staleSession := "cutover-failover-" + strings.Repeat("a", 32)
	fixture.assignments[staleSession] = "dead-account"
	journalPath := filepath.Join(dir, "journal.json")
	cfg := IsolatedLegConfig{Schema: ConfigSchema, HTTP: HTTPConfig{BaseURL: fixture.server.URL, TimeoutSeconds: 2, MaxResponseBytes: 1 << 16}, ProofFile: filepath.Join(dir, "proof.json"), Journal: journalPath, Model: "model", UnavailableAccountID: "dead-account"}
	writeConfig(t, dir, "journal.json", cleanupJournal{Schema: JournalSchema, SessionID: staleSession, AgentType: "codex", RunID: "prior-run", ConfigHash: isolatedCleanupConfigHash(cfg)})
	killJournalLeaseHolder(t, journalPath)
	if err := RunLeg(t.Context(), "safe-failover-reuse", writeConfig(t, dir, "config.json", cfg), "replacement-run"); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.assignments) != 0 {
		t.Fatalf("stale failover session remained: %v", fixture.assignments)
	}
}

func TestChallengeRecoversStaleArtifactAfterSIGKILL(t *testing.T) {
	dir := privateDir(t)
	challengePath := filepath.Join(dir, "challenge.json")
	readyPath := filepath.Join(dir, "ready")
	command := exec.Command(os.Args[0], "-test.run=^TestChallengeLeaseSIGKILLHelper$")
	command.Env = append(os.Environ(), "SUBROUTER_TEST_CHALLENGE_PATH="+challengePath, "SUBROUTER_TEST_CHALLENGE_READY="+readyPath)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatal("challenge lease helper did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("SIGKILL helper unexpectedly exited successfully")
	}

	replacement := Challenge{Schema: ChallengeSchema, RunID: "replacement", Nonce: strings.Repeat("b", 32)}
	lease, err := publishChallenge(challengePath, challengePath+".witness", replacement)
	if err != nil {
		t.Fatalf("recover stale challenge after SIGKILL: %v", err)
	}
	defer lease.Close()
	var got Challenge
	if err := readStrictPrivateJSON(challengePath, &got); err != nil {
		t.Fatal(err)
	}
	if got.RunID != replacement.RunID || got.Nonce != replacement.Nonce {
		t.Fatalf("stale challenge was not replaced: %+v", got)
	}
}
