package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testRecorderOptions never flushes on its own, so a test controls exactly
// when buffered events reach disk.
var testRecorderOptions = recorderOptions{
	maxOpenFiles:     256,
	flushBytes:       1 << 30,
	flushInterval:    time.Hour,
	idleTicks:        1 << 30,
	maxBufferedBytes: 64 << 20,
}

func TestRecorderConcurrentSessionsWriteCompleteOrderedLines(t *testing.T) {
	dir := t.TempDir()
	// A small LRU, a small flush threshold, and a short interval exercise
	// eviction, size-triggered flushes, and ticks while writers are running.
	recorder := newRecorder(dir, recorderOptions{
		maxOpenFiles:     5,
		flushBytes:       2 << 10,
		flushInterval:    2 * time.Millisecond,
		idleTicks:        3,
		maxBufferedBytes: 64 << 20,
	})

	const sessions = 24
	const writersPerSession = 4
	const eventsPerWriter = 150
	var wg sync.WaitGroup
	for s := 0; s < sessions; s++ {
		for w := 0; w < writersPerSession; w++ {
			wg.Add(1)
			go func(s, w int) {
				defer wg.Done()
				sessionID := fmt.Sprintf("session-%02d", s)
				streamID := fmt.Sprintf("writer-%d", w)
				for i := 0; i < eventsPerWriter; i++ {
					body := bytes.Repeat([]byte{byte('a' + w)}, 100+i%400)
					recorder.RecordPayloadChunk("codex", sessionID, "http_body", "upstream_to_client", streamID, i, 0, body, nil)
				}
			}(s, w)
		}
	}
	wg.Wait()
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if got := recorder.DroppedEvents(); got != 0 {
		t.Fatalf("dropped %d events", got)
	}

	for s := 0; s < sessions; s++ {
		path := recorder.PathForSession("codex", fmt.Sprintf("session-%02d", s))
		next := map[string]int{}
		for _, event := range readEvents(t, path) {
			streamID := event.Payload["stream_id"].(string)
			index := int(event.Payload["chunk_index"].(float64))
			if index != next[streamID] {
				t.Fatalf("%s: %s chunk %d out of order, want %d", path, streamID, index, next[streamID])
			}
			next[streamID]++
		}
		for w := 0; w < writersPerSession; w++ {
			if got := next[fmt.Sprintf("writer-%d", w)]; got != eventsPerWriter {
				t.Fatalf("%s: writer-%d has %d events, want %d", path, w, got, eventsPerWriter)
			}
		}
	}
}

