package cutovercanary

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const ConfigSchema = "subrouter.cutover-canary-config/v1"

type LegResult struct {
	Schema string `json:"schema"`
	Leg    string `json:"leg"`
	OK     bool   `json:"ok"`
}

type PeerProbeResult struct {
	Schema             string `json:"schema"`
	OK                 bool   `json:"ok"`
	HealthOK           bool   `json:"health_ok"`
	ReadyOK            bool   `json:"ready_ok"`
	Draining           bool   `json:"draining"`
	ExecutableIdentity string `json:"executable_identity"`
	IdentityKind       string `json:"identity_kind"`
}

type proof struct {
	Schema          string `json:"schema"`
	Leg             string `json:"leg"`
	OK              bool   `json:"ok"`
	Scope           string `json:"scope"`
	DurationMillis  int64  `json:"duration_ms"`
	PeerCount       int    `json:"peer_count,omitempty"`
	HTTPStatus      int    `json:"http_status,omitempty"`
	SessionHash     string `json:"session_hash,omitempty"`
	AccountHash     string `json:"account_hash,omitempty"`
	StickyMatched   bool   `json:"sticky_matched,omitempty"`
	AccountChanged  bool   `json:"account_changed,omitempty"`
	Attempts        int    `json:"attempts,omitempty"`
	RunHash         string `json:"run_hash"`
	UnavailableHash string `json:"unavailable_account_hash,omitempty"`
	LogMatched      bool   `json:"candidate_log_matched,omitempty"`
}

type liveState struct {
	Schema      string `json:"schema"`
	SessionHash string `json:"session_hash"`
	AccountHash string `json:"account_hash"`
	ConfigHash  string `json:"config_hash"`
	CreatedAt   string `json:"created_at"`
	RunID       string `json:"run_id"`
}

type cleanupJournal struct {
	Schema     string `json:"schema"`
	SessionID  string `json:"session_id"`
	AgentType  string `json:"agent_type"`
	RunID      string `json:"run_id"`
	ConfigHash string `json:"config_hash"`
}

func RunLeg(ctx context.Context, leg, configPath, runID string) error {
	if !validRunID(runID) {
		return errors.New("invalid canary run ID")
	}
	start := time.Now()
	switch leg {
	case "peer-health-readiness":
		var cfg PeerLegConfig
		if err := loadConfig(configPath, &cfg, cfg.Schema); err != nil {
			return err
		}
		if err := validateArtifactPaths([]string{cfg.ProofFile}, []string{cfg.configPath}); err != nil {
			return err
		}
		if len(cfg.Peers) == 0 || len(cfg.Peers) > 32 {
			return errors.New("peer list must contain 1..32 entries")
		}
		for _, peer := range cfg.Peers {
			if peer.SSHIdentityFile != "" {
				// Validate each identity against the writable proof and the
				// config independently. Multiple peers may intentionally share
				// the same read-only identity file.
				if err := validateArtifactPaths([]string{cfg.ProofFile}, []string{cfg.configPath, peer.SSHIdentityFile}); err != nil {
					return err
				}
			}
		}
		names := map[string]bool{}
		for _, peer := range cfg.Peers {
			if names[peer.Name] {
				return errors.New("duplicate peer name")
			}
			names[peer.Name] = true
			if err := runPeer(ctx, peer); err != nil {
				return err
			}
		}
		return writePrivateJSON(cfg.ProofFile, proof{Schema: ProofSchema, Leg: leg, OK: true, Scope: "configured-peers", DurationMillis: time.Since(start).Milliseconds(), PeerCount: len(cfg.Peers), RunHash: hashProof("run", runID)})
	case "authenticated-routed-codex":
		var cfg RoutedLegConfig
		if err := loadConfig(configPath, &cfg, cfg.Schema); err != nil {
			return err
		}
		return runAuthenticated(ctx, cfg, start, runID)
	case "sticky-reuse":
		var cfg RoutedLegConfig
		if err := loadConfig(configPath, &cfg, cfg.Schema); err != nil {
			return err
		}
		return runSticky(ctx, cfg, start, runID)
	case "safe-failover-reuse":
		var cfg IsolatedLegConfig
		if err := loadConfig(configPath, &cfg, cfg.Schema); err != nil {
			return err
		}
		if _, err := runIsolatedFailover(ctx); err != nil {
			return err
		}
		return runLiveFailover(ctx, cfg, start, runID)
	case "authenticated-routed-claude":
		var cfg ClaudeLegConfig
		if err := loadConfig(configPath, &cfg, cfg.Schema); err != nil {
			return err
		}
		return runAuthenticatedClaude(ctx, cfg, start, runID)
	case "existing-session-next-turn":
		var cfg ExistingLegConfig
		if err := loadConfig(configPath, &cfg, cfg.Schema); err != nil {
			return err
		}
		return runExisting(ctx, cfg, start, runID)
	default:
		return errors.New("unsupported canary leg")
	}
}

