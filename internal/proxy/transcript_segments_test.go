package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/transcript"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// TestSegmentTranscriptsRebuildEveryBodyExactly drives real HTTP and
// websocket traffic through the proxy into a real upstream, lets the segment
// recorder seal and compress what it saw, and reads the compressed segment
// back through the exporter `subrouter transcripts export` uses. Every body
// must come back byte for byte, attributed to the client machine.
func TestSegmentTranscriptsRebuildEveryBodyExactly(t *testing.T) {
	t.Parallel()
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstreamReply := func(n int) []byte {
		return []byte(fmt.Sprintf("event: response.output_text.delta\ndata: {\"delta\":\"héllo %d ✓\"}\n\nevent: response.completed\ndata: {\"usage\":{\"output_tokens\":%d}}\n\n", n, n))
	}
	var repliesMu sync.Mutex
	var replies [][]byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			for {
				messageType, body, err := conn.ReadMessage()
				if err != nil {
					return
				}
				if err := conn.WriteMessage(messageType, append([]byte(`{"echo_bytes":`), []byte(fmt.Sprint(len(body), "}"))...)); err != nil {
					return
				}
			}
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		repliesMu.Lock()
		reply := upstreamReply(len(replies))
		replies = append(replies, reply)
		repliesMu.Unlock()
		_, _ = w.Write(reply)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	spool := t.TempDir()
	recorder, err := transcript.NewSegmentRecorder(transcript.SegmentConfig{
		Dir:  spool,
		Host: "test-router",
		WhoIs: func(_ context.Context, remoteAddr string) (string, string, bool) {
			if strings.HasPrefix(remoteAddr, "127.0.0.1:") {
				return "lawrences-macbook-pro-2", "lawrence@example.com", true
			}
			return "", "", false
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID:       "a@example.com",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "a-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 32 << 20,
		Transcripts:  recorder,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	// A conversation that grows each turn, with an edit in the middle (the
	// way Claude Code moves its cache marker) and a large shared tail.
	tools := strings.Repeat(`{"name":"tool","description":"a long tool description, repeated"},`, 3000)
	var sent [][]byte
	history := ""
	for turn := 0; turn < 4; turn++ {
		marker := fmt.Sprintf(`"cache_marker_at":%d`, turn)
		history += fmt.Sprintf(`{"role":"user","content":"turn %d: %s"},`, turn, strings.Repeat("ünïcode ", 200+turn))
		sent = append(sent, []byte(`{"session_id":"seg-session:0",`+marker+`,"input":[`+history+`{}],"tools":[`+tools+`{}]}`))
	}
	post := func(path string, body []byte, encoding string) {
		t.Helper()
		wire := body
		if encoding == "zstd" {
			encoder, _ := zstd.NewWriter(nil)
			wire = encoder.EncodeAll(body, nil)
		}
		req, err := http.NewRequest(http.MethodPost, subrouter.URL+path, bytes.NewReader(wire))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer client-token")
		req.Header.Set("X-Codex-Window-ID", "seg-session:0")
		if encoding != "" {
			req.Header.Set("Content-Encoding", encoding)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	for _, body := range sent[:3] {
		post("/v1/responses", body, "")
	}
	post("/v1/responses", sent[3], "zstd")
	streamed := []byte(`{"session_id":"seg-session:0","note":"not a replayable path"}`)
	post("/v1/other", streamed, "")

	wsURL := "ws" + strings.TrimPrefix(subrouter.URL, "http") + "/v1/responses"
	conn, wsResponse, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"X-Codex-Window-ID": []string{"seg-ws:0"}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	wsMessages := [][]byte{[]byte(`{"type":"response.create","input":["` + strings.Repeat("a", 5000) + `"]}`), []byte(`{"type":"response.create","input":["` + strings.Repeat("a", 5000) + `","next"]}`)}
	for _, message := range wsMessages {
		if err := conn.WriteMessage(websocket.TextMessage, message); err != nil {
			t.Fatal(err)
		}
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatal(err)
		}
	}
	_ = conn.Close()
	_ = wsResponse.Body.Close()

	// Seal, then wait for the store to compress the segment.
	deadline := time.Now().Add(30 * time.Second)
	var compressed []string
	for {
		recorder.SealSegment()
		compressed, _ = filepath.Glob(filepath.Join(recorder.SegmentDir(), "upload", "*.jsonl.zst"))
		if len(compressed) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(compressed) == 0 {
		t.Fatal("no compressed segment appeared")
	}

	var records []transcript.ExportRecord
	exporter := transcript.NewSegmentExporter(func(record transcript.ExportRecord) error {
		records = append(records, record)
		return nil
	})
	for _, path := range compressed {
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		decoder, err := transcript.NewSegmentDecoder(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := exporter.ReadSegment(filepath.Base(path), decoder); err != nil {
			t.Fatal(err)
		}
		decoder.Close()
		_ = file.Close()
	}
	exporter.Finish()
	if exporter.Stats.Failed() || exporter.Stats.Mismatched != 0 {
		t.Fatalf("export stats = %+v", exporter.Stats)
	}
	if exporter.Stats.DeltaBodies < 3 {
		t.Fatalf("delta bodies = %d, want the later turns stored as deltas", exporter.Stats.DeltaBodies)
	}

	digest := func(body []byte) string {
		sum := sha256.Sum256(body)
		return hex.EncodeToString(sum[:])
	}
	wantRequests := map[string]bool{}
	for _, body := range append(append([][]byte(nil), sent...), streamed) {
		wantRequests[digest(body)] = false
	}
	wantReplies := map[string]bool{}
	repliesMu.Lock()
	defer repliesMu.Unlock()
	for _, reply := range replies {
		wantReplies[digest(reply)] = false
	}
	wantMessages := map[string]bool{digest(wsMessages[0]): false, digest(wsMessages[1]): false}
	for _, record := range records {
		switch record.Kind {
		case "http":
			if record.Client == nil || record.Client.Node != "lawrences-macbook-pro-2" || record.Client.User != "lawrence@example.com" || record.Client.IP != "127.0.0.1" {
				t.Fatalf("request %s client = %+v", record.RequestID, record.Client)
			}
			if record.Request == nil || !record.Request.Verified || record.Response == nil || record.Response.Body == nil || !record.Response.Body.Verified {
				t.Fatalf("request %s is not complete and verified: %+v", record.RequestID, record)
			}
			if _, ok := wantRequests[record.Request.SHA256]; !ok {
				t.Fatalf("request %s body %s was never sent", record.RequestID, record.Request.SHA256)
			}
			wantRequests[record.Request.SHA256] = true
			wantReplies[record.Response.Body.SHA256] = true
			if record.Request.SHA256 == digest(sent[3]) && record.Request.ContentEncoding != "zstd" {
				t.Fatalf("zstd request recorded with encoding %q", record.Request.ContentEncoding)
			}
		case "websocket_message":
			if record.Direction == "client_to_upstream" {
				if record.Message == nil || !record.Message.Verified {
					t.Fatalf("websocket message not verified: %+v", record)
				}
				wantMessages[record.Message.SHA256] = true
			}
		}
	}
	for name, want := range map[string]map[string]bool{"request": wantRequests, "response": wantReplies, "websocket message": wantMessages} {
		for sum, seen := range want {
			if !seen {
				t.Fatalf("%s %s missing from the exported transcript", name, sum)
			}
		}
	}
}
