package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// codexNoContentTypeCapture is a live chatgpt.com response to a codex_cli_rs
// POST /v1/responses, captured on the team server: the header set as sent
// (no Content-Type; Set-Cookie, X-Codex-Turn-State, and safety_identifier
// redacted) and the SSE body with the echoed filler instructions cut to
// 80 KiB. usage carries the nested attribution object chatgpt.com sends.
const codexNoContentTypeCapture = "testdata/codex-responses-no-content-type.http"

func loadCodexCapture(t *testing.T) (http.Header, []byte) {
	t.Helper()
	raw, err := os.ReadFile(codexNoContentTypeCapture)
	if err != nil {
		t.Fatal(err)
	}
	head, body, ok := bytes.Cut(raw, []byte("\r\n\r\n"))
	if !ok {
		t.Fatal("capture has no header terminator")
	}
	header := http.Header{}
	for _, line := range strings.Split(string(head), "\r\n")[1:] {
		name, value, _ := strings.Cut(line, ": ")
		header.Add(name, value)
	}
	if header.Get("Content-Type") != "" || !bytes.HasPrefix(body, []byte("event: response.created\n")) {
		t.Fatalf("capture is not the no-Content-Type stream (Content-Type %q)", header.Get("Content-Type"))
	}
	return header, body
}

// serveCodexCapture writes the captured headers exactly (Transfer-Encoding is
// the server's to set) and the body one event at a time. Go's server would
// otherwise sniff a Content-Type of its own, so it is suppressed.
func serveCodexCapture(w http.ResponseWriter, header http.Header, body []byte) {
	for name, values := range header {
		if name == "Transfer-Encoding" {
			continue
		}
		w.Header()[name] = values
	}
	w.Header()["Content-Type"] = nil
	w.WriteHeader(http.StatusOK)
	flusher := w.(http.Flusher)
	for _, event := range bytes.SplitAfter(body, []byte("\n\n")) {
		_, _ = w.Write(event)
		flusher.Flush()
	}
}

func TestSniffBodyKind(t *testing.T) {
	for _, tc := range []struct {
		prefix  string
		kind    sniffedBodyKind
		decided bool
	}{
		{"event: response.created\n", sniffedSSE, true},
		{"data: {}", sniffedSSE, true},
		{": keepalive\n", sniffedSSE, true},
		{"id: 1\n", sniffedSSE, true},
		{"retry: 100\n", sniffedSSE, true},
		{"\r\n  event:", sniffedSSE, true},
		{"{\"id\":1}", sniffedJSON, true},
		{"  [1]", sniffedJSON, true},
		{"<html>", sniffedUnknown, true},
		{"", sniffedUnknown, false},
		{" \n\t", sniffedUnknown, false},
		{"eve", sniffedUnknown, false},
		{"da", sniffedUnknown, false},
	} {
		kind, decided := sniffBodyKind([]byte(tc.prefix))
		if kind != tc.kind || decided != tc.decided {
			t.Errorf("sniffBodyKind(%q) = %v, %v; want %v, %v", tc.prefix, kind, decided, tc.kind, tc.decided)
		}
	}
}

// The scanner decides from the first bytes when the Content-Type leaves the
// body kind open, at any chunk size.
func TestTokenUsageScannerSniffsBodyWithoutContentType(t *testing.T) {
	_, body := loadCodexCapture(t)
	for _, contentType := range []string{"", "text/plain; charset=utf-8", "application/octet-stream"} {
		for _, chunk := range []int{1, 7, 4096, 0} {
			usage, model, ok := scanTokenUsage(contentType, string(body), chunk)
			if !ok || model != "gpt-6-astra" || usage.InputTokens != 36024 || usage.OutputTokens != 5 {
				t.Fatalf("content type %q chunk %d: usage %+v model %q ok %v", contentType, chunk, usage, model, ok)
			}
		}
	}
	usage, _, ok := scanTokenUsage("", " \n"+`{"type":"response","usage":{"input_tokens":3,"output_tokens":2}}`, 1)
	if !ok || usage.InputTokens != 3 || usage.OutputTokens != 2 {
		t.Fatalf("JSON body without Content-Type: usage %+v ok %v", usage, ok)
	}
}