func loadConfig(path string, dst any, _ string) error {
	if err := readStrictPrivateJSON(path, dst); err != nil {
		return err
	}
	var schema string
	switch cfg := dst.(type) {
	case *PeerLegConfig:
		schema = cfg.Schema
		cfg.configPath = path
	case *RoutedLegConfig:
		schema = cfg.Schema
		cfg.configPath = path
	case *IsolatedLegConfig:
		schema = cfg.Schema
		cfg.configPath = path
	case *ClaudeLegConfig:
		schema = cfg.Schema
		cfg.configPath = path
	case *ExistingLegConfig:
		schema = cfg.Schema
		cfg.configPath = path
	case *PeerProbeConfig:
		schema = cfg.Schema
		cfg.configPath = path
	default:
		return errors.New("unsupported canary configuration type")
	}
	if schema != ConfigSchema {
		return errors.New("unsupported canary config schema")
	}
	return nil
}

var sshCommandPath = "/usr/bin/ssh"

func runPeer(ctx context.Context, peer PeerTarget) error {
	if peer.Name == "" || !safeSSHHost(peer.SSHHost) || !safeRemotePath(peer.RemoteExecutable) ||
		!safeRemotePath(peer.RemoteConfigFile) || !validExecutableIdentity(peer.ExpectedIdentityKind, peer.ExpectedExecutableIdentity) {
		return errors.New("invalid peer command")
	}
	if peer.SSHIdentityFile != "" {
		if !safeRemotePath(peer.SSHIdentityFile) {
			return errors.New("invalid peer SSH identity file")
		}
		if _, err := readPrivateFile(peer.SSHIdentityFile, 1<<20); err != nil {
			return errors.New("invalid peer SSH identity file")
		}
	}
	if peer.TimeoutSeconds < 1 || peer.TimeoutSeconds > 120 {
		return errors.New("invalid peer timeout")
	}
	deadline, cancel := context.WithTimeout(ctx, time.Duration(peer.TimeoutSeconds)*time.Second)
	defer cancel()
	args := []string{
		"-T", "-F", "none", "-o", "BatchMode=yes", "-o", "ClearAllForwardings=yes",
		"-o", "PermitLocalCommand=no", "-o", "ControlMaster=no", "-o", "ControlPersist=no",
		"-o", "ForkAfterAuthentication=no", "-o", "ProxyCommand=none", "-o", "ProxyJump=none",
	}
	if peer.SSHIdentityFile != "" {
		args = append(args, "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-i", peer.SSHIdentityFile)
	}
	args = append(args, "--", peer.SSHHost, peer.RemoteExecutable, "peer-probe", "--config", peer.RemoteConfigFile)
	cmd := exec.CommandContext(deadline, sshCommandPath, args...)
	cmd.Env = canaryEnvironment()
	var out strings.Builder
	cmd.Stdout = &limitedWriter{w: &out, remaining: 4096}
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return errors.New("peer probe command failed")
	}
	var got PeerProbeResult
	dec := json.NewDecoder(strings.NewReader(out.String()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil || got.Schema != PeerProbeSchema || !got.OK || !got.HealthOK || !got.ReadyOK || got.Draining || got.IdentityKind != peer.ExpectedIdentityKind || got.ExecutableIdentity != peer.ExpectedExecutableIdentity {
		return errors.New("peer probe proof invalid")
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("peer probe emitted trailing output")
	}
	return nil
}

func safeSSHHost(value string) bool {
	if value == "" || len(value) > 255 || strings.HasPrefix(value, "-") {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._@:-", r) {
			continue
		}
		return false
	}
	return true
}

func safeRemotePath(value string) bool {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value || len(value) > 1024 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._/:-", r) {
			continue
		}
		return false
	}
	return true
}

func validSHA256(value string) bool {
	return len(value) == 64 && isLowerHex(value)
}

func canaryEnvironment() []string {
	allowed := []string{"HOME", "PATH", "TMPDIR", "LANG", "LC_ALL", "SSH_AUTH_SOCK"}
	env := make([]string, 0, len(allowed))
	for _, key := range allowed {
		if value, ok := os.LookupEnv(key); ok && !strings.ContainsAny(value, "\r\n") {
			env = append(env, key+"="+value)
		}
	}
	return env
}

