package session

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/mail"
	"regexp"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
)

var headerCandidates = []string{
	"X-Subrouter-Session",
	"X-Codex-Window-ID",
	"X-Codex-Turn-State",
	"X-Codex-Parent-Thread-ID",
	"X-Session-ID",
	"X-Conversation-ID",
	"X-Codex-Session-ID",
	"X-Claude-Session-ID",
	"X-Claude-Code-Session-Id",
	"X-Gemini-Session-ID",
	"X-Gemini-Conversation-ID",
	"OpenAI-Conversation-ID",
	"Anthropic-Conversation-ID",
	"Google-Conversation-ID",
	// Codex sends bare session-id/thread-id on every responses request and on
	// the websocket upgrade. They come last so the existing window-scoped
	// headers keep priority, but they keep session identity working if the
	// x-codex-* compatibility headers ever go away.
	"Session-Id",
	"Thread-Id",
	"Idempotency-Key",
}

var userEmailHeaderCandidates = []string{
	"X-Subrouter-User-Email",
	"X-Subrouter-User",
	"X-User-Email",
}

var accountIDHeaderCandidates = []string{
	"X-Subrouter-Account-ID",
	"X-Subrouter-Account",
}

var modelHeaderCandidates = []string{
	"X-Subrouter-Model",
	"X-Model",
}

var codexAgentHeaderCandidates = []string{
	"X-Codex-Window-ID",
	"X-Codex-Turn-State",
	"X-Codex-Parent-Thread-ID",
	"X-Codex-Session-ID",
	"OpenAI-Conversation-ID",
}

var claudeAgentHeaderCandidates = []string{
	"X-Claude-Session-ID",
	"X-Claude-Code-Session-Id",
	"Anthropic-Conversation-ID",
}

var geminiAgentHeaderCandidates = []string{
	"X-Gemini-Session-ID",
	"X-Gemini-Conversation-ID",
	"Google-Conversation-ID",
}

var jsonCandidates = map[string]struct{}{
	"session_id":      {},
	"conversation_id": {},
	"thread_id":       {},
}

func ExtractAgentType(r *http.Request) string {
	if explicit := NormalizeAgentType(r.Header.Get("X-Subrouter-Agent")); explicit != "" {
		return explicit
	}
	if claudeCodeUserAgent(r.UserAgent()) {
		return "claude"
	}
	if hasAnyHeader(r, claudeAgentHeaderCandidates) {
		return "claude"
	}
	if hasAnyHeader(r, geminiAgentHeaderCandidates) {
		return "gemini"
	}
	if hasAnyHeader(r, codexAgentHeaderCandidates) {
		return "codex"
	}
	return "codex"
}

func claudeCodeUserAgent(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "claude-cli") || strings.Contains(lower, "claude-code")
}

func NormalizeAgentType(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" || len(normalized) > 64 {
		return ""
	}
	if !agentTypePattern.MatchString(normalized) {
		return ""
	}
	return normalized
}

func ExtractUserEmail(r *http.Request) string {
	for _, header := range userEmailHeaderCandidates {
		if email := NormalizeUserEmail(r.Header.Get(header)); email != "" {
			return email
		}
	}
	return ""
}

func ExtractAccountID(r *http.Request) string {
	for _, header := range accountIDHeaderCandidates {
		if accountID := NormalizeAccountID(r.Header.Get(header)); accountID != "" {
			return accountID
		}
	}
	return ""
}

func ExtractModel(r *http.Request, maxBodyBytes int64) string {
	for _, header := range modelHeaderCandidates {
		if value := NormalizeModel(r.Header.Get(header)); value != "" {
			return value
		}
	}
	if value := NormalizeModel(r.URL.Query().Get("model")); value != "" {
		return value
	}
	return extractJSONModel(r, maxBodyBytes)
}

func NormalizeModel(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || len(trimmed) > 256 {
		return ""
	}
	return trimmed
}

func NormalizeUserEmail(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || len(trimmed) > 320 {
		return ""
	}
	address, err := mail.ParseAddress(trimmed)
	if err != nil {
		return ""
	}
	return strings.ToLower(address.Address)
}

func NormalizeAccountID(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || len(trimmed) > 256 {
		return ""
	}
	return trimmed
}

func StripSubrouterHeaders(headers http.Header) {
	headers.Del("X-Subrouter-Lease")
	headers.Del("X-Subrouter-Session")
	headers.Del("X-Subrouter-Agent")
	headers.Del("X-Subrouter-User-Email")
	headers.Del("X-Subrouter-User")
	headers.Del("X-User-Email")
	headers.Del("X-Subrouter-Account-ID")
	headers.Del("X-Subrouter-Account")
	headers.Del("X-Subrouter-Model")
	headers.Del("X-Model")
	headers.Del("X-Subrouter-Azure")
}

func ExtractID(r *http.Request, maxBodyBytes int64) string {
	for _, header := range headerCandidates {
		if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
			return canonicalThreadID(value)
		}
	}

	for _, key := range []string{"session_id", "conversation_id", "thread_id"} {
		if value := strings.TrimSpace(r.URL.Query().Get(key)); value != "" {
			return value
		}
	}

	if id := extractJSONID(r, maxBodyBytes); id != "" {
		return id
	}

	return fallbackID(r)
}

func extractJSONID(r *http.Request, maxBodyBytes int64) string {
	value := extractJSONValue(r, maxBodyBytes)
	if value == nil {
		return ""
	}
	return findJSONID(value)
}

func extractJSONModel(r *http.Request, maxBodyBytes int64) string {
	value := extractJSONValue(r, maxBodyBytes)
	if value != nil {
		return findJSONModel(value)
	}
	return extractJSONModelScan(r, maxBodyBytes)
}

