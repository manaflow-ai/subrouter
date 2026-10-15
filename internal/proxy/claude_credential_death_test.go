package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

const (
	anthropicRevokedBodyWithID = `{"type":"error","error":{"type":"authentication_error","message":"OAuth access token has been revoked."},"request_id":"req_revoked_1"}`
	anthropicOrgOAuthOffBody   = `{"type":"error","error":{"type":"permission_error","message":"OAuth authentication is currently not allowed for this organization."},"request_id":"req_org_1"}`
)

func brotliBytes(t *testing.T, body string) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := brotli.NewWriter(&out)
	if _, err := writer.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func readCredentialLedger(t *testing.T, path string) []ClaudeCredentialEvent {
	t.Helper()
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var events []ClaudeCredentialEvent
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var event ClaudeCredentialEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("ledger line %q: %v", scanner.Text(), err)
		}
		events = append(events, event)
	}
	return events
}

func getCredentialDeaths(t *testing.T, baseURL string) ClaudeCredentialDeathReport {
	t.Helper()
	response, err := http.Get(baseURL + ClaudeCredentialDeathsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var report ClaudeCredentialDeathReport
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&report) != nil {
		t.Fatalf("GET %s = %d", ClaudeCredentialDeathsPath, response.StatusCode)
	}
	return report
}