type limitedWriter struct {
	w         io.Writer
	remaining int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		return 0, errors.New("output cap exceeded")
	}
	n, err := w.w.Write(p)
	w.remaining -= n
	return n, err
}

func runAuthenticated(ctx context.Context, cfg RoutedLegConfig, start time.Time, runID string) error {
	if cfg.Model == "" {
		return errors.New("canary model is required")
	}
	if err := validateArtifactPaths(
		[]string{cfg.ProofFile, cfg.StateFile, cfg.StateFile + ".lock", cfg.Journal, cfg.Journal + ".lock"},
		[]string{cfg.configPath, cfg.HTTP.AdminTokenFile},
	); err != nil {
		return err
	}
	client, err := newAPIClient(cfg.HTTP, false)
	if err != nil {
		return err
	}
	journalLease, err := acquireJournalLease(cfg.Journal)
	if err != nil {
		return err
	}
	defer journalLease.Close()
	stateLease, err := acquireStateLease(cfg.StateFile)
	if err != nil {
		return err
	}
	defer stateLease.Close()
	configHash := routedHandoffConfigHash(cfg)
	if err := recoverJournal(ctx, client, cfg.StateFile, cfg.Journal, configHash); err != nil {
		return err
	}
	nonce, err := randomHex(16)
	if err != nil {
		return err
	}
	sessionID := "cutover-canary-" + nonce
	journal := cleanupJournal{Schema: JournalSchema, SessionID: sessionID, AgentType: "codex", RunID: runID, ConfigHash: configHash}
	if err := createPrivateJSON(cfg.Journal, journal); err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = cleanupLiveArtifacts(context.Background(), client, cfg.StateFile, cfg.Journal, journal)
		}
	}()
	marker := "SUBROUTER_CANARY_" + strings.ToUpper(nonce)
	status, err := client.routedTurn(ctx, sessionID, cfg.Model, marker)
	if err != nil {
		return err
	}
	all, err := client.sessions(ctx)
	if err != nil {
		return err
	}
	assignment, ok := findSession(all, "codex", sessionID)
	if !ok || assignment.AccountID == "" {
		return errors.New("routed session assignment absent")
	}
	stickyNonce, err := randomHex(16)
	if err != nil {
		return err
	}
	status, err = client.routedTurn(ctx, sessionID, cfg.Model, "SUBROUTER_AUTH_STICKY_"+strings.ToUpper(stickyNonce))
	if err != nil {
		return err
	}
	all, err = client.sessions(ctx)
	if err != nil {
		return err
	}
	stickyAssignment, ok := findSession(all, "codex", sessionID)
	if !ok || stickyAssignment.AccountID != assignment.AccountID {
		return errors.New("authenticated session did not remain sticky")
	}
	if err := cleanupLiveArtifacts(ctx, client, cfg.StateFile, cfg.Journal, journal); err != nil {
		return err
	}
	cleanup = false
	state := liveState{Schema: LiveStateSchema, SessionHash: hashProof(nonce, sessionID), AccountHash: hashProof(nonce, assignment.AccountID), ConfigHash: routedHandoffConfigHash(cfg), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), RunID: runID}
	if err := writePrivateJSON(cfg.StateFile, state); err != nil {
		return err
	}
	if err := writePrivateJSON(cfg.ProofFile, proof{Schema: ProofSchema, Leg: "authenticated-routed-codex", OK: true, Scope: "live-two-turn-sticky-cleaned", DurationMillis: time.Since(start).Milliseconds(), HTTPStatus: status, SessionHash: state.SessionHash, AccountHash: state.AccountHash, StickyMatched: true, Attempts: 2, RunHash: hashProof("run", runID)}); err != nil {
		_ = removeIfExists(cfg.StateFile, LiveStateSchema)
		return err
	}
	return nil
}

