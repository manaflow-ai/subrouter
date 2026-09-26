package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/tailnet"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func scanTokenUsage(contentType, body string, chunk int) (tokenUsage, string, bool) {
	scanner := newTokenUsageScanner(contentType)
	data := []byte(body)
	for len(data) > 0 {
		n := chunk
		if n <= 0 || n > len(data) {
			n = len(data)
		}
		scanner.Write(data[:n])
		data = data[n:]
	}
	return scanner.Finish()
}

func TestTokenUsageScannerParsesProviderShapes(t *testing.T) {
	bigText := strings.Repeat("x", tokenUsageLineHeadBytes+10_000)
	responsesCompleted := `{"type":"response.completed","response":{"id":"r1","model":"gpt-5-codex","output":[{"type":"message","content":[{"type":"output_text","text":"hi \"usage\":{\"input_tokens\":999}"}]}],"usage":{"input_tokens":1200,"input_tokens_details":{"cached_tokens":1000},"output_tokens":300,"output_tokens_details":{"reasoning_tokens":120},"total_tokens":1500}}}`
	bigCompleted := `{"type":"response.completed","response":{"id":"r1","model":"gpt-5-codex","output":[{"type":"message","content":[{"type":"output_text","text":"` + bigText + `"}]}],"usage":{"input_tokens":50,"input_tokens_details":{"cached_tokens":10},"output_tokens":7,"output_tokens_details":{"reasoning_tokens":2}},"metadata":{}}}`
	tests := []struct {
		name        string
		contentType string
		body        string
		want        tokenUsage
		wantModel   string
		wantOK      bool
	}{
		{
			name:        "responses sse",
			contentType: "text/event-stream",
			body: "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"model\":\"gpt-5-codex\",\"usage\":null}}\n\n" +
				"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
				"event: response.completed\ndata: " + responsesCompleted + "\n\n",
			want:      tokenUsage{InputTokens: 1200, CachedInputTokens: 1000, OutputTokens: 300, ReasoningOutputTokens: 120},
			wantModel: "gpt-5-codex",
			wantOK:    true,
		},
		{
			name:        "responses sse with a completed event larger than the line head",
			contentType: "text/event-stream; charset=utf-8",
			body:        "data: " + bigCompleted + "\r\n\r\n",
			want:        tokenUsage{InputTokens: 50, CachedInputTokens: 10, OutputTokens: 7, ReasoningOutputTokens: 2},
			wantModel:   "gpt-5-codex",
			wantOK:      true,
		},
		{
			name:        "responses json",
			contentType: "application/json",
			body:        `{"object":"response","model":"gpt-5","output":[],"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":4},"output_tokens":5,"output_tokens_details":{"reasoning_tokens":1}}}`,
			want:        tokenUsage{InputTokens: 10, CachedInputTokens: 4, OutputTokens: 5, ReasoningOutputTokens: 1},
			wantModel:   "gpt-5",
			wantOK:      true,
		},
		{
			name:        "chat completions json",
			contentType: "application/json",
			body:        `{"object":"chat.completion","model":"kimi-k2","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":6},"completion_tokens_details":{"reasoning_tokens":3}}}`,
			want:        tokenUsage{InputTokens: 20, CachedInputTokens: 6, OutputTokens: 8, ReasoningOutputTokens: 3},
			wantModel:   "kimi-k2",
			wantOK:      true,
		},
		{
			name:        "anthropic stream",
			contentType: "text/event-stream",
			body: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-opus-4-5\",\"usage\":{\"input_tokens\":12,\"cache_read_input_tokens\":3000,\"cache_creation_input_tokens\":200,\"output_tokens\":1}}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"secret prompt text\"}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":450}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			want:      tokenUsage{InputTokens: 3212, CachedInputTokens: 3000, CacheWriteInputTokens: 200, OutputTokens: 450},
			wantModel: "claude-opus-4-5",
			wantOK:    true,
		},
		{
			name:        "anthropic json",
			contentType: "application/json",
			body:        `{"id":"msg","type":"message","model":"claude-sonnet-4-5","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":7,"cache_read_input_tokens":100,"cache_creation_input_tokens":0,"output_tokens":9}}`,
			want:        tokenUsage{InputTokens: 107, CachedInputTokens: 100, OutputTokens: 9},
			wantModel:   "claude-sonnet-4-5",
			wantOK:      true,
		},
		{
			name:        "truncated sse stream",
			contentType: "text/event-stream",
			body:        "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"model\":\"gpt-5\"}}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output",
			wantModel:   "gpt-5",
		},
		{
			name:        "malformed json",
			contentType: "application/json",
			body:        `{"usage":{"input_tokens":`,
		},
		{
			name:        "json without usage",
			contentType: "application/json",
			body:        `{"error":{"message":"nope"}}`,
		},
	}
	for _, test := range tests {
		for _, chunk := range []int{0, 1, 7, 4096} {
			t.Run(fmt.Sprintf("%s/chunk=%d", test.name, chunk), func(t *testing.T) {
				usage, model, ok := scanTokenUsage(test.contentType, test.body, chunk)
				if ok != test.wantOK || usage != test.want || model != test.wantModel {
					t.Fatalf("got usage=%+v model=%q ok=%v, want usage=%+v model=%q ok=%v", usage, model, ok, test.want, test.wantModel, test.wantOK)
				}
			})
		}
	}
}

