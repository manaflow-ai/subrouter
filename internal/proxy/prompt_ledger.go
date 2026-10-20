package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/attribution"
	"github.com/manaflow-ai/subrouter/session"
)

// PromptsPath serves the prompt ledger: who sent which prompt, by time range.
const PromptsPath = "/_subrouter/prompts"

// promptPeekMaxBytes bounds the wire body read for attribution. Bigger bodies
// are still proxied; they are just not attributed. The handler only copies
// wire bytes; decoding and parsing happen on the ledger's worker.
const promptPeekMaxBytes = int64(8 << 20)

// TailnetIdentify adapts a TailnetAuthorizer to the ledger. It names every
// resolvable peer, including one outside an admin allowlist: attribution is
// about who sent a request, not whether they are an admin.
func TailnetIdentify(auth TailnetAuthorizer) attribution.IdentifyFunc {
	if auth == nil {
		return nil
	}
	return func(ctx context.Context, remoteAddr string) (attribution.Identity, bool) {
		identity, _ := auth.Authorize(ctx, remoteAddr)
		if strings.TrimSpace(identity.LoginName) == "" && len(identity.Tags) == 0 {
			return attribution.Identity{}, false
		}
		return attribution.Identity{LoginName: identity.LoginName, NodeName: identity.NodeName, Tags: identity.Tags}, true
	}
}

// submitPromptAttribution hands a Claude Messages body to the ledger. The
// ledger parses, identifies and writes on its own goroutine.
func (s Server) submitPromptAttribution(r *http.Request, agentType, sessionID, model string) {
	if s.PromptLedger == nil || r == nil || r.Method != http.MethodPost ||
		!(strings.HasSuffix(r.URL.Path, "/v1/messages") || strings.HasSuffix(r.URL.Path, "/responses")) {
		return
	}
	if r.ContentLength > promptPeekMaxBytes {
		return
	}
	if !strings.Contains(r.Header.Get("Content-Type"), "json") {
		return
	}
	body, ok := peekWireBody(r, promptPeekMaxBytes)
	if !ok {
		return
	}
	s.PromptLedger.Submit(attribution.Request{
		RemoteAddr:      r.RemoteAddr,
		Agent:           agentType,
		SessionID:       sessionID,
		ClientSessionID: clientSessionHeader(r.Header),
		Model:           model,
		Path:            r.URL.Path,
		ClaimedUser:     session.ExtractUserEmail(r),
		UserAgent:       r.Header.Get("User-Agent"),
		ContentEncoding: r.Header.Get("Content-Encoding"),
	}, body)
}

// peekWireBody reads up to max bytes of the body and puts them back in front
// of the rest, so the proxied body is byte-identical. ok is false for an empty
// body or one longer than max.
func peekWireBody(r *http.Request, max int64) ([]byte, bool) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, false
	}
	wire, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{Reader: io.MultiReader(bytes.NewReader(wire), r.Body), Closer: r.Body}
	if err != nil || len(wire) == 0 || int64(len(wire)) > max {
		return nil, false
	}
	return bytes.Clone(wire), true
}

// submitWebSocketPromptAttribution hands a client response.create frame to
// the ledger. The frame is a pooled buffer, so it is copied first; frames that
// are not response.create are not copied at all.
func (s Server) submitWebSocketPromptAttribution(state *webSocketModelState, agentType, sessionID, model, claimedUser string, frame []byte) {
	if s.PromptLedger == nil || state == nil || !bytes.Contains(frame, []byte(`response.create`)) {
		return
	}
	s.PromptLedger.Submit(attribution.Request{
		RemoteAddr:  state.peerAddr,
		Agent:       agentType,
		SessionID:   sessionID,
		Model:       model,
		Path:        "websocket",
		ClaimedUser: claimedUser,
	}, bytes.Clone(frame))
}

