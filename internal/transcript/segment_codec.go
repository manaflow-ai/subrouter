package transcript

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// SegmentFormat names the sealed segment layout. A segment is a JSONL stream
// of Event lines, compressed with zstd, that starts with a segment_header
// event and (when the writer closed it cleanly) ends with a segment_footer.
//
// Bodies are stored as text, not base64, whenever they are valid UTF-8
// ("body_text"); anything else is "body_base64". A request body may instead
// carry "delta": the copy and insert operations that rebuild it from an
// earlier body of the same session in the same segment, named by its
// "body_seq". Every body carries the sha256 of its exact bytes, so a reader
// proves that reconstruction is lossless.
const SegmentFormat = "subrouter-transcript-segment/v1"

const (
	// deltaBlock is the block size the block matcher indexes the base with.
	deltaBlock = 32
	// deltaMinMatch is the shortest copy worth an operation; shorter runs
	// stay literal, where zstd compresses them anyway.
	deltaMinMatch = 64
	// deltaPrefixSuffixOnly is the changed-middle size below which the
	// encoder does not run the block matcher at all: a common prefix plus a
	// common suffix already describes the body.
	deltaPrefixSuffixOnly = 8 << 10
)

// deltaOp is one step of a delta: copy n bytes of the base from off, or
// insert lit.
type deltaOp struct {
	off int
	n   int
	lit []byte
}

func (op deltaOp) isCopy() bool { return op.lit == nil }

// encodeDelta describes target as copies from base plus literal bytes.
//
// Agent requests are mostly the previous request of the same conversation
// with a short tail appended, so the common prefix and common suffix carry
// almost all of it. Claude Code moves its cache_control marker forward each
// turn, which leaves one small edit near the end of the old messages; that is
// still one prefix, one short literal and one suffix. Only when the changed
// middle is large does the block matcher look for moved or reordered runs.
//
// Every literal starts and ends on a UTF-8 rune boundary of target, so a
// literal cut from a valid UTF-8 body is itself valid UTF-8 and is stored as
// text.
func encodeDelta(base, target []byte) []deltaOp {
	prefix := commonPrefixLen(base, target)
	for prefix > 0 && prefix < len(target) && !utf8.RuneStart(target[prefix]) {
		prefix--
	}
	suffix := commonSuffixLen(base[prefix:], target[prefix:])
	for suffix > 0 && !utf8.RuneStart(target[len(target)-suffix]) {
		suffix--
	}
	var ops []deltaOp
	if prefix > 0 {
		ops = append(ops, deltaOp{off: 0, n: prefix})
	}
	middle := target[prefix : len(target)-suffix]
	if len(middle) > deltaPrefixSuffixOnly && len(base) >= deltaMinMatch {
		ops = append(ops, blockMatch(base, middle)...)
	} else if len(middle) > 0 {
		ops = append(ops, deltaOp{lit: middle})
	}
	if suffix > 0 {
		ops = append(ops, deltaOp{off: len(base) - suffix, n: suffix})
	}
	return ops
}

// deltaSavings reports how many target bytes the copies cover.
func deltaSavings(ops []deltaOp) int {
	covered := 0
	for _, op := range ops {
		if op.isCopy() {
			covered += op.n
		}
	}
	return covered
}

func commonPrefixLen(a, b []byte) int {
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	n := 0
	const stride = 4096
	for n+stride <= limit && bytes.Equal(a[n:n+stride], b[n:n+stride]) {
		n += stride
	}
	for n < limit && a[n] == b[n] {
		n++
	}
	return n
}

func commonSuffixLen(a, b []byte) int {
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	n := 0
	const stride = 4096
	for n+stride <= limit && bytes.Equal(a[len(a)-n-stride:len(a)-n], b[len(b)-n-stride:len(b)-n]) {
		n += stride
	}
	for n < limit && a[len(a)-1-n] == b[len(b)-1-n] {
		n++
	}
	return n
}

const deltaHashMul = 0x100000001b3

// blockMatch finds runs of target that occur anywhere in base, using a
// rolling hash over deltaBlock-byte windows and an index of base blocks at
// deltaBlock strides. Offsets in the returned copies are absolute base
// offsets.
func blockMatch(base, target []byte) []deltaOp {
	if len(target) < deltaBlock {
		return []deltaOp{{lit: target}}
	}
	index := make(map[uint64]int32, len(base)/deltaBlock+1)
	// filter is a one-bit-per-bucket summary of index, so the per-byte scan
	// below skips most map lookups.
	var filter [1 << 14]uint64
	for start := 0; start+deltaBlock <= len(base); start += deltaBlock {
		h := hashBlock(base[start : start+deltaBlock])
		if _, ok := index[h]; !ok {
			index[h] = int32(start)
			bucket := h >> 44
			filter[bucket>>6] |= 1 << (bucket & 63)
		}
	}
	var pow uint64 = 1
	for i := 0; i < deltaBlock-1; i++ {
		pow *= deltaHashMul
	}

	var ops []deltaOp
	literalStart := 0
	i := 0
	h := hashBlock(target[:deltaBlock])
	for i+deltaBlock <= len(target) {
		bucket := h >> 44
		if filter[bucket>>6]&(1<<(bucket&63)) != 0 {
			if pos, ok := index[h]; ok {
				j := int(pos)
				if bytes.Equal(base[j:j+deltaBlock], target[i:i+deltaBlock]) {
					forward := deltaBlock + commonPrefixLen(base[j+deltaBlock:], target[i+deltaBlock:])
					back := 0
					for i-back > literalStart && j-back > 0 && base[j-back-1] == target[i-back-1] {
						back++
					}
					start, end := i-back, i+forward
					// Keep literals on rune boundaries.
					for start < end && !utf8.RuneStart(target[start]) {
						start++
					}
					for end > start && end < len(target) && !utf8.RuneStart(target[end]) {
						end--
					}
					if end-start >= deltaMinMatch {
						if start > literalStart {
							ops = append(ops, deltaOp{lit: target[literalStart:start]})
						}
						ops = append(ops, deltaOp{off: j - (i - start), n: end - start})
						literalStart = end
						i = end
						if i+deltaBlock <= len(target) {
							h = hashBlock(target[i : i+deltaBlock])
						}
						continue
					}
				}
			}
		}
		if i+deltaBlock >= len(target) {
			break
		}
		h = (h-uint64(target[i])*pow)*deltaHashMul + uint64(target[i+deltaBlock])
		i++
	}
	if literalStart < len(target) {
		ops = append(ops, deltaOp{lit: target[literalStart:]})
	}
	return ops
}

