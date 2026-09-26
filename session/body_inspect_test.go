package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// treeSearchID and treeSearchModel are the tree searches walkJSONFields
// replaced, kept as the reference its results are checked against.
func treeSearchID(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if _, ok := jsonCandidates[strings.ToLower(key)]; ok {
				if str, ok := child.(string); ok && strings.TrimSpace(str) != "" {
					return strings.TrimSpace(str)
				}
			}
		}
		for _, child := range typed {
			if id := treeSearchID(child); id != "" {
				return id
			}
		}
	case []any:
		for _, child := range typed {
			if id := treeSearchID(child); id != "" {
				return id
			}
		}
	}
	return ""
}

func treeSearchModel(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		if str, ok := typed["model"].(string); ok {
			return NormalizeModel(str)
		}
		for _, child := range typed {
			if model := treeSearchModel(child); model != "" {
				return model
			}
		}
	case []any:
		for _, child := range typed {
			if model := treeSearchModel(child); model != "" {
				return model
			}
		}
	}
	return ""
}

// checkWalkMatchesTreeSearch compares the walker with the tree search. The
// tree search visits map entries in random order, so when it can return more
// than one answer the walker only has to return one of them.
func checkWalkMatchesTreeSearch(t *testing.T, data []byte) {
	t.Helper()
	var value any
	unmarshalErr := json.Unmarshal(data, &value)
	id, model, ok := walkJSONFields(data)
	if unmarshalErr != nil || value == nil {
		if ok {
			t.Fatalf("walker accepted a body the tree search rejects: %q", data)
		}
		return
	}
	if !ok {
		t.Fatalf("walker rejected a body the tree search accepts: %q", data)
	}
	ids := map[string]bool{}
	models := map[string]bool{}
	for range 64 {
		ids[treeSearchID(value)] = true
		models[treeSearchModel(value)] = true
	}
	if !ids[id] {
		t.Fatalf("walker id %q, tree search gave %v for %q", id, ids, data)
	}
	if !models[model] {
		t.Fatalf("walker model %q, tree search gave %v for %q", model, models, data)
	}
}

func randomJSONValue(rng *rand.Rand, depth int) string {
	keys := []string{"model", "Model", "session_id", "SESSION_ID", "conversation_id", "thread_id", "Thread_Id",
		`model`, `session_id`, "input", "content", "metadata", "x", "type", "ſession_id", "sessİon_id"}
	strs := []string{`""`, `"  "`, `"a"`, `" b "`, `"c\"d"`, `"é"`, `"gpt-5"`, `"claude-fable-5"`, `"` + strings.Repeat("m", 257) + `"`}
	if depth <= 0 {
		switch rng.Intn(4) {
		case 0:
			return strs[rng.Intn(len(strs))]
		case 1:
			return []string{"1", "-2.5e3", "true", "false", "null", "0"}[rng.Intn(6)]
		case 2:
			return "{}"
		default:
			return "[]"
		}
	}
	switch rng.Intn(3) {
	case 0:
		var b strings.Builder
		b.WriteString("{")
		for i, n := 0, rng.Intn(5); i < n; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`"` + keys[rng.Intn(len(keys))] + `": `)
			b.WriteString(randomJSONValue(rng, depth-1))
		}
		b.WriteString("}")
		return b.String()
	case 1:
		var b strings.Builder
		b.WriteString("[ ")
		for i, n := 0, rng.Intn(4); i < n; i++ {
			if i > 0 {
				b.WriteString(" ,\n")
			}
			b.WriteString(randomJSONValue(rng, depth-1))
		}
		b.WriteString("]")
		return b.String()
	default:
		return randomJSONValue(rng, 0)
	}
}

func TestWalkJSONFieldsMatchesTreeSearch(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for range 20000 {
		checkWalkMatchesTreeSearch(t, []byte(randomJSONValue(rng, 1+rng.Intn(5))))
	}
	for _, body := range []string{
		codexResponsesBody,
		string(realisticCodexBody(64 << 10)),
		`{"n":1e400,"model":"x"}`,
		`{"n":` + strings.Repeat("9", 400) + `,"model":"x"}`,
		`{"n":1e-400,"model":"x"}`,
		` null `,
		"{\"model\":\"a\xffb\",\"session_id\":\"\xff\"}",
		"{\"mod\xffel\":\"a\",\"model\":\"b\"}",
		"{\"session_id\\u0000\":\"a\",\"thread_id\":\"b\"}",
	} {
		checkWalkMatchesTreeSearch(t, []byte(body))
	}
}