func TestTokenUsageFromWebSocketMessage(t *testing.T) {
	usage, model, ok, terminal := tokenUsageFromWebSocketMessage([]byte(`{"type":"response.completed","response":{"model":"gpt-5-codex","usage":{"input_tokens":9,"input_tokens_details":{"cached_tokens":2},"output_tokens":4,"output_tokens_details":{"reasoning_tokens":1}}}}`))
	if !terminal || !ok || model != "gpt-5-codex" || usage != (tokenUsage{InputTokens: 9, CachedInputTokens: 2, OutputTokens: 4, ReasoningOutputTokens: 1}) {
		t.Fatalf("completed: usage=%+v model=%q ok=%v terminal=%v", usage, model, ok, terminal)
	}
	if _, _, ok, terminal := tokenUsageFromWebSocketMessage([]byte(`{"type":"response.failed","response":{"error":{"code":"server_error"}}}`)); !terminal || ok {
		t.Fatalf("failed without usage: ok=%v terminal=%v, want terminal without usage", ok, terminal)
	}
	if _, _, _, terminal := tokenUsageFromWebSocketMessage([]byte(`{"type":"response.output_text.delta","delta":"x"}`)); terminal {
		t.Fatal("delta must not end a turn")
	}
	if _, _, _, terminal := tokenUsageFromWebSocketMessage(nil); terminal {
		t.Fatal("an uninspected message must not end a turn")
	}
}

func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

func TestTokenUsageRecorderBucketsAndCountsMissingUsage(t *testing.T) {
	recorder := NewTokenUsageRecorder("", nil)
	now := time.Date(2026, 9, 26, 10, 15, 0, 0, time.UTC)
	recorder.now = fixedClock(now)
	recorder.Record("codex", "acct-1", "GPT-5-Codex", "leos-mbp", tokenUsage{InputTokens: 100, CachedInputTokens: 60, OutputTokens: 10, ReasoningOutputTokens: 4}, true)
	recorder.Record("codex", "acct-1", "gpt-5-codex", "leos-mbp", tokenUsage{InputTokens: 50, OutputTokens: 5}, true)
	recorder.Record("codex", "acct-1", "gpt-5-codex", "leos-mbp", tokenUsage{InputTokens: 999}, false)
	recorder.now = fixedClock(now.Add(time.Hour))
	recorder.Record("claude", "claude-1", "claude-opus-4-5", "", tokenUsage{InputTokens: 7, CacheWriteInputTokens: 2, OutputTokens: 3}, true)

	rows, err := recorder.Rows(now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := []TokenUsageRow{
		{Hour: "2026-09-26T10:00:00Z", Provider: "codex", AccountID: "acct-1", Model: "gpt-5-codex", Client: "leos-mbp", Requests: 3, RequestsWithoutUsage: 1, InputTokens: 150, CachedInputTokens: 60, OutputTokens: 15, ReasoningOutputTokens: 4},
		{Hour: "2026-09-26T11:00:00Z", Provider: "claude", AccountID: "claude-1", Model: "claude-opus-4-5", Client: "unknown", Requests: 1, InputTokens: 7, CacheWriteInputTokens: 2, OutputTokens: 3},
	}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("rows =\n%+v\nwant\n%+v", rows, want)
	}
	later, _ := recorder.Rows(now.Add(time.Hour))
	if len(later) != 1 || later[0].Provider != "claude" {
		t.Fatalf("since filter rows = %+v, want only the 11:00 row", later)
	}
}

