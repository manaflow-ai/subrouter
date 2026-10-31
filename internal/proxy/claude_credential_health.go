package proxy

import (
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zlib"
	"github.com/klauspost/compress/zstd"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

// Claude credential death tracking.
//
// Anthropic ends a Claude OAuth credential in two ways the router can see:
// a 401 "OAuth access token has been revoked." and a 403 permission_error
// "OAuth authentication is currently not allowed for this organization."
// Either one is final for that credential. The router records the death in
// the existing revoked overlay of claude-cost-state.json (which keeps the
// account out of routing until a re-login changes the credential), logs one
// event per alive-to-dead transition, and appends the transition to an
// append-only ledger served at ClaudeCredentialDeathsPath.

// ClaudeCredentialDeathsPath serves the death ledger and the accounts that
// are dead right now.
const ClaudeCredentialDeathsPath = "/_subrouter/claude-credential-deaths"

const (
	claudeCredentialLedgerFile = "claude-credential-deaths.jsonl"
	// Not *.json: the account store dir parses every *.json file as an
	// account record.
	claudeCredentialLastOKFile = "claude-credential-last-ok.state"
	// claudeLivenessProbeInterval bounds liveness probes of accounts the
	// usage sweep otherwise skips (exhausted until a known reset).
	claudeLivenessProbeInterval = 15 * time.Minute
	// claudeLastOKFlushEvery bounds last_ok writes: the time only needs to
	// say roughly when the credential last worked.
	claudeLastOKFlushEvery = 5 * time.Minute
	// claudeAliveRecheckEvery bounds how often a stream of successful
	// responses rereads the dead overlay to notice a recovery.
	claudeAliveRecheckEvery = time.Minute
	claudeLedgerReadLimit   = 1000
	claudeErrorBodyMax      = 64 << 10
)

// Death sources.
const (
	claudeDeathSourceProxy = "proxy"
	claudeDeathSourceProbe = "probe"
)

// ClaudeCredentialEvent is one ledger line: a credential death or recovery.
type ClaudeCredentialEvent struct {
	At           time.Time  `json:"at"`
	Event        string     `json:"event"` // "dead" or "recovered"
	Account      string     `json:"account"`
	Kind         string     `json:"kind"`
	Status       int        `json:"status,omitempty"`
	ErrorType    string     `json:"error_type,omitempty"`
	ErrorMessage string     `json:"error_message,omitempty"`
	RequestID    string     `json:"request_id,omitempty"`
	Source       string     `json:"source"`
	FirstSeen    time.Time  `json:"first_seen"`
	LastOKAt     *time.Time `json:"last_ok_at,omitempty"`
	Session      string     `json:"session,omitempty"`
	Path         string     `json:"path,omitempty"`
}

// ClaudeDeadCredential is one account whose current credential is dead.
type ClaudeDeadCredential struct {
	Account      string     `json:"account"`
	Kind         string     `json:"kind"`
	FirstSeen    time.Time  `json:"first_seen"`
	Status       int        `json:"status,omitempty"`
	ErrorType    string     `json:"error_type,omitempty"`
	ErrorMessage string     `json:"error_message,omitempty"`
	RequestID    string     `json:"request_id,omitempty"`
	Source       string     `json:"source,omitempty"`
	LastOKAt     *time.Time `json:"last_ok_at,omitempty"`
}

// ClaudeCredentialDeathReport is the ClaudeCredentialDeathsPath payload.
type ClaudeCredentialDeathReport struct {
	Dead   []ClaudeDeadCredential  `json:"dead"`
	Events []ClaudeCredentialEvent `json:"events"`
}

// claudeDeathEvidence is what one upstream answer proves.
type claudeDeathEvidence struct {
	Kind         string
	Status       int
	ErrorType    string
	ErrorMessage string
	RequestID    string
	Source       string
	Session      string
	Path         string
}

// claudeCredentialHealth keeps the death ledger, the last successful
// upstream answer per account, and the liveness probe schedule.
type claudeCredentialHealth struct {
	ledgerPath string
	lastOKPath string

	logMu  sync.Mutex
	logger *slog.Logger

	ledgerMu sync.Mutex

	mu            sync.Mutex
	lastOK        map[string]time.Time
	lastOKLoaded  bool
	lastOKFlushed map[string]time.Time
	aliveChecked  map[string]time.Time
	probeAt       map[string]time.Time
	flushing      bool
}

func newClaudeCredentialHealth(dir string, logger *slog.Logger) *claudeCredentialHealth {
	h := &claudeCredentialHealth{
		logger:        logger,
		lastOK:        map[string]time.Time{},
		lastOKFlushed: map[string]time.Time{},
		aliveChecked:  map[string]time.Time{},
		probeAt:       map[string]time.Time{},
	}
	if dir != "" {
		h.ledgerPath = filepath.Join(dir, claudeCredentialLedgerFile)
		h.lastOKPath = filepath.Join(dir, claudeCredentialLastOKFile)
	}
	return h
}

func (h *claudeCredentialHealth) setLogger(logger *slog.Logger) {
	if h == nil || logger == nil {
		return
	}
	h.logMu.Lock()
	h.logger = logger
	h.logMu.Unlock()
}

func (h *claudeCredentialHealth) log() *slog.Logger {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	if h.logger != nil {
		return h.logger
	}
	return slog.Default()
}

// claudeHealth returns the ref's shared tracker, created on first use under
// the account store dir.
func (r *AccountRef) claudeHealth() *claudeCredentialHealth {
	if r == nil {
		return nil
	}
	if h := r.claudeHealthTracker.Load(); h != nil {
		return h
	}
	r.claudeHealthTracker.CompareAndSwap(nil, newClaudeCredentialHealth(r.store.Dir, nil))
	return r.claudeHealthTracker.Load()
}

// claudeHealth returns the tracker for this server: the ref's when it has
// one, else the one Handler created.
func (s Server) claudeHealth() *claudeCredentialHealth {
	if s.AccountRef != nil {
		return s.AccountRef.claudeHealth()
	}
	return s.claudeHealthFallback
}

func (h *claudeCredentialHealth) loadLastOKLocked() {
	if h.lastOKLoaded {
		return
	}
	h.lastOKLoaded = true
	if h.lastOKPath == "" {
		return
	}
	data, err := os.ReadFile(h.lastOKPath)
	if err != nil {
		return
	}
	var stored claudeLastOKFileShape
	if json.Unmarshal(data, &stored) != nil {
		return
	}
	for id, at := range stored.LastOK {
		if at.After(h.lastOK[id]) {
			h.lastOK[id] = at
		}
		h.lastOKFlushed[id] = at
	}
}

func (h *claudeCredentialHealth) lastOKAt(accountID string) *time.Time {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.loadLastOKLocked()
	at, ok := h.lastOK[accountID]
	if !ok {
		return nil
	}
	at = at.UTC()
	return &at
}

// noteOK records a successful upstream answer and reports whether the
// caller should check the dead overlay for a recovery now.
func (h *claudeCredentialHealth) noteOK(accountID string, now time.Time) bool {
	h.mu.Lock()
	h.loadLastOKLocked()
	h.lastOK[accountID] = now
	flush := !h.flushing && h.lastOKPath != "" && now.Sub(h.lastOKFlushed[accountID]) >= claudeLastOKFlushEvery
	var snapshot map[string]time.Time
	if flush {
		h.flushing = true
		snapshot = make(map[string]time.Time, len(h.lastOK))
		for id, at := range h.lastOK {
			snapshot[id] = at.UTC()
		}
	}
	recheck := now.Sub(h.aliveChecked[accountID]) >= claudeAliveRecheckEvery
	if recheck {
		h.aliveChecked[accountID] = now
	}
	h.mu.Unlock()
	if flush {
		go h.flushLastOK(snapshot)
	}
	return recheck
}

func (h *claudeCredentialHealth) flushLastOK(snapshot map[string]time.Time) {
	err := writeFileAtomic(h.lastOKPath, claudeLastOKFileShape{LastOK: snapshot})
	h.mu.Lock()
	h.flushing = false
	if err == nil {
		for id, at := range snapshot {
			h.lastOKFlushed[id] = at
		}
	}
	h.mu.Unlock()
	if err != nil {
		h.log().Warn("claude credential last-ok write failed", "path", h.lastOKPath, "error", err)
	}
}

type claudeLastOKFileShape struct {
	LastOK map[string]time.Time `json:"claude_last_ok"`
}

func writeFileAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// A unique dot-file temp keeps two workers (during a swap) from writing
	// one temp file, and keeps it out of the account store's listing.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".claude-last-ok-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// claimLivenessProbe allows one liveness probe per account per
// claudeLivenessProbeInterval.
func (h *claudeCredentialHealth) claimLivenessProbe(accountID string, now time.Time) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if last, ok := h.probeAt[accountID]; ok && now.Sub(last) < claudeLivenessProbeInterval {
		return false
	}
	h.probeAt[accountID] = now
	return true
}

