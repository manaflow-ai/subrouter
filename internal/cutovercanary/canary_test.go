package cutovercanary

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/session"
)

func privateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeConfig(t *testing.T, dir, name string, value any) string {
	t.Helper()
	path := filepath.Join(dir, name)
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitForChallengeReady(t *testing.T, challenge Challenge) {
	t.Helper()
	notBefore, err := time.Parse(time.RFC3339Nano, challenge.NotBefore)
	if err != nil {
		t.Fatal(err)
	}
	if delay := time.Until(notBefore) + 10*time.Millisecond; delay > 0 {
		time.Sleep(delay)
	}
}

func liveServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var mu sync.Mutex
	assignments := map[string]string{}
	var turns atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_subrouter/health":
			io.WriteString(w, `{"ok":true}`)
		case "/_subrouter/ready":
			io.WriteString(w, `{"ok":true,"draining":false}`)
		case "/_subrouter/sessions":
			mu.Lock()
			defer mu.Unlock()
			if r.Method == http.MethodDelete {
				delete(assignments, r.URL.Query().Get("session_id"))
				w.WriteHeader(http.StatusNoContent)
				return
			}
			inactive := false
			all := []sessionStatus{}
			for id, account := range assignments {
				all = append(all, sessionStatus{Assignment: session.Assignment{AgentType: "codex", SessionID: id, AccountID: account}, Active: &inactive})
			}
			json.NewEncoder(w).Encode(all)
		case "/v1/responses":
			if r.Header.Get("X-Subrouter-No-Retry") != "true" {
				t.Errorf("missing no-retry header")
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			marker := strings.TrimPrefix(canaryResponsePrompt(body), "Reply with exactly ")
			mu.Lock()
			assignments[r.Header.Get("X-Subrouter-Session")] = "account-secret"
			turns.Add(1)
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", marker)
		default:
			http.NotFound(w, r)
		}
	}))
	return server, &turns
}

func canaryResponsePrompt(body map[string]any) string {
	input, ok := body["input"].([]any)
	if !ok || len(input) != 1 {
		return ""
	}
	message, ok := input[0].(map[string]any)
	if !ok {
		return ""
	}
	content, ok := message["content"].([]any)
	if !ok || len(content) != 1 {
		return ""
	}
	text, ok := content[0].(map[string]any)
	if !ok {
		return ""
	}
	value, _ := text["text"].(string)
	return value
}