func TestLabelSniffedContentType(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		contentLength    int64
		header           string
	}{
		{name: "sse", body: "event: response.created\ndata: {}\n\n", want: "text/event-stream", contentLength: -1},
		{name: "json", body: " {\"id\":1}", want: "application/json", contentLength: -1},
		{name: "unknown", body: "<html></html>", want: "", contentLength: -1},
		{name: "whitespace only", body: strings.Repeat(" ", 100), want: "", contentLength: -1},
		{name: "declared type kept", body: "event: x\n", want: "text/plain", contentLength: -1, header: "text/plain"},
		{name: "empty body untouched", body: "", want: "", contentLength: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{
				StatusCode:    http.StatusOK,
				Header:        http.Header{},
				Body:          io.NopCloser(strings.NewReader(tc.body)),
				ContentLength: tc.contentLength,
			}
			if tc.header != "" {
				response.Header.Set("Content-Type", tc.header)
			}
			labelSniffedContentType(response)
			if got := response.Header.Get("Content-Type"); got != tc.want {
				t.Fatalf("Content-Type = %q, want %q", got, tc.want)
			}
			got, _ := io.ReadAll(response.Body)
			if string(got) != tc.body {
				t.Fatalf("body changed: %q", got)
			}
		})
	}
}

// The live Codex response, served as captured with no Content-Type, records
// its usage through the proxy, reaches the client byte for byte, and is
// labeled as the stream it is.
func TestProxyRecordsTokenUsageForCodexStreamWithoutContentType(t *testing.T) {
	header, body := loadCodexCapture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		serveCodexCapture(w, header, body)
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	recorder := NewTokenUsageRecorder("", nil)
	subrouter := httptest.NewServer(Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID: "codex-a", AuthMode: accounts.AuthModeOAuth, Token: "token-a", AccountID: "acct-a",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1 << 20,
		TokenUsage:   recorder,
	}.Handler())
	defer subrouter.Close()

	request, _ := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses",
		strings.NewReader(`{"model":"gpt-6-astra","instructions":"x","input":"say ok","stream":true,"store":false}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("User-Agent", "codex_cli_rs/0.146.0 (Mac OS 26.4.1; arm64)")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !bytes.Equal(got, body) {
		t.Fatalf("status %d, body changed (%d bytes, want %d)", response.StatusCode, len(got), len(body))
	}
	if ct := response.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("client Content-Type = %q, want text/event-stream", ct)
	}

	var rows []TokenUsageRow
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err = recorder.Rows(time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want one", rows)
	}
	row := rows[0]
	if row.Model != "gpt-6-astra" || row.Requests != 1 || row.RequestsWithoutUsage != 0 ||
		row.InputTokens != 36024 || row.CachedInputTokens != 0 || row.OutputTokens != 5 || row.ReasoningOutputTokens != 0 {
		t.Fatalf("row = %+v", row)
	}
}

// The Codex capacity handling (#373, #382) holds a 2xx stream until its first
// visible output and retries a pre-output server_is_overloaded. It recognized
// a stream by Content-Type, so a chatgpt.com stream with none was passed
// straight through, overload and all.
func TestCodexOverloadRetriesStreamWithoutContentType(t *testing.T) {
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.Header()["Content-Type"] = nil
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n")
		if token == "oauth-token-0" {
			_, _ = io.WriteString(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"capacity\"}}}\n\n")
			return
		}
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"served-from-"+token+"\"}]}]}}\n\n")
	}))
	defer pool.Close()
	poolURL, _ := url.Parse(pool.URL)
	server := codexOverloadServer(t, poolURL, 2, true)
	if _, err := server.Sessions.Put("codex", "session-a", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body := codexEgressPost(t, proxy.URL, "session-a")
	if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") || strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("status=%d body=%s, want the overload retried onto account 1", status, body)
	}
}