func clientSessionHeader(header http.Header) string {
	for _, name := range []string{"X-Claude-Code-Session-Id", "Session_id", "Session-Id", "X-Codex-Session-Id"} {
		if value := strings.TrimSpace(header.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

// transcriptReaderPaths are the routes that serve raw transcripts (every
// person's prompts and replies). They take the prompt-reader rule, not the
// admin rule, when tailnet auth is on.
func transcriptReaderPaths() []string {
	return []string{"/_subrouter/transcripts", "/_subrouter/transcripts/"}
}

// requireTranscriptReader guards transcript routes. Without tailnet auth the
// admin rule is unchanged (token or local configs); with it, every tailnet
// peer is an admin, so only prompt readers and the admin token pass.
func (s Server) requireTranscriptReader(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	admin := s.requireAdmin(next)
	return func(w http.ResponseWriter, r *http.Request) {
		if s.TailnetAuth == nil {
			admin(w, r)
			return
		}
		if !s.authorizePromptReader(r) {
			http.Error(w, "transcripts are limited to prompt readers", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// authorizePromptReader admits the admin token, tailnet logins listed in
// PromptLedgerReaders, and loopback only when PromptLedgerLoopbackReader is
// set. Tailnet admin status alone is not enough: on a tailnet-auth gateway
// every team member is an admin, and on a shared host every local process is
// loopback.
func (s Server) authorizePromptReader(r *http.Request) bool {
	if s.matchesConfiguredAdminToken(r) || (s.PromptLedgerLoopbackReader && isLoopbackRemote(r.RemoteAddr)) {
		return true
	}
	if s.TailnetAuth == nil || len(s.PromptLedgerReaders) == 0 {
		return false
	}
	identity, _ := s.TailnetAuth.Authorize(r.Context(), r.RemoteAddr)
	login := strings.TrimSpace(identity.LoginName)
	if login == "" {
		return false
	}
	for _, reader := range s.PromptLedgerReaders {
		if strings.EqualFold(strings.TrimSpace(reader), login) {
			return true
		}
	}
	return false
}

type promptsResponse struct {
	Since    time.Time           `json:"since"`
	Until    time.Time           `json:"until"`
	Person   string              `json:"person,omitempty"`
	Count    int                 `json:"count"`
	ByPerson map[string]int      `json:"by_person"`
	People   []promptPersonCount `json:"people"`
	Entries  []attribution.Entry `json:"entries"`
}

type promptPersonCount struct {
	Person   string `json:"person"`
	Prompts  int    `json:"prompts"`
	Sessions int    `json:"sessions"`
}

// handlePrompts: GET /_subrouter/prompts?person=leo&since=24h&until=RFC3339&limit=500
// since/until take an RFC 3339 time or a duration back from now ("90m",
// "24h", "7d"). The default window is the last 24 hours.
func (s Server) handlePrompts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if s.PromptLedger == nil {
		http.Error(w, "prompt ledger disabled", http.StatusNotFound)
		return
	}
	if !s.authorizePromptReader(r) {
		http.Error(w, "prompt ledger readers only", http.StatusForbidden)
		return
	}
	now := time.Now().UTC()
	query := r.URL.Query()
	until, err := parsePromptTime(query.Get("until"), now, now)
	if err != nil {
		http.Error(w, "bad until: "+err.Error(), http.StatusBadRequest)
		return
	}
	since, err := parsePromptTime(query.Get("since"), now, until.Add(-24*time.Hour))
	if err != nil {
		http.Error(w, "bad since: "+err.Error(), http.StatusBadRequest)
		return
	}
	limit, _ := strconv.Atoi(query.Get("limit"))
	person := strings.TrimSpace(query.Get("person"))
	entries, err := s.PromptLedger.Query(attribution.Filter{Person: person, Since: since, Until: until, Limit: limit})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	response := promptsResponse{Since: since, Until: until, Person: person, Count: len(entries), ByPerson: map[string]int{}, Entries: entries}
	sessions := map[string]map[string]struct{}{}
	for _, entry := range entries {
		response.ByPerson[entry.Person]++
		if sessions[entry.Person] == nil {
			sessions[entry.Person] = map[string]struct{}{}
		}
		sessions[entry.Person][entry.SessionID] = struct{}{}
	}
	for person, count := range response.ByPerson {
		delete(sessions[person], "")
		response.People = append(response.People, promptPersonCount{Person: person, Prompts: count, Sessions: len(sessions[person])})
	}
	sort.Slice(response.People, func(i, j int) bool { return response.People[i].Prompts > response.People[j].Prompts })
	if response.Entries == nil {
		response.Entries = []attribution.Entry{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(response)
}

func parsePromptTime(value string, now, fallback time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.UTC(), nil
	}
	if strings.HasSuffix(value, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(value, "d"))
		if err != nil {
			return time.Time{}, err
		}
		return now.Add(-time.Duration(days) * 24 * time.Hour), nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return time.Time{}, err
	}
	return now.Add(-d), nil
}