func TestAuthenticatedClaudeCanaryRoutesAndCleansExactSession(t *testing.T) {
	dir := privateDir(t)
	assignments := map[string]string{}
	var mu sync.Mutex
	var observedSession string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/messages":
			if r.Header.Get("Authorization") != "Bearer subrouter" ||
				r.Header.Get("anthropic-beta") != "claude-code-20250219,oauth-2025-04-20" ||
				r.Header.Get("anthropic-version") != "2023-06-01" ||
				r.Header.Get("User-Agent") != "claude-cli/2.1.199 (external, cli)" ||
				r.Header.Get("x-app") != "cli" ||
				r.Header.Get("X-Subrouter-Agent") != "claude" ||
				r.Header.Get("X-Subrouter-No-Retry") != "true" {
				t.Error("Claude canary request headers do not match the routed CLI shape")
			}
			observedSession = r.Header.Get("X-Claude-Code-Session-Id")
			var body struct {
				Model     string `json:"model"`
				MaxTokens int    `json:"max_tokens"`
				System    []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"system"`
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "claude-test" ||
				body.MaxTokens != 256 || len(body.Messages) != 1 || body.Messages[0].Role != "user" {
				t.Error("Claude canary request body is invalid")
			}
			if len(body.System) != 1 || body.System[0].Type != "text" ||
				body.System[0].Text != "You are Claude Code, Anthropic's official CLI for Claude." {
				t.Error("Claude canary request does not carry the Claude Code system identity")
			}
			marker := strings.TrimPrefix(body.Messages[0].Content, "Reply with exactly ")
			mu.Lock()
			assignments[observedSession] = "claude-account-secret"
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"type":"message","content":[{"type":"text","text":%q}],"stop_reason":"end_turn"}`, marker)
		case "/_subrouter/sessions":
			mu.Lock()
			defer mu.Unlock()
			if r.Method == http.MethodDelete {
				delete(assignments, r.URL.Query().Get("session_id"))
				w.WriteHeader(http.StatusNoContent)
				return
			}
			all := make([]session.Assignment, 0, len(assignments))
			for id, account := range assignments {
				all = append(all, session.Assignment{AgentType: "claude", SessionID: id, AccountID: account})
			}
			_ = json.NewEncoder(w).Encode(all)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := ClaudeLegConfig{
		Schema: ConfigSchema, HTTP: HTTPConfig{BaseURL: server.URL, TimeoutSeconds: 2, MaxResponseBytes: 1 << 16},
		ProofFile: filepath.Join(dir, "proof.json"), Journal: filepath.Join(dir, "journal.json"), Model: "claude-test",
	}
	path := writeConfig(t, dir, "config.json", cfg)
	if err := RunLeg(context.Background(), "authenticated-routed-claude", path, "test-run"); err != nil {
		t.Fatal(err)
	}
	if !validCanarySessionID(observedSession, "cutover-claude-") {
		t.Fatalf("Claude canary session ID = %q", observedSession)
	}
	mu.Lock()
	remaining := len(assignments)
	mu.Unlock()
	if remaining != 0 {
		t.Fatal("Claude canary assignment was not cleaned")
	}
	if _, err := os.Stat(cfg.Journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Claude canary cleanup journal remained")
	}
	proofBody, err := os.ReadFile(cfg.ProofFile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(proofBody, []byte("claude-account-secret")) || bytes.Contains(proofBody, []byte(observedSession)) {
		t.Fatal("Claude canary proof leaked raw account or session identity")
	}
}

func TestAuthenticatedClaudeCanaryRejectsWrongMarkerAndCleans(t *testing.T) {
	dir := privateDir(t)
	deleted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/messages":
			_, _ = io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"wrong"}]}`)
		case "/_subrouter/sessions":
			if r.Method == http.MethodDelete {
				deleted = true
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_, _ = io.WriteString(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg := ClaudeLegConfig{Schema: ConfigSchema, HTTP: HTTPConfig{BaseURL: server.URL, TimeoutSeconds: 2, MaxResponseBytes: 1 << 16}, ProofFile: filepath.Join(dir, "proof.json"), Journal: filepath.Join(dir, "journal.json"), Model: "claude-test"}
	path := writeConfig(t, dir, "config.json", cfg)
	if err := RunLeg(context.Background(), "authenticated-routed-claude", path, "test-run"); err == nil {
		t.Fatal("wrong Claude marker was accepted")
	}
	if !deleted {
		t.Fatal("failed Claude canary did not attempt session cleanup")
	}
	if _, err := os.Stat(cfg.ProofFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed Claude canary wrote success proof")
	}
}

func TestExactClaudeMarkerResponseAllowsInternalThinkingOnly(t *testing.T) {
	if !exactClaudeMarkerResponse([]byte(`{"type":"message","content":[{"type":"thinking","thinking":"private","signature":"opaque"},{"type":"text","text":"MARK"}]}`), "MARK") {
		t.Fatal("Claude marker with an internal thinking block was rejected")
	}
	for _, body := range []string{
		`{"type":"message","content":[{"type":"text","text":"MARK"},{"type":"text","text":"EXTRA"}]}`,
		`{"type":"message","content":[{"type":"tool_use","name":"unexpected"},{"type":"text","text":"MARK"}]}`,
		`{"type":"message","content":[{"type":"thinking","thinking":"private"}]}`,
	} {
		if exactClaudeMarkerResponse([]byte(body), "MARK") {
			t.Fatalf("unsafe Claude response was accepted: %s", body)
		}
	}
}

func TestAuthenticatedAndStickyCoordinateAndClean(t *testing.T) {
	dir := privateDir(t)
	server, turns := liveServer(t)
	defer server.Close()
	cfg := RoutedLegConfig{Schema: ConfigSchema, HTTP: HTTPConfig{BaseURL: server.URL, TimeoutSeconds: 2, MaxResponseBytes: 1 << 16}, ProofFile: filepath.Join(dir, "auth-proof.json"), StateFile: filepath.Join(dir, "state.json"), Journal: filepath.Join(dir, "journal.json"), Model: "test-model"}
	path := writeConfig(t, dir, "auth.json", cfg)
	if err := RunLeg(context.Background(), "authenticated-routed-codex", path, "test-run"); err != nil {
		t.Fatal(err)
	}
	stateBytes, err := os.ReadFile(cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stateBytes), "account-secret") || strings.Contains(string(stateBytes), `"session_id"`) || strings.Contains(string(stateBytes), `"account_id"`) {
		t.Fatal("sanitized handoff leaked raw assignment")
	}
	var privateState liveState
	if err := json.Unmarshal(stateBytes, &privateState); err != nil {
		t.Fatal(err)
	}
	if len(privateState.SessionHash) != 64 || len(privateState.AccountHash) != 64 {
		t.Fatal("sanitized handoff hashes invalid")
	}
	authProof, err := os.ReadFile(cfg.ProofFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(authProof), "account-secret") || strings.Contains(string(authProof), `"session_id"`) || strings.Contains(string(authProof), `"account_id"`) {
		t.Fatal("proof leaked raw identity")
	}
	if turns.Load() != 2 {
		t.Fatalf("authenticated turns=%d, want 2", turns.Load())
	}
	if _, err := os.Stat(cfg.Journal); !os.IsNotExist(err) {
		t.Fatal("authenticated leg left cleanup journal")
	}
	client, err := newAPIClient(cfg.HTTP, false)
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := client.sessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("authenticated leg left server assignments: %v", remaining)
	}
	stateBefore, err := os.ReadFile(cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := RunLeg(context.Background(), "authenticated-routed-codex", path, "second-run"); !errors.Is(err, errStateActive) {
		t.Fatalf("concurrent auth replaced pending handoff: %v", err)
	}
	stateAfter, err := os.ReadFile(cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stateBefore, stateAfter) || turns.Load() != 2 {
		t.Fatal("pending auth-to-sticky handoff was mutated")
	}
	cfg.ProofFile = filepath.Join(dir, "sticky-proof.json")
	path = writeConfig(t, dir, "sticky.json", cfg)
	if err := RunLeg(context.Background(), "sticky-reuse", path, "other-run"); err == nil {
		t.Fatal("cross-run handoff accepted")
	}
	if turns.Load() != 2 {
		t.Fatal("cross-run handoff sent provider traffic")
	}
	wrongConfig := cfg
	wrongConfig.Model = "different-model"
	wrongConfig.ProofFile = filepath.Join(dir, "wrong-config-proof.json")
	if err := RunLeg(context.Background(), "sticky-reuse", writeConfig(t, dir, "wrong-config.json", wrongConfig), "test-run"); err == nil {
		t.Fatal("different config consumed authenticated handoff")
	}
	if afterWrong, err := os.ReadFile(cfg.StateFile); err != nil || !bytes.Equal(afterWrong, stateBefore) {
		t.Fatal("different config mutated authenticated handoff")
	}
	if err := RunLeg(context.Background(), "sticky-reuse", path, "test-run"); err != nil {
		t.Fatal(err)
	}
	if turns.Load() != 2 {
		t.Fatalf("turns=%d, want 2", turns.Load())
	}
	for _, artifact := range []string{cfg.StateFile, cfg.Journal} {
		if _, err := os.Stat(artifact); !os.IsNotExist(err) {
			t.Fatalf("artifact not cleaned: %s", artifact)
		}
	}
}

func TestAuthenticatedRejectsArbitraryCrashJournalWithoutMutation(t *testing.T) {
	dir := privateDir(t)
	server, turns := liveServer(t)
	defer server.Close()
	journal := filepath.Join(dir, "journal.json")
	original := []byte(`{"schema":"subrouter.cutover-canary-cleanup-journal/v1","session_id":"real-user-session","agent_type":"codex","run_id":"prior-run"}`)
	if err := os.WriteFile(journal, original, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := RoutedLegConfig{Schema: ConfigSchema, HTTP: HTTPConfig{BaseURL: server.URL, TimeoutSeconds: 2, MaxResponseBytes: 1 << 16}, ProofFile: filepath.Join(dir, "proof.json"), StateFile: filepath.Join(dir, "state.json"), Journal: journal, Model: "test-model"}
	path := writeConfig(t, dir, "auth.json", cfg)
	if err := RunLeg(context.Background(), "authenticated-routed-codex", path, "test-run"); err == nil {
		t.Fatal("arbitrary crash journal accepted")
	}
	if turns.Load() != 0 {
		t.Fatal("arbitrary crash journal caused provider traffic")
	}
	got, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("arbitrary crash journal was mutated")
	}
}

func TestAuthenticatedPreservesJournalFromDifferentRoutingConfig(t *testing.T) {
	dir := privateDir(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer server.Close()
	journalPath := filepath.Join(dir, "journal.json")
	current := RoutedLegConfig{Schema: ConfigSchema, HTTP: HTTPConfig{BaseURL: server.URL, TimeoutSeconds: 2, MaxResponseBytes: 1 << 16}, ProofFile: filepath.Join(dir, "proof.json"), StateFile: filepath.Join(dir, "state.json"), Journal: journalPath, Model: "model"}
	prior := current
	prior.HTTP.BaseURL = "https://prior.invalid"
	journal := cleanupJournal{Schema: JournalSchema, SessionID: "cutover-canary-" + strings.Repeat("a", 32), AgentType: "codex", RunID: "prior-run", ConfigHash: routedHandoffConfigHash(prior)}
	writeConfig(t, dir, "journal.json", journal)
	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := RunLeg(context.Background(), "authenticated-routed-codex", writeConfig(t, dir, "config.json", current), "current-run"); err == nil {
		t.Fatal("journal from a different routing config was accepted")
	}
	after, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || requests.Load() != 0 {
		t.Fatal("mismatched routing journal was mutated or used against the current service")
	}
}

func TestFailoverPreservesJournalFromDifferentRoutingConfig(t *testing.T) {
	dir := privateDir(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer server.Close()
	journalPath := filepath.Join(dir, "journal.json")
	current := IsolatedLegConfig{Schema: ConfigSchema, HTTP: HTTPConfig{BaseURL: server.URL, TimeoutSeconds: 2, MaxResponseBytes: 1 << 16}, ProofFile: filepath.Join(dir, "proof.json"), Journal: journalPath, Model: "model", UnavailableAccountID: "dead-account"}
	prior := current
	prior.HTTP.BaseURL = "https://prior.invalid"
	journal := cleanupJournal{Schema: JournalSchema, SessionID: "cutover-failover-" + strings.Repeat("a", 32), AgentType: "codex", RunID: "prior-run", ConfigHash: isolatedCleanupConfigHash(prior)}
	writeConfig(t, dir, "journal.json", journal)
	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := RunLeg(context.Background(), "safe-failover-reuse", writeConfig(t, dir, "config.json", current), "current-run"); err == nil {
		t.Fatal("failover journal from a different routing config was accepted")
	}
	after, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || requests.Load() != 0 {
		t.Fatal("mismatched failover journal was mutated or used against the current service")
	}
}

func TestLiveJournalLeaseRejectsConcurrentCoordinatorWithoutMutation(t *testing.T) {
	for _, test := range []struct {
		name      string
		leg       string
		prefix    string
		configure func(string, string, string) any
	}{
		{
			name:   "authenticated",
			leg:    "authenticated-routed-codex",
			prefix: "cutover-canary-",
			configure: func(dir, serverURL, journal string) any {
				return RoutedLegConfig{Schema: ConfigSchema, HTTP: HTTPConfig{BaseURL: serverURL, TimeoutSeconds: 1, MaxResponseBytes: 1024}, ProofFile: filepath.Join(dir, "proof.json"), StateFile: filepath.Join(dir, "state.json"), Journal: journal, Model: "model"}
			},
		},
		{
			name:   "failover",
			leg:    "safe-failover-reuse",
			prefix: "cutover-failover-",
			configure: func(dir, serverURL, journal string) any {
				return IsolatedLegConfig{Schema: ConfigSchema, HTTP: HTTPConfig{BaseURL: serverURL, TimeoutSeconds: 1, MaxResponseBytes: 1024}, ProofFile: filepath.Join(dir, "proof.json"), Journal: journal, Model: "model", UnavailableAccountID: "unavailable"}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := privateDir(t)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				http.Error(w, "unexpected", http.StatusInternalServerError)
			}))
			defer server.Close()
			journalPath := filepath.Join(dir, "journal.json")
			lease, err := acquireJournalLease(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			configured := test.configure(dir, server.URL, journalPath)
			var configHash string
			switch cfg := configured.(type) {
			case RoutedLegConfig:
				configHash = routedHandoffConfigHash(cfg)
			case IsolatedLegConfig:
				configHash = isolatedCleanupConfigHash(cfg)
			default:
				t.Fatal("unsupported test configuration")
			}
			journal := cleanupJournal{Schema: JournalSchema, SessionID: test.prefix + strings.Repeat("a", 32), AgentType: "codex", RunID: "live-run", ConfigHash: configHash}
			writeConfig(t, dir, "journal.json", journal)
			before, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			configPath := writeConfig(t, dir, "config.json", configured)
			err = RunLeg(context.Background(), test.leg, configPath, "other-run")
			if !errors.Is(err, errJournalActive) {
				t.Fatalf("concurrent coordinator error=%v", err)
			}
			after, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("live cleanup journal was mutated")
			}
			if requests.Load() != 0 {
				t.Fatalf("concurrent coordinator sent %d requests", requests.Load())
			}
		})
	}
}

func TestArtifactLeaseRejectsUnsafeDerivedLock(t *testing.T) {
	dir := privateDir(t)
	journalPath := filepath.Join(dir, "journal.json")
	lockPath := journalPath + ".lock"
	if runtime.GOOS != "windows" {
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, lockPath); err != nil {
			t.Fatal(err)
		}
		if _, err := acquireJournalLease(journalPath); err == nil {
			t.Fatal("symlinked derived journal lock accepted")
		}
		if err := os.Remove(lockPath); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireJournalLease(journalPath); err == nil {
		t.Fatal("non-private derived journal lock accepted")
	}
}

func TestProbeFailsClosedOnDraining(t *testing.T) {
	dir := privateDir(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "health") {
			io.WriteString(w, `{"ok":true}`)
			return
		}
		io.WriteString(w, `{"ok":true,"draining":true}`)
	}))
	defer server.Close()
	path := writeConfig(t, dir, "probe.json", PeerProbeConfig{Schema: ConfigSchema, HTTP: HTTPConfig{BaseURL: server.URL, TimeoutSeconds: 2, MaxResponseBytes: 1024}})
	if _, err := Probe(context.Background(), path); err == nil {
		t.Fatal("draining readiness accepted")
	}
}

func TestPeerCommandAcceptsOnlyStrictHealthyProof(t *testing.T) {
	dir := privateDir(t)
	digest := strings.Repeat("a", 64)
	identityFile := filepath.Join(dir, "identity")
	if err := os.WriteFile(identityFile, []byte("test identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeSSH := filepath.Join(dir, "ssh")
	argvFile := filepath.Join(dir, "argv")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" >%q\ncase \"$*\" in *draining) draining=true ;; *) draining=false ;; esac\nprintf '{\"schema\":\"%s\",\"ok\":true,\"health_ok\":true,\"ready_ok\":true,\"draining\":%%s,\"identity_kind\":\"%s\",\"executable_identity\":\"%s\"}\\n' \"$draining\"\n", argvFile, PeerProbeSchema, goBuildInfoIdentityKind, digest)
	if err := os.WriteFile(fakeSSH, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	previousSSH := sshCommandPath
	sshCommandPath = fakeSSH
	defer func() { sshCommandPath = previousSSH }()
	peer := PeerTarget{Name: "peer-a", SSHHost: "peer.example", SSHIdentityFile: identityFile, RemoteExecutable: "/private/subrouter-cutover-canary", RemoteConfigFile: "/private/healthy", ExpectedIdentityKind: goBuildInfoIdentityKind, ExpectedExecutableIdentity: digest, TimeoutSeconds: 10}
	if err := runPeer(context.Background(), peer); err != nil {
		t.Fatal(err)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"-F\nnone\n", "ControlMaster=no", "ControlPersist=no", "ForkAfterAuthentication=no", "ProxyCommand=none", "ProxyJump=none", "IdentitiesOnly=yes", "IdentityAgent=none", "-i\n" + identityFile + "\n"} {
		if !strings.Contains(string(argv), required) {
			t.Fatalf("peer ssh argv omitted %q: %s", required, argv)
		}
	}
	peerB := peer
	peerB.Name = "peer-b"
	configPath := writeConfig(t, dir, "shared-identity.json", PeerLegConfig{
		Schema: ConfigSchema, ProofFile: filepath.Join(dir, "shared-identity-proof.json"), Peers: []PeerTarget{peer, peerB},
	})
	if err := RunLeg(t.Context(), "peer-health-readiness", configPath, "shared-identity"); err != nil {
		t.Fatalf("two peers sharing one SSH identity rejected: %v", err)
	}
	peer.RemoteConfigFile = "/private/draining"
	if err := runPeer(context.Background(), peer); err == nil {
		t.Fatal("draining peer accepted")
	}
	peer.RemoteConfigFile = "/private/healthy"
	peer.SSHHost = "-oProxyCommand=bad"
	if err := runPeer(context.Background(), peer); err == nil {
		t.Fatal("option-like peer host accepted")
	}
	peer.SSHHost = "peer.example"
	peer.RemoteExecutable = "/private/helper with spaces"
	if err := runPeer(context.Background(), peer); err == nil {
		t.Fatal("shell-bearing remote path accepted")
	}
	peer.RemoteExecutable = "/private/subrouter-cutover-canary"
	if err := os.Chmod(identityFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runPeer(context.Background(), peer); err == nil {
		t.Fatal("group/world-readable SSH identity accepted")
	}
}

func TestStrictPrivateConfigRejectsUnknownAndSymlink(t *testing.T) {
	dir := privateDir(t)
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte(`{"schema":"subrouter.cutover-canary-config/v1","proof_file":"/tmp/x","unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var cfg IsolatedLegConfig
	if err := readStrictPrivateJSON(path, &cfg); err == nil {
		t.Fatal("unknown field accepted")
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := readStrictPrivateJSON(link, &cfg); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestStrictPrivateConfigRejectsDuplicateKeysAtEveryDepth(t *testing.T) {
	dir := privateDir(t)
	for name, body := range map[string]string{
		"top":    `{"schema":"subrouter.cutover-canary-config/v1","schema":"subrouter.cutover-canary-config/v1","proof_file":"/tmp/x"}`,
		"nested": `{"schema":"subrouter.cutover-canary-config/v1","http":{"base_url":"http://127.0.0.1","base_url":"http://127.0.0.1"},"proof_file":"/tmp/x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			var cfg RoutedLegConfig
			if err := readStrictPrivateJSON(path, &cfg); err == nil {
				t.Fatal("duplicate key accepted")
			}
		})
	}
}

func TestWritableArtifactsRejectReadOnlyPathAndInodeAliases(t *testing.T) {
	t.Run("proof equals config", func(t *testing.T) {
		dir := privateDir(t)
		configPath := filepath.Join(dir, "config.json")
		cfg := PeerLegConfig{Schema: ConfigSchema, ProofFile: configPath, Peers: []PeerTarget{{}}}
		writeConfig(t, dir, "config.json", cfg)
		before, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := RunLeg(t.Context(), "peer-health-readiness", configPath, "test-run"); err == nil || !strings.Contains(err.Error(), "distinct") {
			t.Fatalf("config alias error=%v", err)
		}
		after, err := os.ReadFile(configPath)
		if err != nil || !bytes.Equal(after, before) {
			t.Fatalf("config mutated: %v", err)
		}
	})

	t.Run("proof equals admin token", func(t *testing.T) {
		dir := privateDir(t)
		tokenPath := filepath.Join(dir, "admin-token")
		if err := os.WriteFile(tokenPath, []byte("secret-token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := RoutedLegConfig{
			Schema:    ConfigSchema,
			HTTP:      HTTPConfig{BaseURL: "http://127.0.0.1:1", AdminTokenFile: tokenPath, TimeoutSeconds: 1, MaxResponseBytes: 256},
			ProofFile: tokenPath, StateFile: filepath.Join(dir, "state.json"), Journal: filepath.Join(dir, "journal.json"), Model: "model",
		}
		configPath := writeConfig(t, dir, "config.json", cfg)
		if err := RunLeg(t.Context(), "authenticated-routed-codex", configPath, "test-run"); err == nil || !strings.Contains(err.Error(), "distinct") {
			t.Fatalf("admin-token alias error=%v", err)
		}
		if got, err := os.ReadFile(tokenPath); err != nil || string(got) != "secret-token\n" {
			t.Fatalf("admin token mutated: %q / %v", got, err)
		}
	})

	t.Run("proof equals peer SSH identity", func(t *testing.T) {
		dir := privateDir(t)
		identityPath := filepath.Join(dir, "identity")
		if err := os.WriteFile(identityPath, []byte("test identity"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := PeerLegConfig{
			Schema: ConfigSchema, ProofFile: identityPath,
			Peers: []PeerTarget{{Name: "peer-a", SSHHost: "peer.example", SSHIdentityFile: identityPath, RemoteExecutable: "/private/helper", RemoteConfigFile: "/private/config", ExpectedIdentityKind: goBuildInfoIdentityKind, ExpectedExecutableIdentity: strings.Repeat("a", 64), TimeoutSeconds: 1}},
		}
		configPath := writeConfig(t, dir, "config.json", cfg)
		before, err := os.ReadFile(identityPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := RunLeg(t.Context(), "peer-health-readiness", configPath, "test-run"); err == nil || !strings.Contains(err.Error(), "distinct") {
			t.Fatalf("SSH identity alias error=%v", err)
		}
		after, err := os.ReadFile(identityPath)
		if err != nil || !bytes.Equal(after, before) {
			t.Fatalf("SSH identity mutated: %q / %v", after, err)
		}
	})

	t.Run("proof equals candidate log", func(t *testing.T) {
		dir := privateDir(t)
		logPath := filepath.Join(dir, "candidate.log")
		if err := os.WriteFile(logPath, []byte("original\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		selection := writeConfig(t, dir, "selection.json", Selection{Schema: "subrouter.cutover-canary-selection/v1", AgentType: "codex", SessionID: "selected"})
		cfg := ExistingLegConfig{
			Schema: ConfigSchema, HTTP: HTTPConfig{BaseURL: "http://127.0.0.1:1", TimeoutSeconds: 1, MaxResponseBytes: 256},
			ProofFile: logPath, SelectionFile: selection, ChallengeFile: filepath.Join(dir, "challenge.json"), WitnessFile: filepath.Join(dir, "witness.json"),
			WaitSeconds: 1, CandidateLogFiles: []string{logPath}, MaxLogAppendBytes: 256,
		}
		configPath := writeConfig(t, dir, "config.json", cfg)
		if err := RunLeg(t.Context(), "existing-session-next-turn", configPath, "test-run"); err == nil || !strings.Contains(err.Error(), "distinct") {
			t.Fatalf("candidate-log alias error=%v", err)
		}
		if got, err := os.ReadFile(logPath); err != nil || string(got) != "original\n" {
			t.Fatalf("candidate log mutated: %q / %v", got, err)
		}
	})

	t.Run("proof hardlinks config", func(t *testing.T) {
		dir := privateDir(t)
		configPath := writeConfig(t, dir, "config.json", PeerLegConfig{Schema: ConfigSchema, ProofFile: filepath.Join(dir, "proof.json"), Peers: []PeerTarget{{}}})
		proofPath := filepath.Join(dir, "proof.json")
		if err := os.Link(configPath, proofPath); err != nil {
			t.Fatal(err)
		}
		if err := RunLeg(t.Context(), "peer-health-readiness", configPath, "test-run"); err == nil || !strings.Contains(err.Error(), "distinct") {
			t.Fatalf("hardlink alias error=%v", err)
		}
	})
}

func TestMarkerResponseRequiresMarkerAndCompletion(t *testing.T) {
	if !markerResponse([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"MARK\"}\n\ndata: {\"type\":\"response.completed\"}\n\n"), "MARK") {
		t.Fatal("valid response rejected")
	}
	if markerResponse([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"MARK\"}\n\n"), "MARK") {
		t.Fatal("incomplete response accepted")
	}
	if markerResponse([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"MARK EXTRA\"}\n\ndata: {\"type\":\"response.completed\"}\n\n"), "MARK") {
		t.Fatal("extra output accepted")
	}
	if markerResponse([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"MARK\"}\n\ndata: {\"type\":\"response.completed\"}\n\ndata: {\"type\":\"response.completed\"}\n\n"), "MARK") {
		t.Fatal("duplicate completion accepted")
	}
	if markerResponse([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"MARK\"}\n\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"MARK EXTRA\"}]}}\n\ndata: {\"type\":\"response.completed\"}\n\n"), "MARK") {
		t.Fatal("contradictory final output accepted")
	}
}

func TestQuotaFailureResponseRequiresStructuredQuotaEvidence(t *testing.T) {
	accepted := []string{
		`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`,
		`{"error":{"type":"usage_limit_reached"}}`,
		`{"type":"usage_limit_reached"}`,
		`{"error":"You have exceeded your usage limit."}`,
		`{"type":"response.failed","response":{"error":{"code":"insufficient_quota"}}}`,
		"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"You have hit your usage limit.\"}}}\n\n",
	}
	for _, body := range accepted {
		if !quotaFailureResponse(http.StatusTooManyRequests, []byte(body)) {
			t.Errorf("structured quota failure rejected: %s", body)
		}
	}

	rejected := []string{
		`{"error":{"message":"usage limit status unavailable"}}`,
		`{"type":"response.output_text.delta","delta":"usage limit reached"}`,
		`{"message":"usage limit reached"}`,
		`{"error":{"message":"server unavailable"},"metadata":"usage_limit_reached"}`,
	}
	for _, body := range rejected {
		if quotaFailureResponse(http.StatusTooManyRequests, []byte(body)) {
			t.Errorf("incidental quota text accepted: %s", body)
		}
	}
	if quotaFailureResponse(http.StatusInternalServerError, []byte(accepted[0])) {
		t.Error("quota payload accepted on non-quota status")
	}
}

func TestCodexCanaryUsesBackendCompatibleRequestShape(t *testing.T) {
	body, err := responseRequest("model", "MARK")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	if _, present := request["max_output_tokens"]; present {
		t.Fatalf("unsupported max_output_tokens present: %v", request["max_output_tokens"])
	}
	if store, ok := request["store"].(bool); !ok || store {
		t.Fatalf("store=%v, want false", request["store"])
	}
	if stream, ok := request["stream"].(bool); !ok || !stream {
		t.Fatalf("stream=%v, want true", request["stream"])
	}
	input, ok := request["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input=%#v, want one-item Responses API input list", request["input"])
	}
	message, ok := input[0].(map[string]any)
	if !ok || message["type"] != "message" || message["role"] != "user" {
		t.Fatalf("input message=%#v", input[0])
	}
	content, ok := message["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("input content=%#v", message["content"])
	}
	text, ok := content[0].(map[string]any)
	if !ok || text["type"] != "input_text" || text["text"] != "Reply with exactly MARK" {
		t.Fatalf("input text=%#v", content[0])
	}
}

func TestFailedSessionCleanupPreservesJournal(t *testing.T) {
	dir := privateDir(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client, err := newAPIClient(HTTPConfig{BaseURL: server.URL, TimeoutSeconds: 1, MaxResponseBytes: 1024}, false)
	if err != nil {
		t.Fatal(err)
	}
	state := writeConfig(t, dir, "state.json", map[string]string{"private": "state"})
	expected := cleanupJournal{Schema: JournalSchema, SessionID: "cutover-canary-" + strings.Repeat("a", 32), AgentType: "codex", RunID: "test-run", ConfigHash: strings.Repeat("c", 64)}
	journal := writeConfig(t, dir, "journal.json", expected)
	if err := cleanupLiveArtifacts(context.Background(), client, state, journal, expected); err == nil {
		t.Fatal("failed delete accepted")
	}
	for _, path := range []string{state, journal} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("recovery artifact removed: %s", path)
		}
	}
}

func TestSessionCleanupNeverRemovesReplacementJournal(t *testing.T) {
	dir := privateDir(t)
	journalPath := filepath.Join(dir, "journal.json")
	expected := cleanupJournal{Schema: JournalSchema, SessionID: "cutover-failover-" + strings.Repeat("a", 32), AgentType: "codex", RunID: "owner-run", ConfigHash: strings.Repeat("c", 64)}
	replacement := cleanupJournal{Schema: JournalSchema, SessionID: "cutover-failover-" + strings.Repeat("b", 32), AgentType: "codex", RunID: "replacement-run", ConfigHash: strings.Repeat("d", 64)}
	writeConfig(t, dir, "journal.json", expected)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
			return
		}
		if err := writePrivateJSON(journalPath, replacement); err != nil {
			t.Errorf("replace journal: %v", err)
			http.Error(w, "replace failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := newAPIClient(HTTPConfig{BaseURL: server.URL, TimeoutSeconds: 1, MaxResponseBytes: 1024}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanupJournalSession(context.Background(), client, journalPath, expected); err == nil || !strings.Contains(err.Error(), "ownership changed") {
		t.Fatalf("replacement journal cleanup error=%v", err)
	}
	var got cleanupJournal
	if err := readStrictPrivateJSON(journalPath, &got); err != nil {
		t.Fatal(err)
	}
	if got != replacement {
		t.Fatalf("replacement journal removed or changed: %+v", got)
	}
}

func TestAdminTokenNeverSentToProviderRoute(t *testing.T) {
	dir := privateDir(t)
	tokenPath := writeConfig(t, dir, "token", "admin-secret")
	var providerHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerHeader = r.Header.Get("X-Subrouter-Admin-Token")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client, err := newAPIClient(HTTPConfig{BaseURL: server.URL, AdminTokenFile: tokenPath, TimeoutSeconds: 1, MaxResponseBytes: 1024}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.request(context.Background(), http.MethodPost, "/v1/responses", nil, nil); err != nil {
		t.Fatal(err)
	}
	if providerHeader != "" {
		t.Fatal("admin token was sent to provider route")
	}
}

func TestStickyRejectsExpiredHandoffWithoutTurn(t *testing.T) {
	dir := privateDir(t)
	server, turns := liveServer(t)
	defer server.Close()
	statePath := filepath.Join(dir, "state.json")
	journalPath := filepath.Join(dir, "journal.json")
	cfg := RoutedLegConfig{Schema: ConfigSchema, HTTP: HTTPConfig{BaseURL: server.URL, TimeoutSeconds: 1, MaxResponseBytes: 1024}, ProofFile: filepath.Join(dir, "proof.json"), StateFile: statePath, Journal: journalPath, Model: "model"}
	writeConfig(t, dir, "state.json", liveState{Schema: LiveStateSchema, SessionHash: strings.Repeat("a", 64), AccountHash: strings.Repeat("b", 64), ConfigHash: routedHandoffConfigHash(cfg), CreatedAt: time.Now().Add(-11 * time.Minute).UTC().Format(time.RFC3339Nano), RunID: "test-run"})
	path := writeConfig(t, dir, "config.json", cfg)
	if err := RunLeg(context.Background(), "sticky-reuse", path, "test-run"); err == nil {
		t.Fatal("expired handoff accepted")
	}
	if turns.Load() != 0 {
		t.Fatal("expired handoff sent provider traffic")
	}
}

func TestSameSourceIsolatedFailoverAttemptCaps(t *testing.T) {
	attempts, err := runIsolatedFailover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("attempts=%d, want 3", attempts)
	}
}

type failoverFixture struct {
	server          *httptest.Server
	mu              sync.Mutex
	assignments     map[string]string
	routes          []string
	noRetries       []bool
	deleted         bool
	failTurn        int
	badRequestShape bool
}

func newFailoverFixture(t *testing.T, failTurn int) *failoverFixture {
	t.Helper()
	fixture := &failoverFixture{assignments: map[string]string{}, failTurn: failTurn}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_subrouter/usage-status":
			used := float64(100)
			if fixture.failTurn == -1 {
				used = 99
			}
			json.NewEncoder(w).Encode([]liveUsageStatus{{ID: "dead-account", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, AuthChecked: true, AuthValid: true, Windows: []accounts.UsageWindow{{Name: "secondary", UsedPercent: used, LimitWindowSeconds: 604800, ResetAfterSeconds: 3600}}}})
		case "/_subrouter/sessions":
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if r.Method == http.MethodDelete {
				delete(fixture.assignments, r.URL.Query().Get("session_id"))
				fixture.deleted = true
				w.WriteHeader(http.StatusNoContent)
				return
			}
			all := []session.Assignment{}
			for id, account := range fixture.assignments {
				all = append(all, session.Assignment{AgentType: "codex", SessionID: id, AccountID: account})
			}
			json.NewEncoder(w).Encode(all)
		case "/v1/responses":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, present := body["max_output_tokens"]; present {
				fixture.badRequestShape = true
			}
			marker := strings.TrimPrefix(canaryResponsePrompt(body), "Reply with exactly ")
			sessionID := r.Header.Get("X-Subrouter-Session")
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			forced := r.Header.Get("X-Subrouter-Account-ID")
			if forced != "" {
				fixture.assignments[sessionID] = forced
				fixture.routes = append(fixture.routes, forced)
				fixture.noRetries = append(fixture.noRetries, r.Header.Get("X-Subrouter-No-Retry") == "true")
				if fixture.failTurn == 1 {
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = io.WriteString(w, `{"error":{"message":"server error"}}`)
					return
				}
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`)
				return
			}
			fixture.assignments[sessionID] = "replacement-account"
			fixture.routes = append(fixture.routes, "replacement-account")
			fixture.noRetries = append(fixture.noRetries, r.Header.Get("X-Subrouter-No-Retry") == "true")
			if fixture.failTurn == len(fixture.routes) {
				marker += " EXTRA"
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", marker)
		default:
			http.NotFound(w, r)
		}
	}))
	return fixture
}

