package session

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"unicode"

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
	accountID, _, err := ExtractAccountIDWithPresence(r)
	if err != nil {
		return ""
	}
	return accountID
}

// ExtractAccountIDWithPresence keeps header presence separate from account ID
// normalization. Callers that treat the account header as a hard routing pin
// must reject a present but invalid selector instead of silently entering the
// unpinned pool.
func ExtractAccountIDWithPresence(r *http.Request) (accountID string, present bool, err error) {
	if r == nil {
		return "", false, nil
	}
	for _, candidate := range accountIDHeaderCandidates {
		var values []string
		candidatePresent := false
		for name, headerValues := range r.Header {
			if strings.EqualFold(name, candidate) {
				candidatePresent = true
				values = append(values, headerValues...)
			}
		}
		if !candidatePresent {
			continue
		}
		present = true
		if len(values) == 0 {
			return "", true, fmt.Errorf("invalid %s account selector", candidate)
		}
		for _, value := range values {
			normalized := NormalizeAccountID(value)
			if normalized == "" {
				return "", true, fmt.Errorf("invalid %s account selector", candidate)
			}
			if accountID != "" && accountID != normalized {
				return "", true, fmt.Errorf("conflicting forced account selectors")
			}
			accountID = normalized
		}
	}
	return accountID, present, nil
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
	return inspectRequestBody(r, maxBodyBytes).model
}

var jsonServiceTierFieldPattern = regexp.MustCompile(`"service_tier"\s*:\s*"([A-Za-z0-9_.-]{0,64})"`)

// ExtractServiceTier returns the request body's top-level service_tier
// ("priority", "flex", ...) or "" when absent. It scans the decoded body
// rather than parsing it: the field is a short bare token, and requests can
// be megabytes of conversation.
func ExtractServiceTier(r *http.Request, maxBodyBytes int64) string {
	return inspectRequestBody(r, maxBodyBytes).serviceTier
}