// TestClaudeCredentialDeathsFromCompressedUpstreamAnswers drives requests
// through Server.Handler() against an Anthropic stand-in that answers one
// account with a brotli-compressed 401 "token has been revoked" and another
// with the 403 "OAuth authentication is currently not allowed for this
// organization". Both requests must fail over; both accounts must leave
// routing; each death must produce one log event, one ledger line, and an
// entry in the read-only endpoint, with the decoded error type, message,
// and request id but no credential.
func TestClaudeCredentialDeathsFromCompressedUpstreamAnswers(t *testing.T) {
	var revokedHits, orgHits, healthyHits atomic.Int32
	revokedBody := brotliBytes(t, anthropicRevokedBodyWithID)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		auth := r.Header.Get("Authorization")
		switch {
		case strings.Contains(auth, "tok-revoked"):
			revokedHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "br")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write(revokedBody)
		case strings.Contains(auth, "tok-orgoff"):
			orgHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("request-id", "req_org_1")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(anthropicOrgOAuthOffBody))
		default:
			healthyHits.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"msg_ok","type":"message"}`))
		}
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store, err := session.NewStore(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	for sessionID, account := range map[string]string{"s-revoked": "revoked@example.com", "s-org": "orgoff@example.com"} {
		if _, err := store.Put("claude", sessionID, account, ""); err != nil {
			t.Fatal(err)
		}
	}
	var logMu sync.Mutex
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&lockedWriter{mu: &logMu, w: &logBuf}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	server := Server{
		ClaudeUpstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "revoked@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-revoked"},
			{ID: "orgoff@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-orgoff"},
			{ID: "healthy@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-healthy"},
		},
		Sessions: store,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "revoked@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
			{AccountID: "orgoff@example.com", Provider: accounts.ProviderClaude, Headroom: 0.9, ShortHeadroom: 0.9},
			{AccountID: "healthy@example.com", Provider: accounts.ProviderClaude, Headroom: 0.5, ShortHeadroom: 0.5},
		})),
		MaxBodyBytes: 1 << 20,
		Logger:       logger,
		claudeCosts:  newClaudeCostStore(filepath.Join(dir, "claude-cost-state.json")),
	}
	subrouter := httptest.NewServer(server.Handler())
	defer subrouter.Close()

	send := func(sessionID string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8","messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Subrouter-Agent", "claude")
		req.Header.Set("X-Subrouter-Session", sessionID)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		_, _ = io.ReadAll(response.Body)
		return response.StatusCode
	}
	for _, sessionID := range []string{"s-revoked", "s-org"} {
		if status := send(sessionID); status != http.StatusOK {
			t.Fatalf("%s status = %d, want 200 after failover", sessionID, status)
		}
	}
	// New sessions must never reach a dead account again.
	for _, sessionID := range []string{"s-new-1", "s-new-2", "s-new-3"} {
		if status := send(sessionID); status != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", sessionID, status)
		}
	}
	if revokedHits.Load() != 1 || orgHits.Load() != 1 {
		t.Fatalf("dead account hits revoked=%d org=%d, want 1 each (excluded after the first answer)", revokedHits.Load(), orgHits.Load())
	}

	logMu.Lock()
	logs := logBuf.String()
	logMu.Unlock()
	if got := strings.Count(logs, "claude account credential dead"); got != 2 {
		t.Fatalf("death events = %d, want one per account; logs=\n%s", got, logs)
	}
	for _, want := range []string{
		"kind=revoked", "error_type=authentication_error", "request_id=req_revoked_1",
		"kind=org_oauth_disallowed", "error_type=permission_error", "request_id=req_org_1",
		"source=proxy", "session=s-revoked", "path=/v1/messages",
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("logs missing %q; logs=\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "tok-") {
		t.Fatalf("a credential reached the log; logs=\n%s", logs)
	}

	ledger := readCredentialLedger(t, filepath.Join(dir, claudeCredentialLedgerFile))
	if len(ledger) != 2 {
		t.Fatalf("ledger = %+v, want one line per death", ledger)
	}
	kinds := map[string]ClaudeCredentialEvent{}
	for _, event := range ledger {
		if event.Event != "dead" || event.Source != "proxy" || event.FirstSeen.IsZero() {
			t.Fatalf("ledger event = %+v, want a proxy-sourced death", event)
		}
		kinds[event.Account] = event
	}
	if kinds["revoked@example.com"].Kind != agentclaude.CredentialDeathRevoked ||
		kinds["revoked@example.com"].ErrorMessage != "OAuth access token has been revoked." ||
		kinds["orgoff@example.com"].Kind != agentclaude.CredentialDeathOrgOAuthDisallowed ||
		kinds["orgoff@example.com"].Status != http.StatusForbidden {
		t.Fatalf("ledger kinds = %+v", kinds)
	}

	report := getCredentialDeaths(t, subrouter.URL)
	if len(report.Dead) != 2 || len(report.Events) != 2 {
		t.Fatalf("endpoint = %+v, want both dead accounts and both events", report)
	}
	dead := map[string]ClaudeDeadCredential{}
	for _, entry := range report.Dead {
		dead[entry.Account] = entry
	}
	if dead["revoked@example.com"].Kind != agentclaude.CredentialDeathRevoked ||
		dead["orgoff@example.com"].Kind != agentclaude.CredentialDeathOrgOAuthDisallowed ||
		dead["orgoff@example.com"].RequestID != "req_org_1" {
		t.Fatalf("endpoint dead = %+v", dead)
	}
	statuses := server.withClaudeCostTiers([]AccountUsageStatus{
		{AccountStatus: AccountStatus{ID: "orgoff@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}},
	})
	if statuses[0].RevokedSince.IsZero() || statuses[0].RevokedKind != agentclaude.CredentialDeathOrgOAuthDisallowed {
		t.Fatalf("status = %+v, want the org-disabled death with its first-seen time", statuses[0])
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// claudeProbeTransport answers the Messages probe with a configurable
// status and body and the usage endpoint with healthy usage.
type claudeProbeTransport struct {
	mu          sync.Mutex
	probeStatus int
	probeBody   string
	probeCalls  map[string]int
}

func (c *claudeProbeTransport) set(status int, body string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.probeStatus, c.probeBody = status, body
}

func (c *claudeProbeTransport) calls(token string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.probeCalls[token]
}

func (c *claudeProbeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	if !strings.HasSuffix(req.URL.Path, "/v1/messages") {
		return usageOKResponse(), nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.probeCalls == nil {
		c.probeCalls = map[string]int{}
	}
	c.probeCalls[strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")]++
	return &http.Response{
		StatusCode: c.probeStatus, Status: http.StatusText(c.probeStatus),
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(strings.NewReader(c.probeBody)),
	}, nil
}

// TestClaudeLivenessProbeFindsDeathOfExhaustedAccount covers an account
// the usage sweep skips because it waits for a known quota reset. Its
// credential death must still be found without traffic, by at most one
// probe per interval, and a re-login must log a recovery.
func TestClaudeLivenessProbeFindsDeathOfExhaustedAccount(t *testing.T) {
	transport := &claudeProbeTransport{}
	transport.set(http.StatusUnauthorized, anthropicRevokedBodyWithID)
	ref := cacheTestAccountRef(t, transport)
	var logMu sync.Mutex
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&lockedWriter{mu: &logMu, w: &logBuf}, nil))
	account := accounts.Account{ID: "claude@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok"}
	server := Server{
		AccountRef:   ref,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{futureExhaustedQuota(account, time.Now().Add(3*time.Hour))})),
		Logger:       logger,
	}
	ref.claudeHealth().setLogger(logger)
	deaths := func() ClaudeCredentialDeathReport {
		t.Helper()
		recorder := httptest.NewRecorder()
		server.handleClaudeCredentialDeaths(recorder, httptest.NewRequest(http.MethodGet, ClaudeCredentialDeathsPath, nil))
		var report ClaudeCredentialDeathReport
		if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &report) != nil {
			t.Fatalf("GET %s = %d", ClaudeCredentialDeathsPath, recorder.Code)
		}
		return report
	}

	for pass := 0; pass < 4; pass++ {
		server.scoreAccounts(context.Background(), []accounts.Account{account})
	}
	if got := transport.calls("tok"); got != 1 {
		t.Fatalf("liveness probes = %d, want exactly one within the probe interval", got)
	}
	logMu.Lock()
	logs := logBuf.String()
	logMu.Unlock()
	if got := strings.Count(logs, "claude account credential dead"); got != 1 ||
		!strings.Contains(logs, "source=probe") || !strings.Contains(logs, "request_id=req_revoked_1") {
		t.Fatalf("death events = %d, want one probe-sourced event; logs=\n%s", got, logs)
	}
	ledgerPath := filepath.Join(ref.store.Dir, claudeCredentialLedgerFile)
	if ledger := readCredentialLedger(t, ledgerPath); len(ledger) != 1 || ledger[0].Source != "probe" || ledger[0].Kind != agentclaude.CredentialDeathRevoked {
		t.Fatalf("ledger = %+v, want one probe-sourced revoked death", ledger)
	}
	report := deaths()
	if len(report.Dead) != 1 || report.Dead[0].Account != account.ID || report.Dead[0].Source != "probe" {
		t.Fatalf("endpoint = %+v, want the dead account", report)
	}

	// A re-login replaces the credential; its first live Messages answer
	// clears the death and logs a recovery.
	transport.set(http.StatusOK, `{"id":"msg_probe","type":"message"}`)
	if err := ref.claudeStore.ImportProfileCredential(account.ID, agentclaude.CredentialInfo{
		AccessToken: "tok-relogged", RefreshToken: "ref-relogged", SubscriptionType: "max",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	relogged := account
	relogged.Token = "tok-relogged"
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: account.ID, Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
	}))
	server.scoreAccounts(context.Background(), []accounts.Account{relogged})
	if transport.calls("tok-relogged") == 0 {
		t.Fatal("re-logged credential was not probed")
	}
	logMu.Lock()
	logs = logBuf.String()
	logMu.Unlock()
	if got := strings.Count(logs, "claude account credential recovered"); got != 1 {
		t.Fatalf("recovery events = %d, want 1; logs=\n%s", got, logs)
	}
	ledger := readCredentialLedger(t, ledgerPath)
	if len(ledger) != 2 || ledger[1].Event != "recovered" || ledger[1].LastOKAt == nil {
		t.Fatalf("ledger = %+v, want the death then one recovery", ledger)
	}
	if report := deaths(); len(report.Dead) != 0 || len(report.Events) != 2 {
		t.Fatalf("endpoint after re-login = %+v, want no dead account and both events", report)
	}
}