func (f *failoverFixture) close() { f.server.Close() }

func TestLiveFailoverProvesExactRouteAndReuse(t *testing.T) {
	dir := privateDir(t)
	fixture := newFailoverFixture(t, 0)
	defer fixture.close()
	cfg := IsolatedLegConfig{Schema: ConfigSchema, HTTP: HTTPConfig{BaseURL: fixture.server.URL, TimeoutSeconds: 2, MaxResponseBytes: 1 << 16}, ProofFile: filepath.Join(dir, "proof.json"), Journal: filepath.Join(dir, "journal.json"), Model: "model", UnavailableAccountID: "dead-account"}
	path := writeConfig(t, dir, "config.json", cfg)
	if err := RunLeg(context.Background(), "safe-failover-reuse", path, "test-run"); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	want := []string{"dead-account", "replacement-account", "replacement-account"}
	if fmt.Sprint(fixture.routes) != fmt.Sprint(want) {
		t.Fatalf("routes=%v, want %v", fixture.routes, want)
	}
	if fmt.Sprint(fixture.noRetries) != "[true false true]" {
		t.Fatalf("no-retry sequence=%v", fixture.noRetries)
	}
	if !fixture.deleted || len(fixture.assignments) != 0 {
		t.Fatal("synthetic failover session not cleaned")
	}
	if fixture.badRequestShape {
		t.Fatal("real canary request included unsupported max_output_tokens")
	}
}