// appendLedger writes one transition. O_APPEND keeps whole lines from the
// old and new worker apart during a swap.
func (h *claudeCredentialHealth) appendLedger(event ClaudeCredentialEvent) {
	if h.ledgerPath == "" {
		return
	}
	line, err := json.Marshal(event)
	if err != nil {
		return
	}
	h.ledgerMu.Lock()
	defer h.ledgerMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(h.ledgerPath), 0o700); err != nil {
		return
	}
	file, err := os.OpenFile(h.ledgerPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		h.log().Warn("claude credential ledger write failed", "path", h.ledgerPath, "error", err)
		return
	}
	defer file.Close()
	_, _ = file.Write(append(line, '\n'))
}

// readLedger returns the newest ledger lines, oldest first.
func (h *claudeCredentialHealth) readLedger(account string, limit int) []ClaudeCredentialEvent {
	out := []ClaudeCredentialEvent{}
	if h == nil || h.ledgerPath == "" {
		return out
	}
	file, err := os.Open(h.ledgerPath)
	if err != nil {
		return out
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var event ClaudeCredentialEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if account != "" && event.Account != account {
			continue
		}
		out = append(out, event)
		if len(out) > 2*limit {
			out = append(out[:0:0], out[len(out)-limit:]...)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// markClaudeCredentialDead records a dead Claude OAuth credential in the
// revoked overlay, which keeps it out of routing until its credential
// changes. The first answer for a credential logs and ledgers the death;
// repeats are silent.
func markClaudeCredentialDead(costs *claudeCostStore, health *claudeCredentialHealth, account accounts.Account, identity string, evidence claudeDeathEvidence) bool {
	if costs == nil || account.ID == "" || identity == "" || evidence.Kind == "" {
		return false
	}
	stored, transition := costs.markDead(account.ID, identity, claudeRevokedState{
		Kind: evidence.Kind, Status: evidence.Status, ErrorType: evidence.ErrorType,
		ErrorMessage: evidence.ErrorMessage, RequestID: evidence.RequestID, Source: evidence.Source,
	})
	if !transition || health == nil {
		return transition
	}
	event := ClaudeCredentialEvent{
		At: time.Now().UTC(), Event: "dead", Account: account.ID, Kind: stored.kind(),
		Status: evidence.Status, ErrorType: evidence.ErrorType, ErrorMessage: evidence.ErrorMessage,
		RequestID: evidence.RequestID, Source: evidence.Source, FirstSeen: stored.FirstSeen.UTC(),
		LastOKAt: health.lastOKAt(account.ID),
	}
	if evidence.Source == claudeDeathSourceProxy {
		event.Session, event.Path = evidence.Session, evidence.Path
	}
	fields := []any{
		"account", event.Account, "kind", event.Kind, "status", event.Status,
		"error_type", event.ErrorType, "error_message", event.ErrorMessage, "request_id", event.RequestID,
		"source", event.Source, "first_seen", event.FirstSeen.Format(time.RFC3339),
		"last_ok_at", formatOptionalTime(event.LastOKAt),
	}
	if event.Source == claudeDeathSourceProxy {
		fields = append(fields, "session", event.Session, "path", event.Path)
	}
	health.log().Warn("claude account credential dead", fields...)
	health.appendLedger(event)
	return true
}

func formatOptionalTime(at *time.Time) string {
	if at == nil {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

// noteClaudeCredentialAlive records a successful Messages answer for the
// account's current credential. When the account was recorded dead with an
// older credential (a re-login replaced it), the record is cleared and the
// recovery logged once. A record for the current credential is never
// cleared here: a live answer for it can only be one that was in flight
// when the death was recorded, since dead credentials are not routed or
// probed.
func noteClaudeCredentialAlive(costs *claudeCostStore, health *claudeCredentialHealth, account accounts.Account, credentialIdentity func() string, source string) {
	if health == nil || account.ID == "" {
		return
	}
	now := time.Now()
	if !health.noteOK(account.ID, now) || costs == nil {
		return
	}
	state, dead := costs.snapshot().Revoked[account.ID]
	if !dead {
		return
	}
	identity := credentialIdentity()
	if identity == "" || state.CredentialFingerprint == claudeCredentialFingerprint(identity) {
		return
	}
	prior, cleared := costs.clearDead(account.ID)
	if !cleared {
		return
	}
	at := now.UTC()
	event := ClaudeCredentialEvent{
		At: at, Event: "recovered", Account: account.ID, Kind: prior.kind(), Source: source,
		FirstSeen: prior.FirstSeen.UTC(), LastOKAt: &at,
	}
	health.log().Info("claude account credential recovered",
		"account", event.Account, "kind", event.Kind, "source", source,
		"dead_since", event.FirstSeen.Format(time.RFC3339), "last_ok_at", at.Format(time.RFC3339))
	health.appendLedger(event)
}

// claudeOAuthDeathCandidate reports whether account is a Claude OAuth
// account, the only kind whose death this tracks.
func (s Server) claudeOAuthDeathCandidate(account accounts.Account) bool {
	return account.ID != "" && accountProviderOrCodex(account) == accounts.ProviderClaude &&
		s.claudeAuthMode(account) != accounts.AuthModeAPIKey
}

// noteClaudeCredentialResponse classifies a Claude 401/403 answer by its
// error envelope (decodedBody: already decoded with decodedUpstreamBody) and
// records a credential death. It returns the death kind, or "" when the
// answer is not one.
func (s Server) noteClaudeCredentialResponse(account accounts.Account, status int, header http.Header, decodedBody []byte, session, path string) string {
	if (status != http.StatusUnauthorized && status != http.StatusForbidden) || !s.claudeOAuthDeathCandidate(account) {
		return ""
	}
	envelope := agentclaude.ParseErrorEnvelope(decodedBody)
	if envelope.RequestID == "" {
		envelope.RequestID = agentclaude.BoundedToken(header.Get("request-id"))
	}
	kind := agentclaude.ClassifyCredentialDeath(status, envelope.Type, envelope.Message)
	if kind == "" {
		return ""
	}
	identity := account.CredentialIdentity()
	if identity == "" {
		identity = s.claudeCredentialIdentity(account.ID)
	}
	costs := s.claudeCosts
	if costs == nil {
		costs = s.AccountRef.claudeCostStore()
	}
	markClaudeCredentialDead(costs, s.claudeHealth(), account, identity, claudeDeathEvidence{
		Kind: kind, Status: status, ErrorType: envelope.Type, ErrorMessage: envelope.Message,
		RequestID: envelope.RequestID, Source: claudeDeathSourceProxy, Session: session, Path: path,
	})
	return kind
}

// noteClaudeProxySuccess records a successful Claude Messages answer. It
// runs on every 2xx, so it skips the auth-mode lookup: an API key never has
// a dead record, and its last-ok time is harmless.
func (s Server) noteClaudeProxySuccess(account accounts.Account) {
	if account.ID == "" || account.AuthMode == accounts.AuthModeAPIKey {
		return
	}
	costs := s.claudeCosts
	if costs == nil {
		costs = s.AccountRef.claudeCostStore()
	}
	noteClaudeCredentialAlive(costs, s.claudeHealth(), account, func() string {
		if identity := account.CredentialIdentity(); identity != "" {
			return identity
		}
		return s.claudeCredentialIdentity(account.ID)
	}, claudeDeathSourceProxy)
}

// claudeFableProbe runs the Messages probe for a Claude OAuth account and
// records credential death or liveness. Inference-only tokens blocked by
// Fable's credit gate use an ordinary model to discover account-wide quota.
func (r *AccountRef) claudeFableProbe(ctx context.Context, client *http.Client, account accounts.Account) ([]accounts.UsageWindow, error) {
	result, err := agentclaude.FableProbe(ctx, client, account.Token)
	if err != nil {
		return nil, err
	}
	r.noteClaudeProbeResult(account, result)
	if result.CreditsRequired && !fableProbeHasPrimaryWindows(result.Windows) && r.claudeUsageScopeDeniedFor(ctx, account) {
		// A setup token cannot read /usage, and Fable's credit gate says
		// nothing about its ordinary subscription quota. Bootstrap that quota
		// with one ordinary-model probe instead of excluding the account.
		primary, err := agentclaude.PrimaryUsageProbe(ctx, client, account.Token)
		if err != nil {
			return nil, err
		}
		r.noteClaudeProbeResult(account, primary)
		if primary.Failure != nil {
			return nil, primary.Failure
		}
		result.Windows = mergeUsageWindows(result.Windows, primary.Windows)
	}
	if len(result.Windows) > 0 {
		return result.Windows, nil
	}
	if result.Failure != nil {
		return nil, result.Failure
	}
	return nil, nil
}

func (r *AccountRef) noteClaudeProbeResult(account accounts.Account, result agentclaude.FableProbeResult) {
	if r == nil || account.ID == "" || account.AuthMode == accounts.AuthModeAPIKey {
		return
	}
	costs, health := r.claudeCostStore(), r.claudeHealth()
	if result.Alive() {
		noteClaudeCredentialAlive(costs, health, account, account.CredentialIdentity, claudeDeathSourceProbe)
		return
	}
	failure := result.Failure
	if failure == nil {
		return
	}
	kind := agentclaude.ClassifyCredentialDeath(failure.StatusCode, failure.ErrorType, failure.Message)
	if kind == "" {
		return
	}
	markClaudeCredentialDead(costs, health, account, account.CredentialIdentity(), claudeDeathEvidence{
		Kind: kind, Status: failure.StatusCode, ErrorType: failure.ErrorType, ErrorMessage: failure.Message,
		RequestID: failure.RequestID, Source: claudeDeathSourceProbe,
	})
}

// probeClaudeLiveness checks a Claude account that the usage sweep skips
// (exhausted until a known reset) so its credential death is noticed
// without traffic. Its answer is liveness evidence only, never a score.
func (r *AccountRef) probeClaudeLiveness(ctx context.Context, client *http.Client, account accounts.Account) {
	result, err := agentclaude.FableProbe(ctx, client, account.Token)
	if err != nil {
		return
	}
	r.noteClaudeProbeResult(account, result)
}

// decodedUpstreamBody undoes the response Content-Encoding of an error body
// prefix so classifiers and logs see the JSON envelope. The Go transport
// already decodes gzip it asked for itself; this covers an upstream that
// answered an encoding the client offered. It returns body unchanged when
// the encoding is unknown or the bytes do not decode.
func decodedUpstreamBody(body []byte, header http.Header) []byte {
	encoding := strings.ToLower(strings.TrimSpace(header.Get("Content-Encoding")))
	if len(body) == 0 || encoding == "" || encoding == "identity" {
		return body
	}
	var reader io.Reader
	switch encoding {
	case "gzip", "x-gzip":
		gz, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return body
		}
		defer gz.Close()
		reader = gz
	case "deflate":
		// HTTP deflate is zlib-wrapped; some servers send raw deflate.
		if zr, err := zlib.NewReader(bytes.NewReader(body)); err == nil {
			defer zr.Close()
			reader = zr
		} else {
			fr := flate.NewReader(bytes.NewReader(body))
			defer fr.Close()
			reader = fr
		}
	case "br":
		reader = brotli.NewReader(bytes.NewReader(body))
	case "zstd":
		decoder, err := zstd.NewReader(bytes.NewReader(body), zstd.WithDecoderMaxMemory(claudeErrorBodyMax*4))
		if err != nil {
			return body
		}
		defer decoder.Close()
		reader = decoder
	default:
		return body
	}
	decoded, err := io.ReadAll(io.LimitReader(reader, claudeErrorBodyMax))
	if len(decoded) == 0 || (err != nil && !json.Valid(decoded)) {
		// A failed decode (for example raw-deflate garbage from a body that
		// was never compressed) keeps the original bytes.
		return body
	}
	return decoded
}

// claudeDeadCredentials lists accounts whose current credential is dead.
func (s Server) claudeDeadCredentials(account string) []ClaudeDeadCredential {
	costs := s.claudeCosts
	if costs == nil {
		costs = s.AccountRef.claudeCostStore()
	}
	out := []ClaudeDeadCredential{}
	if costs == nil {
		return out
	}
	file := costs.snapshot()
	health := s.claudeHealth()
	for id, state := range file.Revoked {
		if account != "" && id != account {
			continue
		}
		if _, dead := claudeRevokedSince(file, id, s.claudeCredentialIdentity(id)); !dead {
			continue
		}
		out = append(out, ClaudeDeadCredential{
			Account: id, Kind: state.kind(), FirstSeen: state.FirstSeen.UTC(), Status: state.Status,
			ErrorType: state.ErrorType, ErrorMessage: state.ErrorMessage, RequestID: state.RequestID,
			Source: state.Source, LastOKAt: health.lastOKAt(id),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].FirstSeen.Equal(out[j].FirstSeen) {
			return out[i].FirstSeen.Before(out[j].FirstSeen)
		}
		return out[i].Account < out[j].Account
	})
	return out
}

func (s Server) handleClaudeCredentialDeaths(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	values := r.URL.Query()
	limit := claudeLedgerReadLimit
	if raw := values.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > claudeLedgerReadLimit {
			http.Error(w, "limit must be 1..1000", http.StatusBadRequest)
			return
		}
		limit = parsed
	}
	account := strings.TrimSpace(values.Get("account"))
	writeJSON(w, ClaudeCredentialDeathReport{
		Dead:   s.claudeDeadCredentials(account),
		Events: s.claudeHealth().readLedger(account, limit),
	})
}
