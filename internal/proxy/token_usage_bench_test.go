package proxy

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// feedTokenUsageStream writes stream in 32 KiB reads, as the reverse proxy
// copies it, and returns the time spent inside Write, which is time the
// client waits for its bytes.
func feedTokenUsageStream(scanner *tokenUsageScanner, stream []byte) time.Duration {
	var inWrite time.Duration
	for data := stream; len(data) > 0; {
		n := 32 << 10
		if n > len(data) {
			n = len(data)
		}
		start := time.Now()
		scanner.Write(data[:n])
		inWrite += time.Since(start)
		data = data[n:]
	}
	return inWrite
}

// BenchmarkTokenUsageScannerCodexStream measures one realistic long Codex
// HTTP turn (about 866 KB). client-ns/op is the time spent inside Write,
// before the bytes reach the client; the rest of ns/op happens at EOF.
func BenchmarkTokenUsageScannerCodexStream(b *testing.B) {
	for _, usageFirst := range []bool{false, true} {
		name := "usage_last"
		if usageFirst {
			name = "usage_first"
		}
		b.Run(name, func(b *testing.B) {
			stream := []byte(realisticCodexStream(b, usageFirst))
			b.SetBytes(int64(len(stream)))
			b.ReportAllocs()
			var client time.Duration
			b.ResetTimer()
			for range b.N {
				scanner := newTokenUsageScanner("text/event-stream")
				client += feedTokenUsageStream(scanner, stream)
				scanner.Finish()
			}
			b.ReportMetric(float64(client.Nanoseconds())/float64(b.N), "client-ns/op")
		})
	}
}

// BenchmarkTokenUsageScannerLargeTerminalEvent measures a response.completed
// event under and over the whole-line bound. Past the bound the scanner falls
// back to head and tail, so memory stays bounded however large the event is.
func BenchmarkTokenUsageScannerLargeTerminalEvent(b *testing.B) {
	for _, size := range []int{512 << 10, 3 << 20, 16 << 20} {
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			reasoning := map[string]any{"id": "rs_1", "type": "reasoning", "encrypted_content": strings.Repeat("g", size)}
			stream := []byte(codexSSEEvent(b, "response.completed", 1, realisticCodexResponse("completed", map[string]any{
				"input_tokens": 1, "output_tokens": 1,
			}, []any{reasoning}, false), nil))
			b.SetBytes(int64(len(stream)))
			b.ReportAllocs()
			var client time.Duration
			b.ResetTimer()
			for range b.N {
				scanner := newTokenUsageScanner("text/event-stream")
				client += feedTokenUsageStream(scanner, stream)
				scanner.Finish()
			}
			b.ReportMetric(float64(client.Nanoseconds())/float64(b.N), "client-ns/op")
		})
	}
}

// A realistic long Codex stream must cost about what the old head and tail
// scanner did (about 550 KB allocated for this stream, nearly all of it
// decoding small deltas): only the terminal event is kept whole, and nothing
// that echoes instructions, tools, or encrypted reasoning is decoded.
func TestTokenUsageScannerCodexStreamAllocationBudget(t *testing.T) {
	for _, usageFirst := range []bool{false, true} {
		stream := []byte(realisticCodexStream(t, usageFirst))
		completed := len(stream) - bytes.LastIndex(stream, []byte("event: response.completed"))
		const runs = 5
		var ok bool
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		for range runs {
			scanner := newTokenUsageScanner("text/event-stream")
			feedTokenUsageStream(scanner, stream)
			_, _, ok = scanner.Finish()
		}
		runtime.ReadMemStats(&after)
		perStream := int((after.TotalAlloc - before.TotalAlloc) / runs)
		if !ok {
			t.Fatalf("usage_first=%v: no usage", usageFirst)
		}
		// Keeping the terminal event whole costs twice its size (its pieces,
		// then one joined copy); the head and tail buffers are fixed.
		if budget := 2*completed + 256<<10; perStream > budget {
			t.Fatalf("usage_first=%v: %d bytes allocated per %d byte stream (terminal event %d), want at most %d",
				usageFirst, perStream, len(stream), completed, budget)
		}
		t.Logf("usage_first=%v: %d bytes allocated per %d byte stream (terminal event %d)", usageFirst, perStream, len(stream), completed)
	}
}

// Events that echo the request but are not terminal keep the head and tail
// path however large they are.
func TestTokenUsageScannerKeepsOnlyTerminalEventsWhole(t *testing.T) {
	scanner := newTokenUsageScanner("text/event-stream")
	created := codexSSEEvent(t, "response.created", 0, realisticCodexResponse("in_progress", nil, []any{}, false), nil)
	if len(created) <= tokenUsageLineKeepBytes {
		t.Fatalf("created event is %d bytes, want more than the keep size", len(created))
	}
	// Stop inside the data line, before its newline.
	scanner.Write([]byte(created[:len(created)-2]))
	if scanner.whole || cap(scanner.head) > 2*tokenUsageLineKeepBytes {
		t.Fatalf("response.created kept whole: whole=%v head cap %d", scanner.whole, cap(scanner.head))
	}
}

// A large terminal event is decoded at Finish, not while its bytes are on the
// way to the client.
func TestTokenUsageScannerDefersLargeTerminalDecode(t *testing.T) {
	scanner := newTokenUsageScanner("text/event-stream")
	feedTokenUsageStream(scanner, []byte(realisticCodexStream(t, true)))
	if _, _, got := scanner.acc.result(); got {
		t.Fatal("usage decoded inside Write, want it deferred to Finish")
	}
	if scanner.pending == nil {
		t.Fatal("no pending terminal event after the stream")
	}
	usage, _, ok := scanner.Finish()
	if !ok || usage.InputTokens != 91234 {
		t.Fatalf("usage = %+v ok %v", usage, ok)
	}
}

// Past the whole-line bound a terminal event falls back to its head and tail:
// usage near the end is still found, usage far from it is the bound's cost.
func TestTokenUsageScannerFallsBackPastWholeLineBound(t *testing.T) {
	for _, usageFirst := range []bool{false, true} {
		scanner := newTokenUsageScannerWithLimit("text/event-stream", tokenUsageLineKeepBytes)
		feedTokenUsageStream(scanner, []byte(realisticCodexStream(t, usageFirst)))
		usage, _, ok := scanner.Finish()
		if ok != !usageFirst || (ok && usage.InputTokens != 91234) {
			t.Fatalf("usage_first=%v: usage = %+v ok %v", usageFirst, usage, ok)
		}
	}
}