func TestLiveFailoverCleansSessionOnEveryFailureStage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failTurn int
	}{{"usage-status", -1}, {"forced", 1}, {"replacement", 2}, {"reuse", 3}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := privateDir(t)
			fixture := newFailoverFixture(t, tc.failTurn)
			defer fixture.close()
			cfg := IsolatedLegConfig{Schema: ConfigSchema, HTTP: HTTPConfig{BaseURL: fixture.server.URL, TimeoutSeconds: 2, MaxResponseBytes: 1 << 16}, ProofFile: filepath.Join(dir, "proof.json"), Journal: filepath.Join(dir, "journal.json"), Model: "model", UnavailableAccountID: "dead-account"}
			path := writeConfig(t, dir, "config.json", cfg)
			if err := RunLeg(context.Background(), "safe-failover-reuse", path, "test-run"); err == nil {
				t.Fatal("failed stage accepted")
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if !fixture.deleted || len(fixture.assignments) != 0 {
				t.Fatal("failed failover left synthetic session")
			}
			if _, err := os.Stat(cfg.Journal); !os.IsNotExist(err) {
				t.Fatal("successful failure cleanup left journal")
			}
		})
	}
}

func TestLiveFailoverRecoversPriorCrashJournal(t *testing.T) {
	dir := privateDir(t)
	fixture := newFailoverFixture(t, 0)
	defer fixture.close()
	staleSession := "cutover-failover-" + strings.Repeat("a", 32)
	fixture.assignments[staleSession] = "dead-account"
	journal := filepath.Join(dir, "journal.json")
	cfg := IsolatedLegConfig{Schema: ConfigSchema, HTTP: HTTPConfig{BaseURL: fixture.server.URL, TimeoutSeconds: 2, MaxResponseBytes: 1 << 16}, ProofFile: filepath.Join(dir, "proof.json"), Journal: journal, Model: "model", UnavailableAccountID: "dead-account"}
	writeConfig(t, dir, "journal.json", cleanupJournal{Schema: JournalSchema, SessionID: staleSession, AgentType: "codex", RunID: "prior-run", ConfigHash: isolatedCleanupConfigHash(cfg)})
	path := writeConfig(t, dir, "config.json", cfg)
	if err := RunLeg(context.Background(), "safe-failover-reuse", path, "test-run"); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if !fixture.deleted || len(fixture.assignments) != 0 {
		t.Fatalf("prior and current canary sessions were not cleaned: %v", fixture.assignments)
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatal("recovered failover journal remained")
	}
}

