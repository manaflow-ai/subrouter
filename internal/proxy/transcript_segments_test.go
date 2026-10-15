package proxy

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
		return []byte(fmt.Sprintf("event: response.output_text.delta\ndata: {\"delta\":\"héllo %d ✓\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-test\",\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":%d}}}\n\n", n, n+1))
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
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	store := newFakeBlobStore(t)
	defer store.server.Close()
	client, err := transcript.NewAzureBlobClient(transcript.AzureBlobClientConfig{
		Destination: store.server.URL + "/calls", SASToken: "sv=test&sp=cw", MaxBytesPerSecond: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := transcript.NewSegmentRecorder(transcript.SegmentConfig{
		Uploader:   client,
		Host:       "test-router",
		FrameBytes: 64 << 10,
		BlockBytes: 32 << 10,
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
		Sessions:     sessions,
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

	// Commit every segment and write the index, then rebuild each call
	// from nothing but its index row and the byte ranges it names.
	recorder.SealSegment()
	var rows []transcript.IndexRow
	for name, data := range store.blobs() {
		if !strings.HasPrefix(name, "calls/index/v1/") {
			continue
		}
		if err := transcript.ReadIndexRows(bytes.NewReader(data), func(row transcript.IndexRow) error {
			rows = append(rows, row)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	read := func(blob string, offset, length int64) ([]byte, error) {
		return client.ReadRange(context.Background(), blob, offset, length)
	}
	digest := func(body []byte) string {
		sum := sha256.Sum256(body)
		return hex.EncodeToString(sum[:])
	}
	wantRequests := map[string]bool{}
	for _, body := range append(append([][]byte(nil), sent...), streamed) {
		wantRequests[digest(body)] = false
	}
	repliesMu.Lock()
	wantReplies := map[string]bool{}
	for _, reply := range replies {
		wantReplies[digest(reply)] = false
	}
	repliesMu.Unlock()
	wantMessages := map[string]bool{digest(wsMessages[0]): false, digest(wsMessages[1]): false}
	for _, row := range rows {
		call, err := transcript.ExportCall(row, read)
		if err != nil {
			t.Fatalf("export %s: %v", row.RequestID, err)
		}
		if !call.Verified {
			raw, _ := read(row.RequestBlob, row.RequestOffset, row.RequestLength)
			decoder, _ := transcript.NewSegmentDecoder(bytes.NewReader(raw))
			text, _ := io.ReadAll(decoder)
			t.Fatalf("call %s did not rebuild exactly: row=%+v\nrequest=%+v\nresponse=%+v\nframes:\n%.3000s", row.RequestID, row, call.Request, call.Response, text)
		}
		if row.ClientNode != "lawrences-macbook-pro-2" || row.ClientUser != "lawrence@example.com" || row.ClientIP != "127.0.0.1" {
			t.Fatalf("call %s client = %s %s %s", row.RequestID, row.ClientNode, row.ClientUser, row.ClientIP)
		}
		wantRequests[call.Request.SHA256] = true
		wantMessages[call.Request.SHA256] = true
		if row.Kind == "http" {
			if row.Status != http.StatusOK || !row.Complete {
				t.Fatalf("call %s status %d complete %v", row.RequestID, row.Status, row.Complete)
			}
			wantReplies[call.Response.SHA256] = true
			if row.Path == "/v1/responses" && row.OutputTokens == 0 {
				t.Fatalf("call %s has no output tokens", row.RequestID)
			}
		}
		if call.Request.SHA256 == digest(sent[3]) && row.RequestEncoding != "zstd" {
			t.Fatalf("zstd request recorded with encoding %q", row.RequestEncoding)
		}
	}
	for name, want := range map[string]map[string]bool{"request": wantRequests, "response": wantReplies, "websocket message": wantMessages} {
		for sum, seen := range want {
			if !seen {
				t.Fatalf("%s %s missing from the index", name, sum)
			}
		}
	}
	// Segments decode whole too, with the seek table skipped.
	exporter := transcript.NewSegmentExporter(nil)
	for name, data := range store.blobs() {
		if !strings.HasPrefix(name, "calls/bodies/v1/") {
			continue
		}
		decoder, err := transcript.NewSegmentDecoder(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if err := exporter.ReadSegment(name, decoder); err != nil {
			t.Fatal(err)
		}
		decoder.Close()
	}
	exporter.Finish()
	if exporter.Stats.Failed() || exporter.Stats.DeltaBodies < 3 || exporter.Stats.MissingFooters != 0 {
		t.Fatalf("whole-segment stats = %+v", exporter.Stats)
	}
}

// fakeBlobStore speaks the part of the Blob service REST API the recorder
// and exporter use: Put Block (checking Content-MD5), Put Block List, Put
// Blob, and ranged Get Blob.
type fakeBlobStore struct {
	server    *httptest.Server
	mu        sync.Mutex
	staged    map[string]map[string][]byte
	committed map[string][]byte
}

func newFakeBlobStore(t *testing.T) *fakeBlobStore {
	store := &fakeBlobStore{staged: map[string]map[string][]byte{}, committed: map[string][]byte{}}
	store.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		body, _ := io.ReadAll(r.Body)
		if sum := r.Header.Get("Content-MD5"); sum != "" {
			got := md5.Sum(body)
			if base64.StdEncoding.EncodeToString(got[:]) != sum {
				http.Error(w, "Md5Mismatch", http.StatusBadRequest)
				return
			}
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		switch {
		case r.Method == http.MethodPut && r.URL.Query().Get("comp") == "block":
			if store.staged[name] == nil {
				store.staged[name] = map[string][]byte{}
			}
			store.staged[name][r.URL.Query().Get("blockid")] = body
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPut && r.URL.Query().Get("comp") == "blocklist":
			var list struct {
				Latest []string `xml:"Latest"`
			}
			if err := xml.Unmarshal(body, &list); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			var content []byte
			for _, id := range list.Latest {
				block, ok := store.staged[name][id]
				if !ok {
					http.Error(w, "InvalidBlockList", http.StatusBadRequest)
					return
				}
				content = append(content, block...)
			}
			store.committed[name] = content
			delete(store.staged, name)
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPut:
			store.committed[name] = body
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet:
			content, ok := store.committed[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			var start, end int64 = 0, int64(len(content)) - 1
			if spec := r.Header.Get("x-ms-range"); spec != "" {
				if _, err := fmt.Sscanf(spec, "bytes=%d-%d", &start, &end); err != nil || end >= int64(len(content)) {
					http.Error(w, "InvalidRange", http.StatusRequestedRangeNotSatisfiable)
					return
				}
			}
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(content[start : end+1])
		default:
			http.Error(w, "unsupported", http.StatusMethodNotAllowed)
		}
	}))
	return store
}

func (s *fakeBlobStore) blobs() map[string][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]byte, len(s.committed))
	for name, data := range s.committed {
		out[name] = data
	}
	return out
}
