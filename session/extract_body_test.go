package session

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const benchSessionID = "01a01336-3c47-7791-8883-ca794f5bd7c1"

// realisticCodexBody builds a Responses API request shaped like the ones
// codex-cli sends late in a long conversation: the model first, a long
// instruction block, many input items (messages, tool calls and their output,
// encrypted reasoning), the tool list, and the session id nested in
// client_metadata at the end. It grows the input until the body reaches
// approximately targetBytes.
func realisticCodexBody(targetBytes int) []byte {
	quote := func(value string) string {
		encoded, _ := json.Marshal(value)
		return string(encoded)
	}
	var buf bytes.Buffer
	buf.WriteString(`{"model":"gpt-5.3-codex","instructions":`)
	buf.WriteString(quote(strings.Repeat("You are Codex, a coding agent. Follow the repository conventions.\n", 60)))
	buf.WriteString(`,"input":[`)
	tools := `],"tools":[` +
		`{"type":"function","name":"shell","description":"Runs a shell command","strict":false,"parameters":{"type":"object","properties":{"command":{"type":"array","items":{"type":"string"}},"workdir":{"type":"string"},"timeout_ms":{"type":"number"}},"required":["command"]}},` +
		`{"type":"function","name":"apply_patch","description":"Applies a patch","strict":false,"parameters":{"type":"object","properties":{"input":{"type":"string"}},"required":["input"]}}` +
		`],"tool_choice":"auto","parallel_tool_calls":false,"reasoning":{"effort":"medium","summary":"auto"},"store":false,"stream":true,` +
		`"include":["reasoning.encrypted_content"],"prompt_cache_key":"` + benchSessionID + `",` +
		`"client_metadata":{"session_id":"` + benchSessionID + `","thread_id":"` + benchSessionID + `"}}`
	for turn := 0; buf.Len()+len(tools) < targetBytes; turn++ {
		if turn > 0 {
			buf.WriteByte(',')
		}
		fmt.Fprintf(&buf, `{"type":"message","role":"user","content":[{"type":"input_text","text":%s}]},`,
			quote(fmt.Sprintf("Turn %d: please look at internal/proxy/proxy.go and explain the \"model\" handling.", turn)))
		fmt.Fprintf(&buf, `{"type":"reasoning","summary":[{"type":"summary_text","text":"Inspecting files"}],"encrypted_content":"%s"},`,
			strings.Repeat("gAAAAABo", 128))
		fmt.Fprintf(&buf, `{"type":"function_call","name":"shell","arguments":%s,"call_id":"call_%d"},`,
			quote(`{"command":["bash","-lc","sed -n 1,200p internal/proxy/proxy.go"],"workdir":"/repo"}`), turn)
		fmt.Fprintf(&buf, `{"type":"function_call_output","call_id":"call_%d","output":%s},`,
			turn, quote(strings.Repeat("func (s Server) handle(w http.ResponseWriter, r *http.Request) {\n\t// \"session_id\": \"not-this-one\"\n}\n", 20)))
		fmt.Fprintf(&buf, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":%s}]}`,
			quote(fmt.Sprintf("Turn %d answer: the handler extracts the model once per request.", turn)))
	}
	buf.WriteString(tools)
	return buf.Bytes()
}

