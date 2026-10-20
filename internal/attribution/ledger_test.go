package attribution

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedIdentity(login, node string, tags ...string) IdentifyFunc {
	return func(context.Context, string) (Identity, bool) {
		return Identity{LoginName: login, NodeName: node, Tags: tags}, true
	}
}

func TestExtractClaudePromptTakesLastHumanText(t *testing.T) {
	body := []byte(`{"model":"claude-opus","messages":[
		{"role":"user","content":"first ask"},
		{"role":"assistant","content":[{"type":"text","text":"ok"}]},
		{"role":"user","content":[
			{"type":"text","text":"<system-reminder>noise</system-reminder>"},
			{"type":"text","text":"fix the sidebar lag"}]}]}`)
	got, ok := ExtractClaudePrompt(body)
	if !ok || got != "fix the sidebar lag" {
		t.Fatalf("prompt = %q, %v; want the last human text without reminders", got, ok)
	}
}

func TestExtractClaudePromptSkipsToolResultTurns(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"t1","content":"ls output"},
		{"type":"text","text":"<system-reminder>todo list</system-reminder>"}]}]}`)
	if got, ok := ExtractClaudePrompt(body); ok {
		t.Fatalf("tool-result turn recorded as a prompt: %q", got)
	}
}

func TestPersonPrefersTailnetLoginOverClaimedEmail(t *testing.T) {
	if got := PersonFor(Identity{LoginName: "leo@manaflow.ai"}, true); got != "leo@manaflow.ai" {
		t.Fatalf("person = %q", got)
	}
	if got := PersonFor(Identity{LoginName: "tagged-devices", Tags: []string{"tag:fleet"}}, true); got != "tag:fleet" {
		t.Fatalf("tagged person = %q", got)
	}
	if got := PersonFor(Identity{}, false); got != Unattributed {
		t.Fatalf("unknown peer person = %q", got)
	}
}

func newTestLedger(t *testing.T, identify IdentifyFunc, now func() time.Time) (*Ledger, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ledger")
	ledger, err := Open(dir, Options{Identify: identify, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ledger.Close() })
	return ledger, dir
}

func prompt(text string) []byte {
	return []byte(`{"messages":[{"role":"user","content":"` + text + `"}]}`)
}

func TestLedgerRecordsQueriesAndDedupes(t *testing.T) {
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	ledger, dir := newTestLedger(t, fixedIdentity("leo@manaflow.ai", "leo-mbp"), func() time.Time { return now })

	meta := Request{RemoteAddr: "100.64.0.9:1234", Agent: "claude", SessionID: "s1", Model: "claude-opus", Path: "/v1/messages", ClaimedUser: "spoof@evil.test"}
	ledger.Submit(meta, prompt("ship the nightly"))
	ledger.Submit(meta, prompt("ship the nightly")) // retry of the same turn
	ledger.Submit(meta, prompt("now fix the tests"))
	ledger.Flush()

	entries, err := ledger.Query(Filter{Person: "leo", Since: now.Add(-time.Hour), Until: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 (retry deduped): %+v", len(entries), entries)
	}
	e := entries[0]
	if e.Person != "leo@manaflow.ai" || e.Node != "leo-mbp" || e.ClaimedUser != "spoof@evil.test" || e.Prompt != "ship the nightly" || e.PromptSHA256 == "" {
		t.Fatalf("entry = %+v", e)
	}
	if other, _ := ledger.Query(Filter{Person: "aziz", Since: now.Add(-time.Hour), Until: now.Add(time.Hour)}); len(other) != 0 {
		t.Fatalf("aziz filter matched %d", len(other))
	}
	info, err := os.Stat(filepath.Join(dir, "2026-10-20.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("ledger file mode = %v, want 0600", info.Mode().Perm())
	}
	if s := ledger.Stats(); s.Recorded != 2 || s.Deduped != 1 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestLedgerTruncatesLongPrompts(t *testing.T) {
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	ledger, _ := newTestLedger(t, fixedIdentity("lawrence@manaflow.ai", "mbp"), func() time.Time { return now })
	ledger.Submit(Request{SessionID: "s", Path: "/v1/messages"}, prompt(strings.Repeat("a", MaxPromptChars+50)))
	ledger.Flush()
	entries, _ := ledger.Query(Filter{Since: now.Add(-time.Minute), Until: now.Add(time.Minute)})
	if len(entries) != 1 || !entries[0].PromptTruncated || len([]rune(entries[0].Prompt)) != MaxPromptChars {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestLedgerPrunesPastRetention(t *testing.T) {
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	ledger, dir := newTestLedger(t, fixedIdentity("lawrence@manaflow.ai", "mbp"), func() time.Time { return now })
	old := filepath.Join(dir, "2026-09-01.jsonl")
	if err := os.WriteFile(old, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "2026-10-01.jsonl")
	if err := os.WriteFile(keep, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger.Prune()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("file past %v retention still present", DefaultRetention)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("file inside retention removed: %v", err)
	}
}

func TestLedgerMarksUnknownPeersUnattributed(t *testing.T) {
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	none := func(context.Context, string) (Identity, bool) { return Identity{}, false }
	ledger, _ := newTestLedger(t, none, func() time.Time { return now })
	ledger.Submit(Request{SessionID: "s", Path: "/v1/messages", ClaimedUser: "lawrence@manaflow.ai"}, prompt("hi"))
	ledger.Flush()
	entries, _ := ledger.Query(Filter{Person: Unattributed, Since: now.Add(-time.Minute), Until: now.Add(time.Minute)})
	if len(entries) != 1 || entries[0].Person != Unattributed {
		t.Fatalf("a self-reported email must not become the person: %+v", entries)
	}
}

// Claude Code 2.1.3xx appends a "system" role message (environment block)
// after the user turn; the prompt is the last user message before it.
func TestExtractClaudePromptSkipsTrailingSystemMessages(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"user","content":[
			{"type":"text","text":"<system-reminder>\nCodebase instructions\n</system-reminder>"},
			{"type":"text","text":"capture probe hello"}]},
		{"role":"system","content":[{"type":"text","text":"# Environment\n..."}]}]}`)
	got, ok := ExtractClaudePrompt(body)
	if !ok || got != "capture probe hello" {
		t.Fatalf("prompt = %q, %v", got, ok)
	}
}
