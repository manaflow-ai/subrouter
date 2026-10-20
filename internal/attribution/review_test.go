package attribution

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// A prompt typed during a tool loop rides next to tool_result blocks.
func TestExtractClaudePromptKeepsTextBesideToolResults(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"t1","content":"ok"},
		{"type":"text","text":"stop, use the other branch"}]}]}`)
	got, ok := ExtractClaudePrompt(body)
	if !ok || got != "stop, use the other branch" {
		t.Fatalf("prompt = %q, %v", got, ok)
	}
}

func TestSubmitDropsPastByteBudget(t *testing.T) {
	block := make(chan struct{})
	identify := func(context.Context, string) (Identity, bool) { <-block; return Identity{LoginName: "a@b"}, true }
	ledger, err := Open(t.TempDir(), Options{Identify: identify, MaxQueuedBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { close(block); ledger.Close() }()
	big := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("x", 80) + `"}]}`)
	for i := 0; i < 5; i++ {
		ledger.Submit(Request{SessionID: "s"}, big)
	}
	if ledger.Stats().DroppedQueue == 0 {
		t.Fatal("bodies past the byte budget were queued")
	}
}

func TestSubmitDecodesZstdOffThePath(t *testing.T) {
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	ledger, _ := newTestLedger(t, fixedIdentity("aziz@manaflow.ai", "m"), func() time.Time { return now })
	var wire bytes.Buffer
	enc, _ := zstd.NewWriter(&wire)
	enc.Write([]byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"zstd ask"}]}]}`))
	enc.Close()
	ledger.Submit(Request{SessionID: "s", ContentEncoding: "zstd"}, wire.Bytes())
	ledger.Flush()
	entries, _ := ledger.Query(Filter{Since: now.Add(-time.Minute), Until: now.Add(time.Minute)})
	if len(entries) != 1 || entries[0].Prompt != "zstd ask" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestQueryKeepsNewestWithinLimit(t *testing.T) {
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	clock := now
	ledger, _ := newTestLedger(t, fixedIdentity("a@manaflow.ai", "m"), func() time.Time { return clock })
	for i := 0; i < 5; i++ {
		clock = now.Add(time.Duration(i) * time.Minute)
		ledger.Submit(Request{SessionID: string(rune('a' + i))}, prompt("p"+string(rune('0'+i))))
		ledger.Flush()
	}
	entries, _ := ledger.Query(Filter{Since: now.Add(-time.Hour), Until: now.Add(time.Hour), Limit: 2})
	if len(entries) != 2 || entries[0].Prompt != "p3" || entries[1].Prompt != "p4" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestFilterMatchesOneOfSeveralTags(t *testing.T) {
	if !(Filter{Person: "tag:fleet"}).Matches("tag:ci,tag:fleet") {
		t.Fatal("tag filter did not match a multi-tag person")
	}
}

func TestLongHeaderFieldsAreTruncated(t *testing.T) {
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	ledger, _ := newTestLedger(t, fixedIdentity("a@manaflow.ai", "m"), func() time.Time { return now })
	ledger.Submit(Request{SessionID: "s", UserAgent: strings.Repeat("u", 10<<20)}, prompt("x"))
	ledger.Submit(Request{SessionID: "t"}, prompt("after"))
	ledger.Flush()
	entries, _ := ledger.Query(Filter{Since: now.Add(-time.Minute), Until: now.Add(time.Minute)})
	if len(entries) != 2 || len(entries[0].UserAgent) > MaxFieldBytes {
		t.Fatalf("got %d entries", len(entries))
	}
}

func TestFlushAfterCloseReturns(t *testing.T) {
	ledger, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	ledger.Close()
	done := make(chan struct{})
	go func() { ledger.Flush(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Flush after Close blocked")
	}
}