func TestLiveFailoverRejectsMalformedPriorCrashJournal(t *testing.T) {
	dir := privateDir(t)
	fixture := newFailoverFixture(t, 0)
	defer fixture.close()
	journal := filepath.Join(dir, "journal.json")
	original := []byte(`{"schema":"subrouter.cutover-canary-cleanup-journal/v1","session_id":"ordinary-session","agent_type":"codex","run_id":"prior-run"}`)
	if err := os.WriteFile(journal, original, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := IsolatedLegConfig{Schema: ConfigSchema, HTTP: HTTPConfig{BaseURL: fixture.server.URL, TimeoutSeconds: 2, MaxResponseBytes: 1 << 16}, ProofFile: filepath.Join(dir, "proof.json"), Journal: journal, Model: "model", UnavailableAccountID: "dead-account"}
	path := writeConfig(t, dir, "config.json", cfg)
	if err := RunLeg(context.Background(), "safe-failover-reuse", path, "test-run"); err == nil {
		t.Fatal("malformed prior journal accepted")
	}
	got, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("malformed prior journal was mutated")
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.deleted || len(fixture.routes) != 0 {
		t.Fatal("malformed prior journal caused live mutation")
	}
}

func existingSessionServer(t *testing.T, sessionID, logPath string, explicitActive *bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_subrouter/cutover-challenge":
			var registration cutoverChallengeRegistration
			if err := json.NewDecoder(r.Body).Decode(&registration); err != nil ||
				registration.Schema != "subrouter.cutover-challenge/v1" ||
				registration.AgentType != "codex" || registration.SessionID != sessionID ||
				len(registration.InputSHA256) != 64 || len(registration.MarkerSHA256) != 64 {
				t.Errorf("invalid challenge registration: %+v / %v", registration, err)
				http.Error(w, "bad challenge", http.StatusBadRequest)
				return
			}
			if r.Method != http.MethodPost && r.Method != http.MethodDelete {
				http.Error(w, "bad method", http.StatusMethodNotAllowed)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case "/_subrouter/sessions":
			json.NewEncoder(w).Encode([]sessionStatus{{
				Assignment: session.Assignment{AgentType: "codex", SessionID: sessionID, AccountID: "account-secret"},
				Active:     explicitActive,
			}})
		case "/v1/responses":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode challenged request: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			marker := strings.TrimPrefix(canaryResponsePrompt(body), "Reply with exactly ")
			if logPath != "" {
				logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Errorf("open candidate log: %v", err)
					http.Error(w, "log unavailable", http.StatusInternalServerError)
					return
				}
				_, err = fmt.Fprintf(logFile, "time=%s level=INFO msg=\"proxy request\" agent=codex session=%s cutover_marker_hash=%s\n", time.Now().UTC().Format(time.RFC3339Nano), sessionID, requestMarkerHash(marker))
				closeErr := logFile.Close()
				if err != nil || closeErr != nil {
					t.Errorf("append candidate log: %v / %v", err, closeErr)
				}
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", marker)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestExistingSessionChallengeWitness(t *testing.T) {
	dir := privateDir(t)
	selection := writeConfig(t, dir, "selection.json", Selection{Schema: "subrouter.cutover-canary-selection/v1", AgentType: "codex", SessionID: "existing-session"})
	logPath := filepath.Join(dir, "candidate.log")
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	inactive := false
	server := existingSessionServer(t, "existing-session", logPath, &inactive)
	defer server.Close()
	client, err := newAPIClient(HTTPConfig{BaseURL: server.URL, TimeoutSeconds: 2, MaxResponseBytes: 1 << 16}, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg := ExistingLegConfig{Schema: ConfigSchema, HTTP: HTTPConfig{BaseURL: server.URL, TimeoutSeconds: 2, MaxResponseBytes: 1 << 16}, ProofFile: filepath.Join(dir, "proof.json"), SelectionFile: selection, ChallengeFile: filepath.Join(dir, "challenge.json"), WitnessFile: filepath.Join(dir, "witness.json"), WaitSeconds: 3, CandidateLogFiles: []string{logPath}, MaxLogAppendBytes: 4096}
	path := writeConfig(t, dir, "existing.json", cfg)
	done := make(chan error, 1)
	go func() { done <- RunLeg(context.Background(), "existing-session-next-turn", path, "test-run") }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(cfg.ChallengeFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("challenge not created")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var challenge Challenge
	if err := readStrictPrivateJSON(cfg.ChallengeFile, &challenge); err != nil {
		t.Fatal(err)
	}
	marker := strings.TrimPrefix(challenge.Prompt, "Reply with exactly ")
	waitForChallengeReady(t, challenge)
	if err := CreateWitness(cfg.ChallengeFile, cfg.WitnessFile, strings.NewReader(marker+"\nEXTRA\n")); err == nil {
		t.Fatal("witness accepted trailing output")
	}
	status, response, err := client.liveTurn(context.Background(), "existing-session", "model", marker, "", true)
	if err != nil || status != http.StatusOK || !markerResponse(response, marker) {
		t.Fatalf("challenged turn status=%d err=%v", status, err)
	}
	if err := CreateWitness(cfg.ChallengeFile, cfg.WitnessFile, strings.NewReader(marker+"\n")); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.ChallengeFile); !os.IsNotExist(err) {
		t.Fatal("challenge not cleaned")
	}
}

func TestExistingWitnessWithoutNewCandidateLogFails(t *testing.T) {
	dir := privateDir(t)
	inactive := false
	server := existingSessionServer(t, "existing-no-log", "", &inactive)
	defer server.Close()
	selection := writeConfig(t, dir, "selection.json", Selection{Schema: "subrouter.cutover-canary-selection/v1", AgentType: "codex", SessionID: "existing-no-log"})
	logPath := filepath.Join(dir, "candidate.log")
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := ExistingLegConfig{Schema: ConfigSchema, HTTP: HTTPConfig{BaseURL: server.URL, TimeoutSeconds: 2, MaxResponseBytes: 1 << 16}, ProofFile: filepath.Join(dir, "proof.json"), SelectionFile: selection, ChallengeFile: filepath.Join(dir, "challenge.json"), WitnessFile: filepath.Join(dir, "witness.json"), WaitSeconds: 2, CandidateLogFiles: []string{logPath}, MaxLogAppendBytes: 4096}
	path := writeConfig(t, dir, "config.json", cfg)
	done := make(chan error, 1)
	go func() { done <- RunLeg(context.Background(), "existing-session-next-turn", path, "test-run") }()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(cfg.ChallengeFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("challenge not created")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var challenge Challenge
	if err := readStrictPrivateJSON(cfg.ChallengeFile, &challenge); err != nil {
		t.Fatal(err)
	}
	marker := strings.TrimPrefix(challenge.Prompt, "Reply with exactly ")
	waitForChallengeReady(t, challenge)
	if err := CreateWitness(cfg.ChallengeFile, cfg.WitnessFile, strings.NewReader(marker+"\n")); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "lacks new candidate") {
		t.Fatalf("witness without log error=%v", err)
	}
}

func TestExistingSessionRequiresExplicitInactivity(t *testing.T) {
	for _, test := range []struct {
		name   string
		active *bool
		want   string
	}{
		{name: "active", active: func() *bool { value := true; return &value }(), want: "is active"},
		{name: "missing activity proof", active: nil, want: "activity unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := privateDir(t)
			logPath := filepath.Join(dir, "candidate.log")
			if err := os.WriteFile(logPath, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			server := existingSessionServer(t, "selected", logPath, test.active)
			defer server.Close()
			selection := writeConfig(t, dir, "selection.json", Selection{Schema: "subrouter.cutover-canary-selection/v1", AgentType: "codex", SessionID: "selected"})
			cfg := ExistingLegConfig{Schema: ConfigSchema, HTTP: HTTPConfig{BaseURL: server.URL, TimeoutSeconds: 2, MaxResponseBytes: 1 << 16}, ProofFile: filepath.Join(dir, "proof.json"), SelectionFile: selection, ChallengeFile: filepath.Join(dir, "challenge.json"), WitnessFile: filepath.Join(dir, "witness.json"), WaitSeconds: 1, CandidateLogFiles: []string{logPath}, MaxLogAppendBytes: 4096}
			err := RunLeg(context.Background(), "existing-session-next-turn", writeConfig(t, dir, "config.json", cfg), "test-run")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
			if _, statErr := os.Stat(cfg.ChallengeFile); !os.IsNotExist(statErr) {
				t.Fatal("challenge published without inactivity proof")
			}
		})
	}
}

func TestExistingSessionRejectsUnrelatedSelectedSessionTurn(t *testing.T) {
	dir := privateDir(t)
	logPath := filepath.Join(dir, "candidate.log")
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	inactive := false
	server := existingSessionServer(t, "selected", logPath, &inactive)
	defer server.Close()
	client, err := newAPIClient(HTTPConfig{BaseURL: server.URL, TimeoutSeconds: 2, MaxResponseBytes: 1 << 16}, false)
	if err != nil {
		t.Fatal(err)
	}
	selection := writeConfig(t, dir, "selection.json", Selection{Schema: "subrouter.cutover-canary-selection/v1", AgentType: "codex", SessionID: "selected"})
	cfg := ExistingLegConfig{Schema: ConfigSchema, HTTP: HTTPConfig{BaseURL: server.URL, TimeoutSeconds: 2, MaxResponseBytes: 1 << 16}, ProofFile: filepath.Join(dir, "proof.json"), SelectionFile: selection, ChallengeFile: filepath.Join(dir, "challenge.json"), WitnessFile: filepath.Join(dir, "witness.json"), WaitSeconds: 2, CandidateLogFiles: []string{logPath}, MaxLogAppendBytes: 4096}
	configPath := writeConfig(t, dir, "config.json", cfg)
	done := make(chan error, 1)
	go func() { done <- RunLeg(context.Background(), "existing-session-next-turn", configPath, "test-run") }()
	var challenge Challenge
	deadline := time.Now().Add(time.Second)
	for {
		if err := readStrictPrivateJSON(cfg.ChallengeFile, &challenge); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("challenge not created")
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitForChallengeReady(t, challenge)
	unrelated := "SUBROUTER_EXISTING_00000000000000000000000000000000"
	if _, response, err := client.liveTurn(context.Background(), "selected", "model", unrelated, "", true); err != nil || !markerResponse(response, unrelated) {
		t.Fatalf("unrelated selected-session turn failed: %v", err)
	}
	marker := strings.TrimPrefix(challenge.Prompt, "Reply with exactly ")
	if err := CreateWitness(cfg.ChallengeFile, cfg.WitnessFile, strings.NewReader(marker)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "unrelated or duplicate") {
		t.Fatalf("unrelated selected-session request accepted: %v", err)
	}
}

func TestLiveChallengeCannotBeOverwritten(t *testing.T) {
	dir := privateDir(t)
	challengePath := filepath.Join(dir, "challenge.json")
	witnessPath := filepath.Join(dir, "witness.json")
	first := Challenge{Schema: ChallengeSchema, RunID: "first", Nonce: strings.Repeat("a", 32)}
	lease, err := publishChallenge(challengePath, witnessPath, first)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	before, err := os.ReadFile(challengePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publishChallenge(challengePath, witnessPath, Challenge{Schema: ChallengeSchema, RunID: "second", Nonce: strings.Repeat("b", 32)}); !errors.Is(err, errChallengeActive) {
		t.Fatalf("concurrent challenge error=%v", err)
	}
	after, err := os.ReadFile(challengePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("live concurrent challenge was overwritten")
	}
}

func TestCandidateLogRejectsOldWrongAndOversizeEvidence(t *testing.T) {
	dir := privateDir(t)
	old := filepath.Join(dir, "old.log")
	matching := fmt.Sprintf("time=%s level=INFO msg=\"proxy request\" agent=codex session=chosen\n", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(old, []byte(matching), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshots, err := snapshotCandidateLogs([]string{old})
	if err != nil {
		t.Fatal(err)
	}
	notBefore := time.Now().UTC()
	if matched, err := freshProxyLogEvidence(snapshots, "codex", "chosen", strings.Repeat("a", 64), notBefore, 4096); err != nil || matched {
		t.Fatalf("old line matched=%t err=%v", matched, err)
	}
	f, err := os.OpenFile(old, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(f, "time=%s level=INFO msg=\"proxy request\" agent=codex session=wrong\n", time.Now().UTC().Format(time.RFC3339Nano))
	_ = f.Close()
	if matched, err := freshProxyLogEvidence(snapshots, "codex", "chosen", strings.Repeat("a", 64), notBefore, 4096); err != nil || matched {
		t.Fatalf("wrong line matched=%t err=%v", matched, err)
	}
	mixed := filepath.Join(dir, "mixed.log")
	if err := os.WriteFile(mixed, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	mixedSnapshots, err := snapshotCandidateLogs([]string{mixed})
	if err != nil {
		t.Fatal(err)
	}
	markerHash := strings.Repeat("a", 64)
	mixedFile, err := os.OpenFile(mixed, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(mixedFile, "time=%s level=INFO msg=\"proxy request\" agent=codex session=chosen\n", time.Now().UTC().Format(time.RFC3339Nano))
	_, _ = fmt.Fprintf(mixedFile, "time=%s level=INFO msg=\"proxy request\" agent=codex session=chosen cutover_marker_hash=%s\n", time.Now().UTC().Format(time.RFC3339Nano), markerHash)
	_ = mixedFile.Close()
	if _, err := freshProxyLogEvidence(mixedSnapshots, "codex", "chosen", markerHash, notBefore, 4096); err == nil {
		t.Fatal("challenged request accepted after an unrelated selected-session request")
	}
	over := filepath.Join(dir, "over.log")
	if err := os.WriteFile(over, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	overSnapshots, err := snapshotCandidateLogs([]string{over})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(over, []byte(strings.Repeat("x", 300)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := freshProxyLogEvidence(overSnapshots, "codex", "chosen", strings.Repeat("a", 64), time.Now().UTC(), 256); err == nil {
		t.Fatal("oversize append accepted")
	}
}

func TestCandidateLogAcceptsDefaultSlogProxyRequest(t *testing.T) {
	dir := privateDir(t)
	path := filepath.Join(dir, "default-slog.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshots, err := snapshotCandidateLogs([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	markerHash := strings.Repeat("a", 64)
	observed := time.Now()
	notBefore := observed.Truncate(time.Second).Add(900 * time.Millisecond)
	line := fmt.Sprintf("%s INFO proxy request agent=codex session=chosen user_hash=\"\" method=POST path=/v1/responses upstream=example.test remote_addr=127.0.0.1 user_agent=codex_exec cutover_marker_hash=%s\n", observed.Format("2006/01/02 15:04:05"), markerHash)
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	matched, err := freshProxyLogEvidence(snapshots, "codex", "chosen", markerHash, notBefore, 4096)
	if err != nil || !matched {
		t.Fatalf("default slog line matched=%t err=%v", matched, err)
	}
}

func TestDefaultSlogProxyRequestStillRejectsWrongOrOldEvidence(t *testing.T) {
	markerHash := strings.Repeat("a", 64)
	now := time.Now()
	wrongMarker := fmt.Sprintf("%s INFO proxy request agent=codex session=chosen cutover_marker_hash=%s", now.Format("2006/01/02 15:04:05"), strings.Repeat("b", 64))
	if selected, exact := selectedProxyRequestLine([]byte(wrongMarker), "codex", "chosen", markerHash, now.Add(-time.Second)); !selected || exact {
		t.Fatalf("wrong marker selected=%t exact=%t", selected, exact)
	}
	old := fmt.Sprintf("%s INFO proxy request agent=codex session=chosen cutover_marker_hash=%s", now.Add(-2*time.Second).Format("2006/01/02 15:04:05"), markerHash)
	if selected, exact := selectedProxyRequestLine([]byte(old), "codex", "chosen", markerHash, now); !selected || exact {
		t.Fatalf("old line selected=%t exact=%t", selected, exact)
	}
	malformed := fmt.Sprintf("%s INFO proxy request agent=codex session=chosen cutover_marker_hash=\"unterminated", now.Format("2006/01/02 15:04:05"))
	if selected, exact := selectedProxyRequestLine([]byte(malformed), "codex", "chosen", markerHash, now.Add(-time.Second)); selected || exact {
		t.Fatalf("malformed line selected=%t exact=%t", selected, exact)
	}
}