func extractJSONValue(r *http.Request, maxBodyBytes int64) any {
	if r.ContentLength < 0 || r.ContentLength > maxBodyBytes {
		return nil
	}
	body, truncated := readDecodedJSONBody(r, maxBodyBytes)
	if body == nil || truncated {
		return nil
	}

	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return nil
	}
	return value
}

// readDecodedJSONBody reads at most maxWireBytes of the request body, restores
// the body for the proxy, and returns it decoded per Content-Encoding. Codex
// sends `content-encoding: zstd` on every responses request, so without this
// step every body inspection here sees compressed bytes and silently finds
// neither the session id nor the model. truncated reports that the decoded body
// was cut short, so callers that need a complete JSON document give up while
// prefix scanners can still use what came back.
func readDecodedJSONBody(r *http.Request, maxWireBytes int64) (body []byte, truncated bool) {
	if r == nil || r.Body == nil || maxWireBytes <= 0 {
		return nil, false
	}
	if contentType := r.Header.Get("Content-Type"); !strings.Contains(contentType, "json") {
		return nil, false
	}
	wire, err := io.ReadAll(io.LimitReader(r.Body, maxWireBytes+1))
	if err != nil {
		return nil, false
	}
	// Anything past the read limit stays on the original body so the proxied
	// request is still byte-identical upstream.
	r.Body = struct {
		io.Reader
		io.Closer
	}{Reader: io.MultiReader(bytes.NewReader(wire), r.Body), Closer: r.Body}
	if int64(len(wire)) > maxWireBytes {
		return wire[:maxWireBytes], true
	}
	return decodeRequestBody(wire, r.Header.Get("Content-Encoding"))
}

func decodeRequestBody(wire []byte, contentEncoding string) (body []byte, truncated bool) {
	switch strings.ToLower(strings.TrimSpace(contentEncoding)) {
	case "", "identity":
		return wire, false
	case "zstd":
		decoder, err := zstd.NewReader(bytes.NewReader(wire), zstd.WithDecoderMaxMemory(uint64(decodedBodyMaxBytes)))
		if err != nil {
			return nil, false
		}
		defer decoder.Close()
		return readDecodedLimit(decoder)
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(wire))
		if err != nil {
			return nil, false
		}
		defer reader.Close()
		return readDecodedLimit(reader)
	default:
		return nil, false
	}
}

func readDecodedLimit(reader io.Reader) (body []byte, truncated bool) {
	decoded, err := io.ReadAll(io.LimitReader(reader, decodedBodyMaxBytes+1))
	if err != nil && int64(len(decoded)) <= decodedBodyMaxBytes {
		return nil, false
	}
	if int64(len(decoded)) > decodedBodyMaxBytes {
		return decoded[:decodedBodyMaxBytes], true
	}
	return decoded, false
}

// decodedBodyMaxBytes bounds how much decompressed request body is held in
// memory per inspection. A compressed body well under the wire limit can expand
// far past it, so the decoded size needs its own ceiling.
const decodedBodyMaxBytes = int64(16 << 20)

func findJSONID(value any) string {
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
			if id := findJSONID(child); id != "" {
				return id
			}
		}
	case []any:
		for _, child := range typed {
			if id := findJSONID(child); id != "" {
				return id
			}
		}
	}
	return ""
}

func findJSONModel(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		if str, ok := typed["model"].(string); ok {
			return NormalizeModel(str)
		}
		for _, child := range typed {
			if model := findJSONModel(child); model != "" {
				return model
			}
		}
	case []any:
		for _, child := range typed {
			if model := findJSONModel(child); model != "" {
				return model
			}
		}
	}
	return ""
}

var jsonModelFieldPattern = regexp.MustCompile(`"model"\s*:\s*"((?:\\.|[^"\\])*)"`)

const modelScanMaxBodyBytes = int64(8 << 20)

func extractJSONModelScan(r *http.Request, maxBodyBytes int64) string {
	if r.Body == nil || maxBodyBytes <= 0 {
		return ""
	}
	if contentType := r.Header.Get("Content-Type"); !strings.Contains(contentType, "json") {
		return ""
	}
	limit := maxBodyBytes
	if limit < modelScanMaxBodyBytes {
		limit = modelScanMaxBodyBytes
	}
	body, _ := readDecodedJSONBody(r, limit)
	if body == nil {
		return ""
	}
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

func fallbackID(r *http.Request) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(r.RemoteAddr))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(r.UserAgent()))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(r.Method))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(r.URL.Path))
	return "fallback:" + hex.EncodeToString(hash.Sum(nil))[:24]
}

func hasAnyHeader(r *http.Request, headers []string) bool {
	for _, header := range headers {
		if strings.TrimSpace(r.Header.Get(header)) != "" {
			return true
		}
	}
	return false
}

var agentTypePattern = regexp.MustCompile(`^[a-z0-9._-]+$`)

// canonicalThreadID drops the window index Codex appends to a thread id.
//
// Codex sends X-Codex-Window-ID as "<thread>:<window>", and the window number
// changes while the conversation does not. Treating those as different sessions
// splits one thread across identities: its account stickiness resets, and so
// does the provider it was pinned to, so a thread that had been served by Azure
// silently returns to OpenAI carrying reasoning OpenAI cannot decrypt. The
// identity has to follow the thread, not the window.
//
// Only a trailing numeric segment is dropped, and only when what precedes it is
// still a usable id, so ids that legitimately contain a colon are untouched.
func canonicalThreadID(value string) string {
	index := strings.LastIndexByte(value, ':')
	if index <= 0 || index == len(value)-1 {
		return value
	}
	suffix := value[index+1:]
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return value
		}
	}
	return value[:index]
}