func runAuthenticatedClaude(ctx context.Context, cfg ClaudeLegConfig, start time.Time, runID string) error {
	if cfg.Model == "" {
		return errors.New("Claude canary model is required")
	}
	if err := validateArtifactPaths(
		[]string{cfg.ProofFile, cfg.Journal, cfg.Journal + ".lock"},
		[]string{cfg.configPath, cfg.HTTP.AdminTokenFile},
	); err != nil {
		return err
	}
	client, err := newAPIClient(cfg.HTTP, false)
	if err != nil {
		return err
	}
	journalLease, err := acquireJournalLease(cfg.Journal)
	if err != nil {
		return err
	}
	defer journalLease.Close()
	configHash := claudeCleanupConfigHash(cfg)
	if err := recoverClaudeJournal(ctx, client, cfg.Journal, configHash); err != nil {
		return err
	}
	nonce, err := randomHex(16)
	if err != nil {
		return err
	}
	sessionID := "cutover-claude-" + nonce
	journal := cleanupJournal{Schema: JournalSchema, SessionID: sessionID, AgentType: "claude", RunID: runID, ConfigHash: configHash}
	if err := createPrivateJSON(cfg.Journal, journal); err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = cleanupJournalSession(context.Background(), client, cfg.Journal, journal)
		}
	}()
	marker := "SUBROUTER_CLAUDE_CANARY_" + strings.ToUpper(nonce)
	status, err := client.claudeTurn(ctx, sessionID, cfg.Model, marker)
	if err != nil {
		return err
	}
	all, err := client.sessions(ctx)
	if err != nil {
		return err
	}
	assignment, ok := findSession(all, "claude", sessionID)
	if !ok || assignment.AccountID == "" {
		return errors.New("routed Claude session assignment absent")
	}
	if err := cleanupClaudeSession(ctx, client, cfg.Journal, journal); err != nil {
		return err
	}
	cleanup = false
	return writePrivateJSON(cfg.ProofFile, proof{
		Schema: ProofSchema, Leg: "authenticated-routed-claude", OK: true,
		Scope: "live-claude-one-turn-cleaned", DurationMillis: time.Since(start).Milliseconds(),
		HTTPStatus: status, SessionHash: hashProof(nonce, sessionID),
		AccountHash: hashProof(nonce, assignment.AccountID), Attempts: 1, RunHash: hashProof("run", runID),
	})
}

func recoverClaudeJournal(ctx context.Context, client *apiClient, journalPath, expectedConfigHash string) error {
	var journal cleanupJournal
	if err := readStrictPrivateJSON(journalPath, &journal); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return errors.New("cleanup journal invalid")
	}
	if journal.Schema != JournalSchema || journal.AgentType != "claude" ||
		!validRunID(journal.RunID) || journal.ConfigHash != expectedConfigHash ||
		!validCanarySessionID(journal.SessionID, "cutover-claude-") {
		return errors.New("cleanup journal invalid")
	}
	return cleanupJournalSession(ctx, client, journalPath, journal)
}

func cleanupClaudeSession(ctx context.Context, client *apiClient, journalPath string, expected cleanupJournal) error {
	if err := requireOwnedJournal(journalPath, expected); err != nil {
		return err
	}
	if err := client.deleteSession(ctx, expected.AgentType, expected.SessionID); err != nil {
		return err
	}
	all, err := client.sessions(ctx)
	if err != nil {
		return err
	}
	if _, exists := findSession(all, expected.AgentType, expected.SessionID); exists {
		return errors.New("routed Claude session remained after cleanup")
	}
	if err := requireOwnedJournal(journalPath, expected); err != nil {
		return err
	}
	return removeIfExists(journalPath, JournalSchema)
}

func runSticky(ctx context.Context, cfg RoutedLegConfig, start time.Time, runID string) error {
	if cfg.Model == "" {
		return errors.New("canary model is required")
	}
	if err := validateArtifactPaths(
		[]string{cfg.ProofFile, cfg.StateFile, cfg.StateFile + ".lock", cfg.Journal, cfg.Journal + ".lock"},
		[]string{cfg.configPath, cfg.HTTP.AdminTokenFile},
	); err != nil {
		return err
	}
	stateLease, err := acquireStateLease(cfg.StateFile)
	if err != nil {
		return err
	}
	defer stateLease.Close()
	var state liveState
	if err := readStrictPrivateJSON(cfg.StateFile, &state); err != nil || state.Schema != LiveStateSchema || len(state.SessionHash) != 64 || len(state.AccountHash) != 64 || len(state.ConfigHash) != 64 || state.ConfigHash != routedHandoffConfigHash(cfg) || state.RunID != runID {
		return errors.New("live canary state invalid")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, state.CreatedAt)
	if err != nil || createdAt.After(time.Now().Add(time.Minute)) || time.Since(createdAt) > 10*time.Minute {
		_ = removeIfExists(cfg.StateFile, LiveStateSchema)
		return errors.New("live canary state expired")
	}
	if _, journalErr := os.Lstat(cfg.Journal); !errors.Is(journalErr, os.ErrNotExist) {
		return errors.New("cleanup journal remained after authenticated leg")
	}
	if err := writePrivateJSON(cfg.ProofFile, proof{Schema: ProofSchema, Leg: "sticky-reuse", OK: true, Scope: "sanitized-authenticated-handoff", DurationMillis: time.Since(start).Milliseconds(), SessionHash: state.SessionHash, AccountHash: state.AccountHash, StickyMatched: true, Attempts: 2, RunHash: hashProof("run", runID)}); err != nil {
		return err
	}
	if err := removeIfExists(cfg.StateFile, LiveStateSchema); err != nil {
		_ = removeIfExists(cfg.ProofFile, ProofSchema)
		return err
	}
	return nil
}

