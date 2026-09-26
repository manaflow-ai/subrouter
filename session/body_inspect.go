package session

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// bodyInspection is everything ExtractID, ExtractModel and ExtractServiceTier
// derive from a request body. An empty id means the body names no session.
type bodyInspection struct {
	id          string
	model       string
	serviceTier string
}

type bodyInspectionKey struct {
	wireLimit       int64
	contentEncoding string
}

// inspectedBody replaces a request body once the extractors have looked at it.
// It replays the prefetched wire bytes and then the rest of the original body,
// so whoever forwards the request reads exactly what the client sent. It also
// remembers what the extractors found: the proxy handler asks for the session
// id and the model several times per request, and decoding and parsing a long
// conversation for each question used to dominate the request's CPU cost.
//
// The cache lives on the body rather than on the request context so that it
// cannot outlive the bytes it describes: a caller that replaces r.Body gets a
// fresh inspection, and one that starts reading it (for example to forward it)
// is never answered from bytes it has already moved past. Clones made with
// r.Clone share the body and so share the cache.
type inspectedBody struct {
	mu      sync.Mutex
	rest    io.ReadCloser
	wire    []byte
	offset  int
	eof     bool  // rest reached EOF while prefetching; wire holds the whole body
	failed  bool  // rest failed while prefetching
	err     error // that failure, replayed after the buffered bytes
	started bool  // a consumer has read from the body

	cached bool
	key    bodyInspectionKey
	result bodyInspection
}

func (b *inspectedBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.started = true
	if b.offset < len(b.wire) {
		n := copy(p, b.wire[b.offset:])
		b.offset += n
		return n, nil
	}
	if b.err != nil {
		// The body broke while it was prefetched. Report that instead of
		// reading on, so a truncated body is never forwarded as complete.
		return 0, b.err
	}
	return b.rest.Read(p)
}

func (b *inspectedBody) Close() error {
	return b.rest.Close()
}

// ensureWire prefetches until the body is complete or more than limit bytes
// are buffered. The caller holds b.mu and the body has not been read yet.
func (b *inspectedBody) ensureWire(limit int64) {
	if b.eof || b.failed || int64(len(b.wire)) > limit {
		return
	}
	want := limit + 1 - int64(len(b.wire))
	more, err := io.ReadAll(io.LimitReader(b.rest, want))
	if b.wire == nil {
		b.wire = more
	} else {
		b.wire = append(b.wire, more...)
	}
	if err != nil {
		b.failed = true
		b.err = err
		return
	}
	if int64(len(more)) < want {
		b.eof = true
	}
}

func (b *inspectedBody) inspect(wireLimit int64, contentEncoding string) bodyInspection {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := bodyInspectionKey{wireLimit: wireLimit, contentEncoding: contentEncoding}
	if b.cached && b.key == key {
		return b.result
	}
	b.ensureWire(wireLimit)
	result := inspectWire(b.wire, b.failed, wireLimit, contentEncoding)
	b.cached, b.key, b.result = true, key, result
	return result
}

// inspectRequestBody returns the session id and model the request body names.
// The body is read and decoded at most once per request for a given limit;
// later calls reuse the result as long as nobody has started reading the body.
//
// Up to max(maxBodyBytes, modelScanMaxBodyBytes) wire bytes are buffered. A
// body that fits is decoded and walked once without building a Go value tree,
// so a long conversation past maxBodyBytes still yields its session id and
// model. A larger body gets no session id, and its model comes from a textual
// scan of the buffered prefix.
func inspectRequestBody(r *http.Request, maxBodyBytes int64) bodyInspection {
	if r == nil || r.Body == nil || maxBodyBytes <= 0 {
		return bodyInspection{}
	}
	if contentType := r.Header.Get("Content-Type"); !strings.Contains(contentType, "json") {
		return bodyInspection{}
	}
	wireLimit := max(maxBodyBytes, modelScanMaxBodyBytes)
	body, ok := r.Body.(*inspectedBody)
	if ok {
		body.mu.Lock()
		started := body.started
		body.mu.Unlock()
		ok = !started
	}
	if !ok {
		body = &inspectedBody{rest: r.Body}
		r.Body = body
	}
	return body.inspect(wireLimit, r.Header.Get("Content-Encoding"))
}