func TestRecorderCloseFlushesBufferedEvents(t *testing.T) {
	dir := t.TempDir()
	recorder := newRecorder(dir, testRecorderOptions)
	for i := 0; i < 10; i++ {
		recorder.RecordMeta("claude", fmt.Sprintf("s%d:0", i%3), map[string]any{"i": i})
	}
	path := recorder.PathForSession("claude", "s0")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("events reached disk before any flush: %v", err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	total := 0
	for i := 0; i < 3; i++ {
		total += len(readEvents(t, recorder.PathForSession("claude", fmt.Sprintf("s%d", i))))
	}
	if total != 10 {
		t.Fatalf("after Close found %d events, want 10", total)
	}

	// Late writers after Close still reach disk, synchronously and in order.
	recorder.RecordMeta("claude", "s0:0", map[string]any{"i": "late"})
	events := readEvents(t, path)
	if got := events[len(events)-1].Payload["i"]; got != "late" {
		t.Fatalf("last event after Close = %v, want late", got)
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestRecorderLRUEvictionFlushesAndReopens(t *testing.T) {
	dir := t.TempDir()
	opts := testRecorderOptions
	opts.maxOpenFiles = 2
	recorder := newRecorder(dir, opts)
	defer recorder.Close()

	recorder.RecordMeta("codex", "a", map[string]any{"n": 1})
	recorder.RecordMeta("codex", "b", map[string]any{"n": 1})
	recorder.RecordMeta("codex", "c", map[string]any{"n": 1}) // evicts a
	recorder.mu.Lock()
	_, live := recorder.appenders[recorder.PathForSession("codex", "a")]
	open := recorder.lru.Len()
	recorder.mu.Unlock()
	if live || open != 2 {
		t.Fatalf("after eviction: a live=%v, open=%d, want false and 2", live, open)
	}

	// Writing a again before the evicted appender is flushed must keep order:
	// the new appender finishes the old one before its own first write.
	recorder.RecordMeta("codex", "a", map[string]any{"n": 2})
	if err := recorder.Flush(); err != nil {
		t.Fatal(err)
	}
	var got []float64
	for _, event := range readEvents(t, recorder.PathForSession("codex", "a")) {
		got = append(got, event.Payload["n"].(float64))
	}
	if fmt.Sprint(got) != "[1 2]" {
		t.Fatalf("a events = %v, want [1 2]", got)
	}
}

func TestRecorderIdleFilesCloseAndFlusherStops(t *testing.T) {
	dir := t.TempDir()
	recorder := newRecorder(dir, recorderOptions{
		maxOpenFiles:     8,
		flushBytes:       1 << 20,
		flushInterval:    time.Millisecond,
		idleTicks:        2,
		maxBufferedBytes: 1 << 20,
	})
	recorder.RecordMeta("codex", "idle", nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		recorder.mu.Lock()
		running, open := recorder.running, len(recorder.appenders)
		recorder.mu.Unlock()
		if !running && open == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("flusher still running=%v with %d open files", running, open)
		}
		time.Sleep(time.Millisecond)
	}
	if n := len(readEvents(t, recorder.PathForSession("codex", "idle"))); n != 1 {
		t.Fatalf("idle session has %d events, want 1", n)
	}
	// A write after the flusher exited starts it again.
	recorder.RecordMeta("codex", "idle", nil)
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if n := len(readEvents(t, recorder.PathForSession("codex", "idle"))); n != 2 {
		t.Fatalf("idle session has %d events, want 2", n)
	}
}

func TestRecorderReadersSeeFlushedEvents(t *testing.T) {
	dir := t.TempDir()
	recorder := newRecorder(dir, testRecorderOptions)
	defer recorder.Close()
	recorder.RecordMeta("codex", "reader:0", map[string]any{"user": "u"})
	recorder.RecordPayload("codex", "reader:0", "http_body", "client_to_upstream", []byte(`{"x":1}`), nil)

	if err := recorder.Flush(); err != nil {
		t.Fatal(err)
	}
	summaries, err := ListSummaries(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].EventCount != 2 {
		t.Fatalf("summaries = %+v, want one session with 2 events", summaries)
	}
	raw, err := ReadRawSession(dir, "codex", "reader")
	if err != nil || len(raw) != 2 {
		t.Fatalf("ReadRawSession = %d events, %v; want 2", len(raw), err)
	}
	sanitized, err := ReadSanitizedSession(dir, "codex", "reader")
	if err != nil || len(sanitized) != 2 {
		t.Fatalf("ReadSanitizedSession = %d events, %v; want 2", len(sanitized), err)
	}
	if _, err := Analyze(dir); err != nil {
		t.Fatal(err)
	}
	files, _, err := collectTranscriptFiles(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("sync sees %d files, %v; want 1", len(files), err)
	}
}

// The syncers archive and delete transcript files, then prune empty
// directories, while the recorder may still hold the file open.
func TestRecorderReopensFileRemovedBySync(t *testing.T) {
	dir := t.TempDir()
	recorder := newRecorder(dir, testRecorderOptions)
	defer recorder.Close()
	path := recorder.PathForSession("codex", "archived")

	recorder.RecordMeta("codex", "archived", map[string]any{"n": 1})
	if err := recorder.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := pruneEmptyDirs(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("session directory survived pruning: %v", err)
	}

	recorder.RecordMeta("codex", "archived", map[string]any{"n": 2})
	if err := recorder.Flush(); err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, path)
	if len(events) != 1 || events[0].Payload["n"].(float64) != 2 {
		t.Fatalf("recreated file events = %+v, want only n=2", events)
	}
	assertMode(t, path, 0o600)
	assertMode(t, filepath.Dir(path), 0o700|os.ModeDir)
}

func TestRecorderDropsWhenBufferFull(t *testing.T) {
	dir := t.TempDir()
	opts := testRecorderOptions
	opts.maxBufferedBytes = 4 << 10
	recorder := newRecorder(dir, opts)

	body := bytes.Repeat([]byte("x"), 1<<10)
	for i := 0; i < 20; i++ {
		recorder.RecordPayload("codex", "full", "http_body", "client_to_upstream", body, nil)
	}
	dropped := recorder.DroppedEvents()
	if dropped == 0 || dropped == 20 {
		t.Fatalf("dropped %d of 20 events, want some but not all", dropped)
	}
	if got := recorder.buffered.Load(); got > opts.maxBufferedBytes {
		t.Fatalf("buffered %d bytes, cap %d", got, opts.maxBufferedBytes)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if got := int64(len(readEvents(t, recorder.PathForSession("codex", "full")))); got != 20-dropped {
		t.Fatalf("wrote %d events, want %d", got, 20-dropped)
	}
	if got := recorder.buffered.Load(); got != 0 {
		t.Fatalf("buffered %d bytes after Close, want 0", got)
	}
}

func TestRecorderOutputMatchesLegacyFormat(t *testing.T) {
	events := []struct {
		agent, session string
		event          Event
	}{
		{"codex", "fmt:0", Event{Timestamp: "2026-01-01T00:00:00Z", Type: "subrouter_meta", Payload: map[string]any{"a": 1, "html": "<&>"}}},
		{"Claude", "fmt:1", Event{Timestamp: "2026-01-01T00:00:01Z", Type: "http_body", Payload: map[string]any{"body_base64": "eA=="}}},
		{"codex", "fmt:2", Event{Timestamp: "2026-01-01T00:00:02Z", Type: "http_body_chunk", Payload: map[string]any{"chunk_index": 3}}},
	}
	legacyDir := t.TempDir()
	newDir := t.TempDir()
	recorder := newRecorder(newDir, testRecorderOptions)
	for _, e := range events {
		legacyWrite(filepath.Join(legacyDir, relSessionPath(e.agent, e.session)), e.event)
		recorder.write(e.agent, e.session, e.event)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{relSessionPath("codex", "fmt"), relSessionPath("claude", "fmt")} {
		want, err := os.ReadFile(filepath.Join(legacyDir, rel))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(newDir, rel))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s differs from legacy output:\ngot  %q\nwant %q", rel, got, want)
		}
		assertMode(t, filepath.Join(newDir, rel), 0o600)
	}
}

func BenchmarkRecorderWrite(b *testing.B) {
	for _, bc := range []struct {
		name  string
		write func(dir string) (func(session int, event Event), func())
	}{
		{"legacy", func(dir string) (func(int, Event), func()) {
			var mu sync.Mutex
			return func(session int, event Event) {
				path := filepath.Join(dir, relSessionPath("codex", fmt.Sprintf("bench-%d", session)))
				mu.Lock()
				legacyWrite(path, event)
				mu.Unlock()
			}, func() {}
		}},
		{"buffered", func(dir string) (func(int, Event), func()) {
			recorder := NewRecorder(dir)
			return func(session int, event Event) {
					recorder.write("codex", fmt.Sprintf("bench-%d", session), event)
				}, func() {
					if err := recorder.Close(); err != nil {
						b.Fatal(err)
					}
				}
		}},
	} {
		for _, chunkBytes := range []int{512, 16 << 10} {
			b.Run(fmt.Sprintf("%s/chunk=%d", bc.name, chunkBytes), func(b *testing.B) {
				write, closeFn := bc.write(b.TempDir())
				event := Event{
					Timestamp: now(),
					Type:      "http_body_chunk",
					Payload: map[string]any{
						"agent_type":  "codex",
						"body_chunk":  true,
						"body_base64": strings.Repeat("A", chunkBytes),
					},
				}
				var next sync.Mutex
				session := 0
				b.SetBytes(int64(chunkBytes))
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					next.Lock()
					mine := session % 64
					session++
					next.Unlock()
					for pb.Next() {
						write(mine, event)
					}
				})
				closeFn()
			})
		}
	}
}

// legacyWrite is the recorder's previous write path, kept as the format and
// benchmark baseline: open, append one line, close, per event.
func legacyWrite(path string, event Event) {
	body, err := json.Marshal(event)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(body, '\n'))
}

func relSessionPath(agentType, sessionID string) string {
	return filepath.Join("by-agent", safeFilename(normalizeAgentType(agentType)), "by-session", safeFilename(BaseSessionID(sessionID))+".jsonl")
}

func readEvents(t *testing.T, path string) []Event {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var events []Event
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 32<<20)
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("%s: malformed line %q: %v", path, scanner.Text(), err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode() & (os.ModeDir | os.ModePerm); got != want {
		t.Fatalf("%s mode = %v, want %v", path, got, want)
	}
}