func runLiveFailover(ctx context.Context, cfg IsolatedLegConfig, start time.Time, runID string) error {
	if cfg.Model == "" || cfg.UnavailableAccountID == "" {
		return errors.New("live failover model and unavailable account are required")
	}
	if err := validateArtifactPaths(
		[]string{cfg.ProofFile, cfg.Journal, cfg.Journal + ".lock"},
		[]string{cfg.configPath, cfg.HTTP.AdminTokenFile},
	); err != nil {
		return err
	}
	client, err := newAPIClient(cfg.HTTP, false)
	if err != nil {
		return err
	}
	journalLease, err := acquireJournalLease(cfg.Journal)
	if err != nil {
		return err
	}
	defer journalLease.Close()
	configHash := isolatedCleanupConfigHash(cfg)
	if err := recoverFailoverJournal(ctx, client, cfg.Journal, configHash); err != nil {
		return err
	}
	nonce, err := randomHex(16)
	if err != nil {
		return err
	}
	sessionID := "cutover-failover-" + nonce
	journal := cleanupJournal{Schema: JournalSchema, SessionID: sessionID, AgentType: "codex", RunID: runID, ConfigHash: configHash}
	if err := createPrivateJSON(cfg.Journal, journal); err != nil {
		return err
	}
	cleaned := false
	defer func() {
		if !cleaned {
			_ = cleanupJournalSession(context.Background(), client, cfg.Journal, journal)
		}
	}()
	unavailable, err := client.unavailableCodexAccount(ctx, cfg.UnavailableAccountID)
	if err != nil {
		return err
	}
	if !unavailable {
		return errors.New("configured account is not proven unavailable by usage status")
	}
	forcedMarker := "SUBROUTER_FORCED_FAILURE_" + strings.ToUpper(nonce)
	status, body, err := client.liveTurn(ctx, sessionID, cfg.Model, forcedMarker, cfg.UnavailableAccountID, true)
	if err != nil {
		return err
	}
	if !quotaFailureResponse(status, body) {
		return errors.New("forced unavailable account did not reconfirm quota failure")
	}
	all, err := client.sessions(ctx)
	if err != nil {
		return err
	}
	forcedAssignment, ok := findSession(all, "codex", sessionID)
	if !ok || forcedAssignment.AccountID != cfg.UnavailableAccountID {
		return errors.New("forced unavailable assignment not established")
	}
	replacementNonce, err := randomHex(16)
	if err != nil {
		return err
	}
	replacementMarker := "SUBROUTER_FAILOVER_" + strings.ToUpper(replacementNonce)
	status, body, err = client.liveTurn(ctx, sessionID, cfg.Model, replacementMarker, "", false)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 || !markerResponse(body, replacementMarker) {
		return errors.New("live failover response not proven")
	}
	all, err = client.sessions(ctx)
	if err != nil {
		return err
	}
	replacement, ok := findSession(all, "codex", sessionID)
	if !ok || replacement.AccountID == "" || replacement.AccountID == cfg.UnavailableAccountID {
		return errors.New("live failover did not move assignment")
	}
	reuseNonce, err := randomHex(16)
	if err != nil {
		return err
	}
	reuseMarker := "SUBROUTER_REUSE_" + strings.ToUpper(reuseNonce)
	status, body, err = client.liveTurn(ctx, sessionID, cfg.Model, reuseMarker, "", true)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 || !markerResponse(body, reuseMarker) {
		return errors.New("replacement reuse response not proven")
	}
	all, err = client.sessions(ctx)
	if err != nil {
		return err
	}
	reused, ok := findSession(all, "codex", sessionID)
	if !ok || reused.AccountID != replacement.AccountID {
		return errors.New("replacement account was not reused")
	}
	if err := cleanupJournalSession(ctx, client, cfg.Journal, journal); err != nil {
		return err
	}
	cleaned = true
	return writePrivateJSON(cfg.ProofFile, proof{Schema: ProofSchema, Leg: "safe-failover-reuse", OK: true, Scope: "live-failover-with-same-source-prerequisite", DurationMillis: time.Since(start).Milliseconds(), HTTPStatus: status, SessionHash: hashProof(nonce, sessionID), AccountHash: hashProof(nonce, replacement.AccountID), UnavailableHash: hashProof(nonce, cfg.UnavailableAccountID), StickyMatched: true, Attempts: 3, RunHash: hashProof("run", runID)})
}