func inspectWire(wire []byte, failed bool, wireLimit int64, contentEncoding string) bodyInspection {
	if failed {
		return bodyInspection{}
	}
	if int64(len(wire)) > wireLimit {
		// Too large to hold whole: scan the raw prefix for a model field, which
		// still finds it in an uncompressed body.
		prefix := wire[:wireLimit]
		return bodyInspection{model: scanJSONModelField(prefix), serviceTier: scanJSONServiceTier(prefix)}
	}
	decoded, truncated := decodeRequestBody(wire, contentEncoding)
	if decoded == nil {
		return bodyInspection{}
	}
	tier := scanJSONServiceTier(decoded)
	if !truncated {
		if id, model, ok := walkJSONFields(decoded); ok {
			return bodyInspection{id: id, model: model, serviceTier: tier}
		}
	}
	return bodyInspection{model: scanJSONModelField(decoded), serviceTier: tier}
}

func scanJSONModelField(body []byte) string {
	match := jsonModelFieldPattern.FindSubmatch(body)
	if len(match) != 2 {
		return ""
	}
	unquoted, err := strconv.Unquote(`"` + string(match[1]) + `"`)
	if err != nil {
		return ""
	}
	return NormalizeModel(unquoted)
}

// walkJSONFields finds the session id and model in one pass over a JSON
// document. ok is false when encoding/json would not decode the document into
// an interface value, so callers fall back exactly where they did when the
// body was unmarshaled.
//
// The search rules are those of the former tree search. For the session id,
// an object's own non-blank string session_id, conversation_id or thread_id
// (keys matched case-insensitively) wins over anything nested in it; otherwise
// the first nested match wins. For the model, an object's own "model" string
// decides the answer for that object, even when blank; otherwise the first
// nested match wins. Duplicate keys follow encoding/json's last-wins rule.
// Where the old search visited map entries in random order, this walk takes
// the first in document order, which is one of the answers the old search
// could give.
func walkJSONFields(data []byte) (id, model string, ok bool) {
	if !json.Valid(data) {
		return "", "", false
	}
	walker := jsonFieldWalker{data: data}
	id, model = walker.value()
	if walker.unrepresentable {
		return "", "", false
	}
	if walker.null {
		// encoding/json yields a nil value for a null document, which the
		// former code treated like an unparseable body.
		return "", "", false
	}
	return id, model, true
}

// jsonFieldWalker walks a document that json.Valid has accepted, so it only
// needs to find token boundaries, not report syntax errors.
type jsonFieldWalker struct {
	data            []byte
	pos             int
	depth           int
	null            bool
	unrepresentable bool
}

type jsonObjectEntry struct {
	key         string
	directID    string
	childID     string
	childModel  string
	model       string
	modelString bool
}

func (w *jsonFieldWalker) skipSpace() {
	for w.pos < len(w.data) {
		switch w.data[w.pos] {
		case ' ', '\t', '\n', '\r':
			w.pos++
		default:
			return
		}
	}
}

func (w *jsonFieldWalker) value() (id, model string) {
	w.skipSpace()
	switch w.data[w.pos] {
	case '{':
		w.depth++
		id, model = w.object()
		w.depth--
	case '[':
		w.depth++
		id, model = w.array()
		w.depth--
	case '"':
		w.stringToken()
	case 't':
		w.pos += len("true")
	case 'f':
		w.pos += len("false")
	case 'n':
		if w.depth == 0 {
			w.null = true
		}
		w.pos += len("null")
	default:
		w.number()
	}
	return id, model
}

func (w *jsonFieldWalker) array() (id, model string) {
	w.pos++ // [
	w.skipSpace()
	if w.data[w.pos] == ']' {
		w.pos++
		return "", ""
	}
	for {
		childID, childModel := w.value()
		if id == "" {
			id = childID
		}
		if model == "" {
			model = childModel
		}
		w.skipSpace()
		if w.data[w.pos] == ',' {
			w.pos++
			continue
		}
		w.pos++ // ]
		return id, model
	}
}