// scanJSONServiceTier finds a service_tier field in a decoded (or raw
// uncompressed) body.
func scanJSONServiceTier(body []byte) string {
	match := jsonServiceTierFieldPattern.FindSubmatch(body)
	if len(match) != 2 {
		return ""
	}
	return strings.ToLower(string(match[1]))
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
	if len(value) > 256 {
		return ""
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return ""
		}
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
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
	headers.Del("X-Subrouter-Preferred-Account-ID")
	headers.Del("X-Subrouter-Model")
	headers.Del("X-Model")
	headers.Del("X-Subrouter-Azure")
	headers.Del("X-Subrouter-No-Retry")
	headers.Del("X-Subrouter-Capacity-Retry")
	headers.Del("X-Subrouter-Capacity-Retry-Budget")
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

	if id := inspectRequestBody(r, maxBodyBytes).id; id != "" {
		return id
	}

	return fallbackID(r)
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

var jsonModelFieldPattern = regexp.MustCompile(`"model"\s*:\s*"((?:\\.|[^"\\])*)"`)

const cutoverCanaryDecodedMaxBodyBytes = int64(16 << 20)

var errCutoverCanaryBodyLimit = errors.New("cutover canary body limit exceeded")

// ExtractResponsesCurrentUserInputHash parses only the Responses API input
// surface and returns the SHA-256 digest of its unique current-user text leaf.
// The parser streams a bounded decoded body; it never materializes the complete
// request or treats marker-shaped text in metadata, keys, or prior messages as
// the current turn.
func ExtractResponsesCurrentUserInputHash(r *http.Request, maxWireBytes int64) (string, bool) {
	if r == nil || r.Body == nil || maxWireBytes <= 0 || r.ContentLength < 0 || r.ContentLength > maxWireBytes {
		return "", false
	}
	if contentType := strings.ToLower(r.Header.Get("Content-Type")); !strings.Contains(contentType, "json") {
		return "", false
	}

	decoded, closeDecoded, err := cutoverCanaryDecodedReader(r.Body, r.Header.Get("Content-Encoding"), maxWireBytes)
	if err != nil {
		return "", false
	}
	defer closeDecoded()
	decoder := json.NewDecoder(&cutoverCanaryLimitReader{reader: decoded, remaining: cutoverCanaryDecodedMaxBodyBytes})
	digest, ok := responsesCurrentUserInputHash(decoder)
	if !ok {
		return "", false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return "", false
	}
	return hex.EncodeToString(digest[:]), true
}

func cutoverCanaryDecodedReader(body io.Reader, contentEncoding string, maxWireBytes int64) (io.Reader, func(), error) {
	wire := &cutoverCanaryLimitReader{reader: body, remaining: maxWireBytes}
	switch strings.ToLower(strings.TrimSpace(contentEncoding)) {
	case "", "identity":
		return wire, func() {}, nil
	case "gzip":
		reader, err := gzip.NewReader(wire)
		if err != nil {
			return nil, func() {}, err
		}
		return reader, func() { _ = reader.Close() }, nil
	case "zstd":
		reader, err := zstd.NewReader(wire, zstd.WithDecoderMaxMemory(uint64(cutoverCanaryDecodedMaxBodyBytes)))
		if err != nil {
			return nil, func() {}, err
		}
		return reader, reader.Close, nil
	default:
		return nil, func() {}, fmt.Errorf("unsupported content encoding")
	}
}

type cutoverCanaryLimitReader struct {
	reader    io.Reader
	remaining int64
}

func (r *cutoverCanaryLimitReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		var probe [1]byte
		n, err := r.reader.Read(probe[:])
		if n > 0 {
			return 0, errCutoverCanaryBodyLimit
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	return n, err
}

type responsesInputLeaf struct {
	digest [sha256.Size]byte
	count  int
}

func responsesCurrentUserInputHash(decoder *json.Decoder) ([sha256.Size]byte, bool) {
	if delim, ok := nextJSONDelimiter(decoder, '{'); !ok || delim != '{' {
		return [sha256.Size]byte{}, false
	}
	var current responsesInputLeaf
	seenInput := false
	for decoder.More() {
		key, ok := nextJSONString(decoder)
		if !ok {
			return [sha256.Size]byte{}, false
		}
		if key != "input" {
			if !skipJSONValue(decoder) {
				return [sha256.Size]byte{}, false
			}
			continue
		}
		if seenInput {
			return [sha256.Size]byte{}, false
		}
		seenInput = true
		current, ok = responsesInput(decoder)
		if !ok {
			return [sha256.Size]byte{}, false
		}
	}
	if delim, ok := nextJSONDelimiter(decoder, '}'); !ok || delim != '}' || !seenInput || current.count != 1 {
		return [sha256.Size]byte{}, false
	}
	return current.digest, true
}

func responsesInput(decoder *json.Decoder) (responsesInputLeaf, bool) {
	token, err := decoder.Token()
	if err != nil {
		return responsesInputLeaf{}, false
	}
	switch value := token.(type) {
	case string:
		return responsesInputLeaf{digest: sha256.Sum256([]byte(value)), count: 1}, true
	case json.Delim:
		if value != '[' {
			return responsesInputLeaf{}, false
		}
		current := responsesInputLeaf{}
		seenUser := false
		lastWasUser := false
		for decoder.More() {
			message, user, ok := responsesInputMessage(decoder)
			if !ok {
				return responsesInputLeaf{}, false
			}
			if user {
				current = message
				seenUser = true
			}
			lastWasUser = user
		}
		if delim, ok := nextJSONDelimiter(decoder, ']'); !ok || delim != ']' || !seenUser || !lastWasUser {
			return responsesInputLeaf{}, false
		}
		return current, true
	default:
		return responsesInputLeaf{}, false
	}
}

func responsesInputMessage(decoder *json.Decoder) (responsesInputLeaf, bool, bool) {
	if delim, ok := nextJSONDelimiter(decoder, '{'); !ok || delim != '{' {
		return responsesInputLeaf{}, false, false
	}
	role := ""
	typeName := ""
	content := responsesInputLeaf{}
	seenRole, seenType, seenContent := false, false, false
	for decoder.More() {
		key, ok := nextJSONString(decoder)
		if !ok {
			return responsesInputLeaf{}, false, false
		}
		switch key {
		case "role":
			if seenRole {
				return responsesInputLeaf{}, false, false
			}
			seenRole = true
			role, ok = nextJSONString(decoder)
		case "type":
			if seenType {
				return responsesInputLeaf{}, false, false
			}
			seenType = true
			typeName, ok = nextJSONString(decoder)
		case "content":
			if seenContent {
				return responsesInputLeaf{}, false, false
			}
			seenContent = true
			content, ok = responsesMessageContent(decoder)
		default:
			ok = skipJSONValue(decoder)
		}
		if !ok {
			return responsesInputLeaf{}, false, false
		}
	}
	if delim, ok := nextJSONDelimiter(decoder, '}'); !ok || delim != '}' {
		return responsesInputLeaf{}, false, false
	}
	user := role == "user" && (typeName == "" || typeName == "message")
	return content, user, true
}

func responsesMessageContent(decoder *json.Decoder) (responsesInputLeaf, bool) {
	token, err := decoder.Token()
	if err != nil {
		return responsesInputLeaf{}, false
	}
	if value, ok := token.(string); ok {
		return responsesInputLeaf{digest: sha256.Sum256([]byte(value)), count: 1}, true
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '[' {
		return responsesInputLeaf{}, false
	}
	content := responsesInputLeaf{}
	for decoder.More() {
		leaf, isText, ok := responsesContentPart(decoder)
		if !ok {
			return responsesInputLeaf{}, false
		}
		if isText {
			content.digest = leaf.digest
			content.count++
		}
	}
	if delim, ok := nextJSONDelimiter(decoder, ']'); !ok || delim != ']' {
		return responsesInputLeaf{}, false
	}
	return content, true
}

func responsesContentPart(decoder *json.Decoder) (responsesInputLeaf, bool, bool) {
	if delim, ok := nextJSONDelimiter(decoder, '{'); !ok || delim != '{' {
		return responsesInputLeaf{}, false, false
	}
	typeName := ""
	text := ""
	seenType, seenText := false, false
	for decoder.More() {
		key, ok := nextJSONString(decoder)
		if !ok {
			return responsesInputLeaf{}, false, false
		}
		switch key {
		case "type":
			if seenType {
				return responsesInputLeaf{}, false, false
			}
			seenType = true
			typeName, ok = nextJSONString(decoder)
		case "text":
			if seenText {
				return responsesInputLeaf{}, false, false
			}
			seenText = true
			text, ok = nextJSONString(decoder)
		default:
			ok = skipJSONValue(decoder)
		}
		if !ok {
			return responsesInputLeaf{}, false, false
		}
	}
	if delim, ok := nextJSONDelimiter(decoder, '}'); !ok || delim != '}' {
		return responsesInputLeaf{}, false, false
	}
	if typeName != "input_text" {
		return responsesInputLeaf{}, false, true
	}
	if !seenText {
		return responsesInputLeaf{}, false, false
	}
	return responsesInputLeaf{digest: sha256.Sum256([]byte(text))}, true, true
}

func nextJSONString(decoder *json.Decoder) (string, bool) {
	token, err := decoder.Token()
	value, ok := token.(string)
	return value, err == nil && ok
}

func nextJSONDelimiter(decoder *json.Decoder, want json.Delim) (json.Delim, bool) {
	token, err := decoder.Token()
	value, ok := token.(json.Delim)
	return value, err == nil && ok && value == want
}

func skipJSONValue(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return true
	}
	var close json.Delim
	switch delim {
	case '{':
		close = '}'
	case '[':
		close = ']'
	default:
		return false
	}
	for decoder.More() {
		if delim == '{' {
			if _, ok := nextJSONString(decoder); !ok {
				return false
			}
		}
		if !skipJSONValue(decoder) {
			return false
		}
	}
	got, ok := nextJSONDelimiter(decoder, close)
	return ok && got == close
}

const modelScanMaxBodyBytes = int64(8 << 20)

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