func recoverFailoverJournal(ctx context.Context, client *apiClient, journalPath, expectedConfigHash string) error {
	var journal cleanupJournal
	if err := readStrictPrivateJSON(journalPath, &journal); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return errors.New("cleanup journal invalid")
	}
	if journal.Schema != JournalSchema || journal.AgentType != "codex" ||
		!validRunID(journal.RunID) || journal.ConfigHash != expectedConfigHash ||
		!validCanarySessionID(journal.SessionID, "cutover-failover-") {
		return errors.New("cleanup journal invalid")
	}
	return cleanupJournalSession(ctx, client, journalPath, journal)
}

func validCanarySessionID(value, prefix string) bool {
	nonce := strings.TrimPrefix(value, prefix)
	return len(value) == len(prefix)+32 && len(nonce) == 32 && isLowerHex(nonce)
}

func isLowerHex(value string) bool {
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func cleanupJournalSession(ctx context.Context, client *apiClient, journalPath string, expected cleanupJournal) error {
	if err := requireOwnedJournal(journalPath, expected); err != nil {
		return err
	}
	if err := client.deleteSession(ctx, expected.AgentType, expected.SessionID); err != nil {
		return err
	}
	if err := requireOwnedJournal(journalPath, expected); err != nil {
		return err
	}
	return removeIfExists(journalPath, JournalSchema)
}

func requireOwnedJournal(journalPath string, expected cleanupJournal) error {
	var current cleanupJournal
	if err := readStrictPrivateJSON(journalPath, &current); err != nil {
		return errors.New("cleanup journal ownership unavailable")
	}
	if current != expected {
		return errors.New("cleanup journal ownership changed")
	}
	return nil
}

func recoverJournal(ctx context.Context, client *apiClient, statePath, journalPath, expectedConfigHash string) error {
	var j cleanupJournal
	if err := readStrictPrivateJSON(journalPath, &j); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			var state liveState
			if stateErr := readStrictPrivateJSON(statePath, &state); errors.Is(stateErr, os.ErrNotExist) {
				return nil
			} else if stateErr != nil || state.Schema != LiveStateSchema || len(state.SessionHash) != 64 || len(state.AccountHash) != 64 || len(state.ConfigHash) != 64 || !validRunID(state.RunID) {
				return errors.New("orphaned live canary state invalid")
			}
			createdAt, parseErr := time.Parse(time.RFC3339Nano, state.CreatedAt)
			if parseErr != nil || createdAt.After(time.Now().Add(time.Minute)) {
				return errors.New("orphaned live canary state invalid")
			}
			if time.Since(createdAt) <= 10*time.Minute {
				return errStateActive
			}
			return removeIfExists(statePath, LiveStateSchema)
		}
		return errors.New("cleanup journal invalid")
	}
	if j.Schema != JournalSchema || j.AgentType != "codex" || !validRunID(j.RunID) || j.ConfigHash != expectedConfigHash ||
		!validCanarySessionID(j.SessionID, "cutover-canary-") {
		return errors.New("cleanup journal invalid")
	}
	return cleanupLiveArtifacts(ctx, client, statePath, journalPath, j)
}

func routedHandoffConfigHash(cfg RoutedLegConfig) string {
	b, _ := json.Marshal(struct {
		BaseURL          string `json:"base_url"`
		AdminTokenFile   string `json:"admin_token_file"`
		TimeoutSeconds   int    `json:"timeout_seconds"`
		MaxResponseBytes int64  `json:"max_response_bytes"`
		StateFile        string `json:"state_file"`
		Journal          string `json:"cleanup_journal"`
		Model            string `json:"model"`
	}{
		BaseURL:          cfg.HTTP.BaseURL,
		AdminTokenFile:   cfg.HTTP.AdminTokenFile,
		TimeoutSeconds:   cfg.HTTP.TimeoutSeconds,
		MaxResponseBytes: cfg.HTTP.MaxResponseBytes,
		StateFile:        cfg.StateFile,
		Journal:          cfg.Journal,
		Model:            cfg.Model,
	})
	return hashProof("handoff-config", string(b))
}