func hashBlock(block []byte) uint64 {
	var h uint64
	for _, b := range block {
		h = h*deltaHashMul + uint64(b)
	}
	return h
}

// marshalDeltaOps renders ops as a JSON array: [off,len] for a copy, a string
// for a UTF-8 literal, and {"b": base64} for any other literal.
func marshalDeltaOps(ops []deltaOp) []any {
	out := make([]any, 0, len(ops))
	for _, op := range ops {
		switch {
		case op.isCopy():
			out = append(out, [2]int{op.off, op.n})
		case utf8.Valid(op.lit):
			out = append(out, string(op.lit))
		default:
			out = append(out, map[string]string{"b": base64.StdEncoding.EncodeToString(op.lit)})
		}
	}
	return out
}

var errDeltaBaseMissing = errors.New("delta base is not in this segment")

// applyDelta rebuilds a body from its base and the decoded ops value.
func applyDelta(base []byte, opsValue any) ([]byte, error) {
	ops, ok := opsValue.([]any)
	if !ok {
		return nil, fmt.Errorf("delta ops are %T, not an array", opsValue)
	}
	var out bytes.Buffer
	for _, raw := range ops {
		switch op := raw.(type) {
		case string:
			out.WriteString(op)
		case map[string]any:
			encoded, _ := op["b"].(string)
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return nil, fmt.Errorf("delta literal: %w", err)
			}
			out.Write(decoded)
		case []any:
			if len(op) != 2 {
				return nil, fmt.Errorf("delta copy has %d fields", len(op))
			}
			off, okOff := jsonInt(op[0])
			n, okN := jsonInt(op[1])
			if !okOff || !okN || off < 0 || n < 0 || off+n > len(base) {
				return nil, fmt.Errorf("delta copy [%v,%v] outside a %d-byte base", op[0], op[1], len(base))
			}
			out.Write(base[off : off+n])
		default:
			return nil, fmt.Errorf("delta op is %T", raw)
		}
	}
	return out.Bytes(), nil
}

func jsonInt(value any) (int, bool) {
	switch v := value.(type) {
	case float64:
		return int(v), float64(int(v)) == v
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	case int:
		return v, true
	case int64:
		return int(v), true
	default:
		return 0, false
	}
}

// putBody stores body under body_text when it is valid UTF-8 and under
// body_base64 otherwise.
func putBody(payload map[string]any, body []byte) {
	if utf8.Valid(body) {
		payload["body_text"] = string(body)
		return
	}
	payload["body_base64"] = base64.StdEncoding.EncodeToString(body)
}

// payloadBody returns the bytes a body_text or body_base64 field holds.
func payloadBody(payload map[string]any) ([]byte, bool, error) {
	if text, ok := payload["body_text"].(string); ok {
		return []byte(text), true, nil
	}
	if encoded, ok := payload["body_base64"].(string); ok {
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		return decoded, true, err
	}
	return nil, false, nil
}

// deltaKeyFor names the chain of bodies a delta may refer back to: the same
// agent session, event type and direction. Writer and reader derive it from
// the same payload fields, so the reader keeps exactly the bases the writer
// may cite.
func deltaKeyFor(eventType string, payload map[string]any) string {
	agent, _ := payload["agent_type"].(string)
	session, _ := payload["session_id"].(string)
	direction, _ := payload["direction"].(string)
	return jsonSafe(agent) + "\x00" + jsonSafe(session) + "\x00" + jsonSafe(eventType) + "\x00" + jsonSafe(direction)
}

// jsonSafe maps each invalid UTF-8 byte to U+FFFD, as encoding/json does, so
// the writer's key matches the key the reader derives from decoded JSON.
func jsonSafe(value string) string {
	if utf8.ValidString(value) {
		return value
	}
	var builder strings.Builder
	for i := 0; i < len(value); {
		r, size := utf8.DecodeRuneInString(value[i:])
		if r == utf8.RuneError && size == 1 {
			builder.WriteString("\uFFFD")
		} else {
			builder.WriteString(value[i : i+size])
		}
		i += size
	}
	return builder.String()
}

// deltaBasesPerKey bounds how many earlier bodies of one chain a delta may
// cite. Claude Code runs subagents under the parent's session id, so one
// session interleaves several conversations; keeping a few bases lets each
// request find its own predecessor.
const deltaBasesPerKey = 8