func TestTokenUsageRecorderCapsKeysPerHour(t *testing.T) {
	recorder := NewTokenUsageRecorder("", nil)
	recorder.now = fixedClock(time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC))
	for i := 0; i < tokenUsageMaxKeysPerHour+50; i++ {
		recorder.Record("codex", "acct-1", "gpt-5", fmt.Sprintf("client-%d", i), tokenUsage{OutputTokens: 1}, true)
	}
	rows, _ := recorder.Rows(time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC))
	if len(rows) != tokenUsageMaxKeysPerHour+1 {
		t.Fatalf("rows = %d, want cap %d plus one overflow row", len(rows), tokenUsageMaxKeysPerHour)
	}
	var overflow *TokenUsageRow
	var total int64
	for i := range rows {
		total += rows[i].Requests
		if rows[i].Client == tokenUsageOverflowLabel {
			overflow = &rows[i]
		}
	}
	if overflow == nil || overflow.Model != tokenUsageOverflowLabel || overflow.Requests != 50 {
		t.Fatalf("overflow row = %+v, want 50 requests under other/other", overflow)
	}
	if total != tokenUsageMaxKeysPerHour+50 {
		t.Fatalf("total requests = %d, want every request counted", total)
	}
}

func TestTokenUsageRecorderPersistsAndPrunes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token-usage.jsonl")
	now := time.Date(2026, 9, 26, 10, 30, 0, 0, time.UTC)
	stale := TokenUsageRow{Hour: now.Add(-31 * 24 * time.Hour).Truncate(time.Hour).Format(time.RFC3339), Provider: "codex", AccountID: "old", Model: "gpt-4", Client: "x", Requests: 9}
	kept := TokenUsageRow{Hour: "2026-09-26T09:00:00Z", Provider: "codex", AccountID: "acct-1", Model: "gpt-5", Client: "a", Requests: 2, InputTokens: 10}
	var seed strings.Builder
	for _, row := range []TokenUsageRow{stale, kept, kept} {
		line, _ := json.Marshal(row)
		seed.Write(line)
		seed.WriteByte('\n')
	}
	seed.WriteString("not json\n")
	if err := os.WriteFile(path, []byte(seed.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	first := NewTokenUsageRecorder("", nil)
	first.path = path
	first.now = fixedClock(now)
	if err := first.compact(); err != nil {
		t.Fatal(err)
	}
	first.Record("codex", "acct-1", "gpt-5", "a", tokenUsage{InputTokens: 5, OutputTokens: 1}, true)
	// Unflushed usage is visible before it reaches the file.
	rows, err := first.Rows(now.Add(-2 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Requests != 4 || rows[0].InputTokens != 20 || rows[1].Requests != 1 {
		t.Fatalf("rows before flush = %+v", rows)
	}
	if err := first.Flush(); err != nil {
		t.Fatal(err)
	}
	// A second flush with nothing new must not double count.
	if err := first.Flush(); err != nil {
		t.Fatal(err)
	}

	second := NewTokenUsageRecorder("", nil)
	second.path = path
	second.now = fixedClock(now.Add(time.Minute))
	rows, err = second.Rows(now.Add(-40 * 24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("reloaded rows = %+v, want the two recent rows and no stale one", rows)
	}
	if rows[0].Hour != "2026-09-26T09:00:00Z" || rows[0].Requests != 4 || rows[1].Hour != "2026-09-26T10:00:00Z" || rows[1].Requests != 1 || rows[1].InputTokens != 5 {
		t.Fatalf("reloaded rows = %+v", rows)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"account_id":"old"`) {
		t.Fatalf("stale row survived compaction:\n%s", body)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestTokenUsageRecorderLoadsOnStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token-usage.jsonl")
	recorder := NewTokenUsageRecorder(path, nil)
	recorder.Record("codex", "acct-1", "gpt-5", "a", tokenUsage{InputTokens: 3}, true)
	if err := recorder.Flush(); err != nil {
		t.Fatal(err)
	}
	reloaded := NewTokenUsageRecorder(path, nil)
	rows, err := reloaded.Rows(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].InputTokens != 3 {
		t.Fatalf("rows after restart = %+v", rows)
	}
}

type fakeTokenUsageWhoIs struct {
	calls atomic.Int32
	name  string
}

func (f *fakeTokenUsageWhoIs) Lookup(_ context.Context, _ string) (tailnet.Identity, bool) {
	f.calls.Add(1)
	if f.name == "" {
		return tailnet.Identity{}, false
	}
	return tailnet.Identity{NodeName: f.name, LoginName: "someone@example.com"}, true
}

func TestTokenUsageClientPrecedence(t *testing.T) {
	whois := &fakeTokenUsageWhoIs{name: "leos-mbp.tail1234.ts.net."}
	recorder := NewTokenUsageRecorder("", whois)
	request := func(remote, client, email string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		r.RemoteAddr = remote
		if client != "" {
			r.Header.Set(ClientNameHeader, client)
		}
		return r
	}
	tests := []struct {
		name         string
		remote       string
		client       string
		email        string
		want         string
		wantBlocking bool
	}{
		{name: "header wins", remote: "100.100.1.2:5555", client: "ci-runner_1.2", email: "a@example.com", want: "ci-runner_1.2"},
		{name: "invalid header falls to email hash", remote: "127.0.0.1:5555", client: "bad name!", email: "Alice@Example.com", want: "user:" + userEmailHash("alice@example.com")},
		{name: "overlong header ignored", remote: "127.0.0.1:5555", client: strings.Repeat("a", 65), want: "unknown"},
		{name: "tailnet peer", remote: "100.100.1.2:5555", want: "leos-mbp", wantBlocking: true},
		{name: "tailnet ipv6 peer", remote: "[fd7a:115c:a1e0::1]:5555", want: "leos-mbp", wantBlocking: true},
		{name: "loopback without identity", remote: "127.0.0.1:5555", want: "unknown"},
		{name: "public address is not looked up", remote: "203.0.113.9:5555", want: "unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder.clientCache = map[string]tokenUsageClientEntry{}
			resolve, blocking := recorder.tokenUsageClient(request(test.remote, test.client, test.email), test.email)
			if blocking != test.wantBlocking {
				t.Fatalf("blocking = %v, want %v", blocking, test.wantBlocking)
			}
			if got := resolve(); got != test.want {
				t.Fatalf("client = %q, want %q", got, test.want)
			}
		})
	}
	// A resolved tailnet peer is cached, so the next request neither blocks
	// nor spawns another lookup.
	first, _ := recorder.tokenUsageClient(request("[fd7a:115c:a1e0::1]:6000", "", ""), "")
	_ = first()
	calls := whois.calls.Load()
	resolve, blocking := recorder.tokenUsageClient(request("[fd7a:115c:a1e0::1]:6001", "", ""), "")
	if blocking || resolve() != "leos-mbp" || whois.calls.Load() != calls {
		t.Fatalf("cached tailnet lookup: blocking=%v calls=%d->%d", blocking, calls, whois.calls.Load())
	}
}

func TestNormalizeClientName(t *testing.T) {
	for value, want := range map[string]string{
		"leos-mbp":              "leos-mbp",
		" ci.runner_2 ":         "ci.runner_2",
		"":                      "",
		"has space":             "",
		"emoji-☃":               "",
		"a/b":                   "",
		strings.Repeat("a", 64): strings.Repeat("a", 64),
		strings.Repeat("a", 65): "",
	} {
		if got := NormalizeClientName(value); got != want {
			t.Errorf("NormalizeClientName(%q) = %q, want %q", value, got, want)
		}
	}
}

func TestParseTokenUsageSince(t *testing.T) {
	now := time.Date(2026, 9, 26, 10, 30, 0, 0, time.UTC)
	for _, test := range []struct {
		value string
		want  time.Time
		err   bool
	}{
		{value: "", want: time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)},
		{value: "2h", want: time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)},
		{value: "2026-09-20T05:45:00Z", want: time.Date(2026, 9, 20, 5, 0, 0, 0, time.UTC)},
		{value: "9000h", want: now.Add(-tokenUsageRetention).Truncate(time.Hour)},
		{value: "-1h", err: true},
		{value: "yesterday", err: true},
	} {
		got, err := parseTokenUsageSince(test.value, now)
		if test.err {
			if err == nil {
				t.Errorf("since %q: want error", test.value)
			}
			continue
		}
		if err != nil || !got.Equal(test.want) {
			t.Errorf("since %q = %v, %v; want %v", test.value, got, err, test.want)
		}
	}
}

func TestTokenUsageEndpointRequiresAdminAndKeepsShape(t *testing.T) {
	recorder := NewTokenUsageRecorder("", nil)
	recorder.Record("codex", "acct-1", "gpt-5", "leos-mbp", tokenUsage{InputTokens: 10, CachedInputTokens: 4, OutputTokens: 2, ReasoningOutputTokens: 1}, true)
	handler := Server{AdminToken: "admin-secret", TokenUsage: recorder}.Handler()

	anonymous := httptest.NewRequest(http.MethodGet, "http://subrouter.example/_subrouter/token-usage", nil)
	anonymous.RemoteAddr = "203.0.113.5:4444"
	recorderResponse := httptest.NewRecorder()
	handler.ServeHTTP(recorderResponse, anonymous)
	if recorderResponse.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", recorderResponse.Code)
	}

	admin := httptest.NewRequest(http.MethodGet, "http://subrouter.example/_subrouter/token-usage?since=48h", nil)
	admin.RemoteAddr = "203.0.113.5:4444"
	admin.Header.Set("Authorization", "Bearer admin-secret")
	adminResponse := httptest.NewRecorder()
	handler.ServeHTTP(adminResponse, admin)
	if adminResponse.Code != http.StatusOK {
		t.Fatalf("admin status = %d body=%s", adminResponse.Code, adminResponse.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(adminResponse.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"since", "generated_at", "rows"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("response missing %q: %s", key, adminResponse.Body.String())
		}
	}
	if len(decoded) != 3 {
		t.Fatalf("response keys = %v, want exactly since/generated_at/rows", decoded)
	}
	rows := decoded["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %v", rows)
	}
	row := rows[0].(map[string]any)
	wantKeys := []string{"hour", "provider", "account_id", "model", "client", "requests", "requests_without_usage", "input_tokens", "cached_input_tokens", "cache_write_input_tokens", "output_tokens", "reasoning_output_tokens"}
	if len(row) != len(wantKeys) {
		t.Fatalf("row keys = %v, want %v", row, wantKeys)
	}
	for _, key := range wantKeys {
		if _, ok := row[key]; !ok {
			t.Fatalf("row missing %q: %v", key, row)
		}
	}
	if row["client"] != "leos-mbp" || row["input_tokens"].(float64) != 10 || row["reasoning_output_tokens"].(float64) != 1 {
		t.Fatalf("row = %v", row)
	}

	bad := httptest.NewRequest(http.MethodGet, "http://subrouter.example/_subrouter/token-usage?since=nope", nil)
	bad.RemoteAddr = "203.0.113.5:4444"
	bad.Header.Set("Authorization", "Bearer admin-secret")
	badResponse := httptest.NewRecorder()
	handler.ServeHTTP(badResponse, bad)
	if badResponse.Code != http.StatusBadRequest {
		t.Fatalf("bad since status = %d, want 400", badResponse.Code)
	}
}

func TestProxyRecordsTokenUsageForStreamedResponse(t *testing.T) {
	const sse = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"model\":\"gpt-5-codex\"}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5-codex\",\"usage\":{\"input_tokens\":40,\"input_tokens_details\":{\"cached_tokens\":30},\"output_tokens\":6,\"output_tokens_details\":{\"reasoning_tokens\":2}}}}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get(ClientNameHeader); got != "" {
			t.Errorf("%s leaked upstream: %q", ClientNameHeader, got)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	recorder := NewTokenUsageRecorder("", nil)
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID: "codex-a", AuthMode: accounts.AuthModeOAuth, Token: "token-a", AccountID: "acct-a",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1 << 20,
		TokenUsage:   recorder,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	post := func(path string) string {
		request, _ := http.NewRequest(http.MethodPost, subrouter.URL+path, strings.NewReader(`{"model":"gpt-5-codex","input":"hello"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(ClientNameHeader, "leos-mbp")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return string(body)
	}
	if got := post("/v1/responses"); got != sse {
		t.Fatalf("client body changed:\n%q\nwant\n%q", got, sse)
	}
	post("/v1/models/whatever") // not a model turn: must not count

	rows, err := recorder.Rows(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want one", rows)
	}
	row := rows[0]
	if row.Provider != "codex" || row.AccountID != "codex-a" || row.Model != "gpt-5-codex" || row.Client != "leos-mbp" ||
		row.Requests != 1 || row.RequestsWithoutUsage != 0 || row.InputTokens != 40 || row.CachedInputTokens != 30 ||
		row.OutputTokens != 6 || row.ReasoningOutputTokens != 2 {
		t.Fatalf("row = %+v", row)
	}
}

func TestProxyRecordsTokenUsageForWebSocketTurns(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"hi"}`))
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":11,"input_tokens_details":{"cached_tokens":5},"output_tokens":3,"output_tokens_details":{"reasoning_tokens":0}}}}`))
		}
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	recorder := NewTokenUsageRecorder("", nil)
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID: "codex-a", AuthMode: accounts.AuthModeOAuth, Token: "token-a", AccountID: "acct-a",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1 << 20,
		TokenUsage:   recorder,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	wsURL := "ws" + strings.TrimPrefix(subrouter.URL, "http") + "/v1/responses"
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"X-Subrouter-User-Email": []string{"alice@example.com"}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer response.Body.Close()
	defer conn.Close()
	for turn := 0; turn < 2; turn++ {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5-codex","input":[]}`)); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if _, _, err := conn.ReadMessage(); err != nil {
				t.Fatal(err)
			}
		}
	}
	rows, err := recorder.Rows(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want one", rows)
	}
	row := rows[0]
	if row.Client != "user:"+userEmailHash("alice@example.com") || row.Model != "gpt-5-codex" || row.Requests != 2 ||
		row.InputTokens != 22 || row.CachedInputTokens != 10 || row.OutputTokens != 6 {
		t.Fatalf("row = %+v", row)
	}
	if strings.Contains(fmt.Sprint(row), "alice") {
		t.Fatalf("row leaks the email: %+v", row)
	}
}