func isolatedCleanupConfigHash(cfg IsolatedLegConfig) string {
	b, _ := json.Marshal(struct {
		HTTP                 HTTPConfig `json:"http"`
		Journal              string     `json:"cleanup_journal"`
		Model                string     `json:"model"`
		UnavailableAccountID string     `json:"unavailable_account_id"`
	}{
		HTTP:                 cfg.HTTP,
		Journal:              cfg.Journal,
		Model:                cfg.Model,
		UnavailableAccountID: cfg.UnavailableAccountID,
	})
	return hashProof("cleanup-config", string(b))
}

func claudeCleanupConfigHash(cfg ClaudeLegConfig) string {
	b, _ := json.Marshal(struct {
		HTTP    HTTPConfig `json:"http"`
		Journal string     `json:"cleanup_journal"`
		Model   string     `json:"model"`
	}{
		HTTP: cfg.HTTP, Journal: cfg.Journal, Model: cfg.Model,
	})
	return hashProof("claude-cleanup-config", string(b))
}

func cleanupLiveArtifacts(ctx context.Context, client *apiClient, statePath, journalPath string, expected cleanupJournal) error {
	if err := requireOwnedJournal(journalPath, expected); err != nil {
		return err
	}
	if err := client.deleteSession(ctx, expected.AgentType, expected.SessionID); err != nil {
		return err
	}
	if err := requireOwnedJournal(journalPath, expected); err != nil {
		return err
	}
	if err := removeIfExists(statePath, LiveStateSchema); err != nil {
		return err
	}
	if err := requireOwnedJournal(journalPath, expected); err != nil {
		return err
	}
	return removeIfExists(journalPath, JournalSchema)
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func Probe(ctx context.Context, configPath string) (PeerProbeResult, error) {
	var cfg PeerProbeConfig
	if err := loadConfig(configPath, &cfg, cfg.Schema); err != nil {
		return PeerProbeResult{}, err
	}
	client, err := newAPIClient(cfg.HTTP, true)
	if err != nil {
		return PeerProbeResult{}, err
	}
	result, err := client.probe(ctx)
	if err != nil {
		return PeerProbeResult{}, err
	}
	identity, err := currentExecutableIdentity()
	if err != nil {
		return PeerProbeResult{}, err
	}
	result.IdentityKind = identity.Kind
	result.ExecutableIdentity = identity.Value
	return result, nil
}

func runExisting(ctx context.Context, cfg ExistingLegConfig, start time.Time, runID string) (retErr error) {
	if cfg.WaitSeconds < 1 || cfg.WaitSeconds > 600 {
		return errors.New("existing-session wait must be 1..600 seconds")
	}
	readOnlyPaths := []string{cfg.configPath, cfg.HTTP.AdminTokenFile, cfg.SelectionFile}
	readOnlyPaths = append(readOnlyPaths, cfg.CandidateLogFiles...)
	if err := validateArtifactPaths(
		[]string{cfg.ProofFile, cfg.ChallengeFile, cfg.ChallengeFile + ".lock", cfg.WitnessFile},
		readOnlyPaths,
	); err != nil {
		return err
	}
	snapshots, err := snapshotCandidateLogs(cfg.CandidateLogFiles)
	if err != nil {
		return err
	}
	if cfg.MaxLogAppendBytes < 256 || cfg.MaxLogAppendBytes > 1<<20 {
		return errors.New("candidate log append cap must be 256 bytes..1 MiB")
	}
	var sel Selection
	if err := readStrictPrivateJSON(cfg.SelectionFile, &sel); err != nil || sel.Schema != "subrouter.cutover-canary-selection/v1" || sel.AgentType == "" || sel.SessionID == "" {
		return errors.New("existing-session selection invalid")
	}
	client, err := newAPIClient(cfg.HTTP, false)
	if err != nil {
		return err
	}
	statuses, err := client.sessionStatuses(ctx)
	if err != nil {
		return err
	}
	selected, ok := findSessionStatus(statuses, sel.AgentType, sel.SessionID)
	if !ok {
		return errors.New("selected existing session absent")
	}
	if selected.Active == nil {
		return errors.New("selected existing session activity unavailable")
	}
	if *selected.Active {
		return errors.New("selected existing session is active")
	}
	before := selected.Assignment
	nonce, err := randomHex(16)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	notBefore := now.Add(250 * time.Millisecond)
	expires := now.Add(time.Duration(cfg.WaitSeconds) * time.Second)
	challenge := Challenge{Schema: ChallengeSchema, RunID: runID, Nonce: nonce, Prompt: "Reply with exactly SUBROUTER_EXISTING_" + strings.ToUpper(nonce), NotBefore: notBefore.Format(time.RFC3339Nano), ExpiresAt: expires.Format(time.RFC3339Nano)}
	lease, err := publishChallenge(cfg.ChallengeFile, cfg.WitnessFile, challenge)
	if err != nil {
		return err
	}
	defer lease.Close()
	defer func() {
		_ = removeIfExists(cfg.ChallengeFile, ChallengeSchema)
		_ = removeIfExists(cfg.WitnessFile, WitnessSchema)
	}()
	marker := strings.TrimPrefix(challenge.Prompt, "Reply with exactly ")
	registration := cutoverChallengeRegistration{
		Schema:       "subrouter.cutover-challenge/v1",
		AgentType:    sel.AgentType,
		SessionID:    sel.SessionID,
		InputSHA256:  requestMarkerHash(challenge.Prompt),
		MarkerSHA256: requestMarkerHash(marker),
		NotBefore:    challenge.NotBefore,
		ExpiresAt:    challenge.ExpiresAt,
	}
	if err := client.setCutoverChallenge(ctx, http.MethodPost, registration); err != nil {
		return err
	}
	defer func() {
		if err := client.setCutoverChallenge(context.Background(), http.MethodDelete, registration); retErr == nil && err != nil {
			retErr = err
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return errors.New("existing-session witness canceled")
		case <-ticker.C:
			var witness Witness
			if err := readStrictPrivateJSON(cfg.WitnessFile, &witness); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					if time.Now().After(expires) {
						return errors.New("existing-session witness timed out")
					}
					continue
				}
				return errors.New("existing-session witness invalid")
			}
			if witness.Schema != WitnessSchema || witness.NonceHash != hashProof("witness", nonce) {
				return errors.New("existing-session witness mismatch")
			}
			matched, logErr := freshProxyLogEvidence(snapshots, sel.AgentType, sel.SessionID, requestMarkerHash(marker), notBefore, cfg.MaxLogAppendBytes)
			if logErr != nil {
				return logErr
			}
			if !matched {
				return errors.New("witness lacks new candidate proxy-request evidence")
			}
			all, err := client.sessions(ctx)
			if err != nil {
				return err
			}
			after, ok := findSession(all, sel.AgentType, sel.SessionID)
			if !ok {
				return errors.New("existing session disappeared")
			}
			return writePrivateJSON(cfg.ProofFile, proof{Schema: ProofSchema, Leg: "existing-session-next-turn", OK: true, Scope: "externally-coordinated-existing-session", DurationMillis: time.Since(start).Milliseconds(), SessionHash: hashProof(nonce, sel.SessionID), AccountHash: hashProof(nonce, after.AccountID), AccountChanged: before.AccountID != after.AccountID, RunHash: hashProof("run", runID), LogMatched: true})
		}
	}
}