func FuzzWalkJSONFields(f *testing.F) {
	f.Add([]byte(codexResponsesBody))
	f.Add([]byte(`{"a":{"session_id":"deep"},"session_id":"top","model":{"model":"m"}}`))
	f.Add([]byte(`[{"thread_id":" t "},{"model":"  "}]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		checkWalkMatchesTreeSearch(t, data)
	})
}

// A conversation longer than --max-body-bytes used to lose its session id and
// fall back to a per-client hash, so its account stickiness broke exactly when
// the conversation was longest. Bodies that fit the model-scan buffer are now
// walked whole.
func TestExtractReadsSessionFromBodiesPastMaxBodyBytes(t *testing.T) {
	body := realisticCodexBody(3 << 20)
	for _, encoding := range []string{"", "gzip", "zstd"} {
		for _, chunked := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/chunked=%t", encoding, chunked), func(t *testing.T) {
				wire := encodeBody(t, encoding, body)
				req := encodedJSONRequest(wire, encoding)
				if chunked {
					req.ContentLength = -1
				}
				if got := ExtractID(req, 1<<20); got != benchSessionID {
					t.Fatalf("ExtractID = %q, want %q", got, benchSessionID)
				}
				if got := ExtractModel(req, 1<<20); got != "gpt-5.3-codex" {
					t.Fatalf("ExtractModel = %q, want gpt-5.3-codex", got)
				}
				forwarded, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(forwarded, wire) {
					t.Fatalf("forwarded body changed: got %d bytes, want %d", len(forwarded), len(wire))
				}
			})
		}
	}
}

// Past the buffer the body is not held whole: the id falls back and the model
// comes from the textual scan of the buffered prefix, as before.
func TestExtractFallsBackPastInspectionBuffer(t *testing.T) {
	body := realisticCodexBody(int(modelScanMaxBodyBytes) + 1<<20)
	req := encodedJSONRequest(body, "")
	if got, want := ExtractID(req, 1<<20), fallbackID(req); got != want {
		t.Fatalf("ExtractID = %q, want fallback %q", got, want)
	}
	if got := ExtractModel(req, 1<<20); got != "gpt-5.3-codex" {
		t.Fatalf("ExtractModel = %q, want gpt-5.3-codex", got)
	}
	forwarded, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(forwarded, body) {
		t.Fatalf("forwarded body changed: got %d bytes, want %d", len(forwarded), len(body))
	}
}

type countingReadCloser struct {
	reader io.Reader
	bytes  int
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	c.bytes += n
	return n, err
}

func (c *countingReadCloser) Close() error { return nil }

// The handler asks for the id and the model several times, some of them on a
// clone of the request. The body is prefetched and inspected once, never
// rewrapped, and inspecting the clone must not consume the bytes the original
// request forwards.
func TestExtractInspectsBodyOnce(t *testing.T) {
	wire := encodeBody(t, "zstd", realisticCodexBody(64<<10))
	req := encodedJSONRequest(wire, "zstd")
	counter := &countingReadCloser{reader: bytes.NewReader(wire)}
	req.Body = counter
	_ = ExtractID(req, 1<<20)
	inspected, ok := req.Body.(*inspectedBody)
	if !ok {
		t.Fatalf("body is %T after inspection", req.Body)
	}
	routing := req.Clone(context.Background())
	for range 3 {
		_ = ExtractModel(routing, 1<<20)
		_ = ExtractID(req, 1<<20)
	}
	if req.Body != inspected || routing.Body != inspected {
		t.Fatal("repeated extraction rewrapped the body")
	}
	if counter.bytes != len(wire) {
		t.Fatalf("original body read %d bytes, want %d", counter.bytes, len(wire))
	}
	forwarded, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(forwarded, wire) {
		t.Fatalf("forwarded body changed after clone inspection: got %d bytes, want %d", len(forwarded), len(wire))
	}
}

// A replaced body is inspected afresh rather than answered from the cache.
func TestExtractReinspectsReplacedBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"session_id":"one","model":"m1"}`))
	req.Header.Set("Content-Type", "application/json")
	if got := ExtractID(req, 1<<20); got != "one" {
		t.Fatalf("ExtractID = %q, want one", got)
	}
	replacement := `{"session_id":"two","model":"m2"}`
	req.Body = io.NopCloser(strings.NewReader(replacement))
	if got := ExtractModel(req, 1<<20); got != "m2" {
		t.Fatalf("ExtractModel after replacement = %q, want m2", got)
	}
	if got := ExtractID(req, 1<<20); got != "two" {
		t.Fatalf("ExtractID after replacement = %q, want two", got)
	}
	forwarded, _ := io.ReadAll(req.Body)
	if string(forwarded) != replacement {
		t.Fatalf("forwarded %q, want %q", forwarded, replacement)
	}
}