func (w *jsonFieldWalker) object() (id, model string) {
	w.pos++ // {
	w.skipSpace()
	if w.data[w.pos] == '}' {
		w.pos++
		return "", ""
	}
	// Most objects in a request body contribute nothing, so entries are only
	// recorded for keys that do, and the backing array stays on the stack.
	var backing [4]jsonObjectEntry
	entries := backing[:0]
	for {
		w.skipSpace()
		rawKey := w.stringToken()
		w.skipSpace()
		w.pos++ // :
		key := jsonKey{raw: rawKey}
		if len(entries) > 0 {
			entries = key.dropFrom(entries)
		}
		isModel, isID := key.classify()
		w.skipSpace()
		if w.data[w.pos] == '"' {
			rawValue := w.stringToken()
			switch {
			case isModel:
				entries = append(entries, jsonObjectEntry{key: key.String(), model: NormalizeModel(unquoteJSONString(rawValue)), modelString: true})
			case isID:
				if value := strings.TrimSpace(unquoteJSONString(rawValue)); value != "" {
					entries = append(entries, jsonObjectEntry{key: key.String(), directID: value})
				}
			}
		} else if childID, childModel := w.value(); childID != "" || childModel != "" {
			entries = append(entries, jsonObjectEntry{key: key.String(), childID: childID, childModel: childModel})
		}
		w.skipSpace()
		if w.data[w.pos] == ',' {
			w.pos++
			continue
		}
		w.pos++ // }
		break
	}

	modelDecided := false
	for _, entry := range entries {
		if entry.modelString {
			model, modelDecided = entry.model, true
			break
		}
	}
	for _, entry := range entries {
		if !modelDecided && model == "" && entry.childModel != "" {
			model = entry.childModel
		}
		if id == "" && entry.directID != "" {
			id = entry.directID
		}
	}
	if id == "" {
		for _, entry := range entries {
			if entry.childID != "" {
				id = entry.childID
				break
			}
		}
	}
	return id, model
}

// stringToken returns the raw string token at pos, quotes included.
func (w *jsonFieldWalker) stringToken() []byte {
	start := w.pos
	cursor := start + 1
	for {
		end := cursor + bytes.IndexByte(w.data[cursor:], '"')
		backslashes := 0
		for index := end - 1; index > start && w.data[index] == '\\'; index-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			w.pos = end + 1
			return w.data[start:w.pos]
		}
		cursor = end + 1
	}
}

func (w *jsonFieldWalker) number() {
	start := w.pos
	exponent := false
scan:
	for w.pos < len(w.data) {
		switch c := w.data[w.pos]; {
		case c >= '0' && c <= '9', c == '-', c == '+', c == '.':
		case c == 'e' || c == 'E':
			exponent = true
		default:
			break scan
		}
		w.pos++
	}
	// encoding/json decodes numbers into float64 and rejects the whole
	// document when one overflows, which only a long or exponent form can.
	if exponent || w.pos-start > 300 {
		if _, err := strconv.ParseFloat(string(w.data[start:w.pos]), 64); err != nil {
			w.unrepresentable = true
		}
	}
}

// jsonKey is an object key as it appears in the document. Plain ASCII keys,
// which is nearly all of them, are compared in place without allocating.
type jsonKey struct {
	raw      []byte
	decoded  string
	resolved bool
}

func (k *jsonKey) plain() bool {
	content := k.raw[1 : len(k.raw)-1]
	for _, c := range content {
		if c == '\\' || c >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// String returns the key as encoding/json would decode it.
func (k *jsonKey) String() string {
	if !k.resolved {
		k.decoded = unquoteJSONString(k.raw)
		k.resolved = true
	}
	return k.decoded
}

func (k *jsonKey) equals(other string) bool {
	if !k.resolved && k.plain() {
		return string(k.raw[1:len(k.raw)-1]) == other
	}
	return k.String() == other
}

func (k *jsonKey) classify() (isModel, isID bool) {
	if !k.resolved && k.plain() {
		content := k.raw[1 : len(k.raw)-1]
		if string(content) == "model" {
			return true, false
		}
		switch len(content) {
		case len("session_id"), len("conversation_id"), len("thread_id"):
			// ASCII-only content, so EqualFold is plain ASCII case folding,
			// which is what strings.ToLower does to it.
			return false, bytes.EqualFold(content, []byte("session_id")) ||
				bytes.EqualFold(content, []byte("conversation_id")) ||
				bytes.EqualFold(content, []byte("thread_id"))
		}
		return false, false
	}
	key := k.String()
	_, isID = jsonCandidates[strings.ToLower(key)]
	return key == "model", isID
}

// dropFrom removes an earlier entry with the same key: encoding/json keeps
// only the last value of a repeated key.
func (k *jsonKey) dropFrom(entries []jsonObjectEntry) []jsonObjectEntry {
	for index := range entries {
		if k.equals(entries[index].key) {
			return append(entries[:index], entries[index+1:]...)
		}
	}
	return entries
}

func unquoteJSONString(raw []byte) string {
	content := raw[1 : len(raw)-1]
	if bytes.IndexByte(content, '\\') < 0 && utf8.Valid(content) {
		return string(content)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}