func gzipBytes(t testing.TB, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(body); err != nil {
		t.Fatalf("write gzip body: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return buf.Bytes()
}

func encodeBody(t testing.TB, encoding string, body []byte) []byte {
	t.Helper()
	switch encoding {
	case "":
		return body
	case "gzip":
		return gzipBytes(t, body)
	case "zstd":
		return zstdBytes(t, string(body))
	default:
		t.Fatalf("unknown encoding %q", encoding)
		return nil
	}
}

func encodedJSONRequest(wire []byte, encoding string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(wire))
	req.Header.Set("Content-Type", "application/json")
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	req.ContentLength = int64(len(wire))
	return req
}

func TestRealisticCodexBodyIsValidJSON(t *testing.T) {
	body := realisticCodexBody(500 << 10)
	if !json.Valid(body) {
		t.Fatal("benchmark body is not valid JSON")
	}
	if len(body) < 500<<10 {
		t.Fatalf("benchmark body is %d bytes, want at least 500KiB", len(body))
	}
}

// TestExtractBodyFieldsAreStable pins what ExtractID and ExtractModel derive
// from request bodies, including the edge cases of the JSON search: top-level
// candidates beat nested ones, keys match case-insensitively and after
// unescaping, a whitespace-only top-level model stops the search, duplicate
// keys follow last-wins, and a body encoding/json cannot represent falls back
// to the textual model scan.
func TestExtractBodyFieldsAreStable(t *testing.T) {
	type bodyCase struct {
		name        string
		contentType string
		body        string
		wantID      string // "" means the fallback id
		wantModel   string
	}
	longModel := strings.Repeat("m", 257)
	cases := []bodyCase{
		{name: "nested session id", body: `{"metadata":{"session_id":"s1"},"model":"m1"}`, wantID: "s1", wantModel: "m1"},
		{name: "top-level beats nested", body: `{"a":{"session_id":"deep"},"session_id":"top","model":"m"}`, wantID: "top", wantModel: "m"},
		{name: "array trims value", body: `{"input":[{"x":1},{"thread_id":" t2 "}]}`, wantID: "t2", wantModel: ""},
		{name: "case-insensitive key", body: `{"Session_ID":"up","MODEL":"ignored"}`, wantID: "up", wantModel: ""},
		{name: "escaped keys", body: `{"session_id":"esc","model":"escm"}`, wantID: "esc", wantModel: "escm"},
		{name: "nested model", body: `{"request":{"model":"nested"}}`, wantModel: "nested"},
		{name: "blank top model stops search", body: `{"model":"  ","x":{"model":"inner"}}`, wantModel: ""},
		{name: "non-string model searches children", body: `{"model":{"model":"inner"}}`, wantModel: "inner"},
		{name: "blank id searches children", body: `{"session_id":"  ","x":{"session_id":"n"}}`, wantID: "n"},
		{name: "non-string id", body: `{"session_id":123}`},
		{name: "invalid json scans model", body: `{"model":"broken",`, wantModel: "broken"},
		{name: "top-level array", body: `[{"session_id":"arr","model":"am"}]`, wantID: "arr", wantModel: "am"},
		{name: "duplicate id last wins", body: `{"session_id":"first","session_id":"second"}`, wantID: "second"},
		{name: "duplicate id non-string last", body: `{"session_id":"a","session_id":1}`},
		{name: "duplicate model non-string last", body: `{"model":"a","model":{"x":1}}`, wantModel: ""},
		{name: "unrepresentable number", body: `{"n":1e400,"model":"big","session_id":"s"}`, wantModel: "big"},
		{name: "escaped model", body: `{"model":"gpt\"5é"}`, wantModel: "gpt\"5é"},
		{name: "overlong model", body: `{"model":"` + longModel + `"}`, wantModel: ""},
		{name: "unicode id value", body: `{"conversation_id":"café-😀"}`, wantID: "café-\U0001F600"},
		{name: "empty object", body: `{}`},
		{name: "null document", body: `null`},
		{name: "scalar document", body: `"{\"model\":\"x\"}"`},
		{name: "not json content type", contentType: "text/plain", body: `{"session_id":"s","model":"m"}`},
		{name: "codex shape", body: codexResponsesBody, wantID: benchSessionID, wantModel: "gpt-5.3-codex"},
	}
	for _, encoding := range []string{"", "gzip", "zstd"} {
		for _, tc := range cases {
			t.Run(encoding+"/"+tc.name, func(t *testing.T) {
				wire := encodeBody(t, encoding, []byte(tc.body))
				req := encodedJSONRequest(wire, encoding)
				if tc.contentType != "" {
					req.Header.Set("Content-Type", tc.contentType)
				}
				fallback := fallbackID(req)
				wantID := tc.wantID
				if wantID == "" {
					wantID = fallback
				}
				// The handler asks for the id once and the model several
				// times; every call must agree and leave the body intact.
				for round := 0; round < 3; round++ {
					if got := ExtractID(req, 1<<20); got != wantID {
						t.Fatalf("round %d ExtractID = %q, want %q", round, got, wantID)
					}
					if got := ExtractModel(req, 1<<20); got != tc.wantModel {
						t.Fatalf("round %d ExtractModel = %q, want %q", round, got, tc.wantModel)
					}
				}
				forwarded, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("read forwarded body: %v", err)
				}
				if !bytes.Equal(forwarded, wire) {
					t.Fatalf("forwarded body changed: got %d bytes, want %d", len(forwarded), len(wire))
				}
			})
		}
	}
}

// TestExtractLeavesLargeBodiesByteIdentical covers bodies around and past the
// inspection limits in every supported encoding: whatever the extractors
// read, the bytes left on the request must be exactly what the client sent.
func TestExtractLeavesLargeBodiesByteIdentical(t *testing.T) {
	sizes := []int{2 << 10, 500 << 10, 3 << 20, 9 << 20}
	for _, encoding := range []string{"", "gzip", "zstd"} {
		for _, size := range sizes {
			t.Run(fmt.Sprintf("%s/%d", encoding, size), func(t *testing.T) {
				body := realisticCodexBody(size)
				wire := encodeBody(t, encoding, body)
				req := encodedJSONRequest(wire, encoding)
				_ = ExtractID(req, 1<<20)
				_ = ExtractModel(req, 1<<20)
				_ = ExtractModel(req, 1<<20)
				_ = ExtractID(req, 1<<20)
				forwarded, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("read forwarded body: %v", err)
				}
				if !bytes.Equal(forwarded, wire) {
					t.Fatalf("forwarded body changed: got %d bytes, want %d", len(forwarded), len(wire))
				}
				if err := req.Body.Close(); err != nil {
					t.Fatalf("close forwarded body: %v", err)
				}
			})
		}
	}
}

// BenchmarkExtractHandlerSequence mirrors the body inspection the proxy
// handler performs for one Codex request: ExtractID for session stickiness,
// then ExtractModel for routing and again inside account selection, with the
// body forwarded afterwards.
func BenchmarkExtractHandlerSequence(b *testing.B) {
	body := realisticCodexBody(500 << 10)
	for _, encoding := range []string{"identity", "zstd", "gzip"} {
		b.Run(encoding, func(b *testing.B) {
			wireEncoding := encoding
			if encoding == "identity" {
				wireEncoding = ""
			}
			wire := encodeBody(b, wireEncoding, body)
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				req := encodedJSONRequest(wire, wireEncoding)
				_ = ExtractID(req, 1<<20)
				_ = ExtractModel(req, 1<<20)
				_ = ExtractModel(req, 1<<20)
				_, _ = io.Copy(io.Discard, req.Body)
			}
		})
	}
}