func CreateWitness(challengePath, witnessPath string, input io.Reader) error {
	if err := validateArtifactPaths([]string{witnessPath}, []string{challengePath}); err != nil {
		return err
	}
	var challenge Challenge
	if err := readStrictPrivateJSON(challengePath, &challenge); err != nil || challenge.Schema != ChallengeSchema || challenge.Nonce == "" || !validRunID(challenge.RunID) {
		return errors.New("challenge invalid")
	}
	expires, err := time.Parse(time.RFC3339Nano, challenge.ExpiresAt)
	if err != nil || time.Now().After(expires) {
		return errors.New("challenge expired")
	}
	notBefore, err := time.Parse(time.RFC3339Nano, challenge.NotBefore)
	if err != nil || !notBefore.Before(expires) || time.Now().Before(notBefore) {
		return errors.New("challenge is not ready")
	}
	body, err := io.ReadAll(io.LimitReader(input, 4097))
	if err != nil {
		return errors.New("witness input failed")
	}
	if len(body) > 4096 {
		return errors.New("witness input too large")
	}
	marker := strings.TrimPrefix(challenge.Prompt, "Reply with exactly ")
	observed := string(body)
	if observed != marker && observed != marker+"\n" && observed != marker+"\r\n" {
		return errors.New("observed marker does not match challenge")
	}
	return createPrivateJSON(witnessPath, Witness{Schema: WitnessSchema, NonceHash: hashProof("witness", challenge.Nonce), ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)})
}

func ServeProbeResult(out io.Writer, result PeerProbeResult) error {
	return json.NewEncoder(out).Encode(result)
}
func ServeLegResult(out io.Writer, leg string) error {
	return json.NewEncoder(out).Encode(LegResult{Schema: LegSchema, Leg: leg, OK: true})
}
