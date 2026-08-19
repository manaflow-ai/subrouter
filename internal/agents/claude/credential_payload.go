package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// unreadableCredentialPhrase appears in every credential-decode error. A
// credential that will not parse cannot be refreshed without re-auth, so the
// proxy classifies this phrase as a terminal credential error rather than a
// transient one.
const unreadableCredentialPhrase = "unreadable credential"

// credentialPayloadPreviewLimit bounds how much of a payload the shape summary
// inspects. The categories below only need the first few bytes of the trailing
// region.
const credentialPayloadPreviewLimit = 64

// describeCredentialPayload summarizes a payload that failed to decode, in
// terms that identify how it is malformed without revealing any of it. The
// payload holds an access token and, when a write left an older value behind,
// the trailing bytes can be the tail of a previous token — so the summary
// reports lengths, the failing offset, and a category, never the bytes.
func describeCredentialPayload(body []byte, err error) string {
	fields := []string{fmt.Sprintf("bytes=%d", len(body))}
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) {
		return strings.Join(append(fields, "trailing=unknown"), " ")
	}
	offset := int(syntaxErr.Offset)
	if offset < 0 || offset > len(body) {
		return strings.Join(append(fields, "trailing=unknown"), " ")
	}
	// json.SyntaxError.Offset is one past the byte that failed, so the trailing
	// region starts one byte earlier.
	start := offset - 1
	if start < 0 {
		start = 0
	}
	trailing := body[start:]
	fields = append(fields,
		fmt.Sprintf("offset=%d", offset),
		fmt.Sprintf("trailing_bytes=%d", len(trailing)),
		"trailing_kind="+classifyTrailingBytes(trailing),
	)
	return strings.Join(fields, " ")
}

// classifyTrailingBytes names the shape of the bytes that follow a complete
// JSON value. The category is the diagnosis: a binary plist means the keychain
// handed back a wrapper, a json fragment means a shorter value was written over
// a longer one and left its tail behind, nul padding means a truncated write.
func classifyTrailingBytes(trailing []byte) string {
	preview := trailing
	if len(preview) > credentialPayloadPreviewLimit {
		preview = preview[:credentialPayloadPreviewLimit]
	}
	switch {
	case len(preview) == 0:
		return "empty"
	case hasPrefixBytes(preview, "bplist00"):
		return "binary-plist"
	case allBytesEqual(preview, 0x00):
		return "nul-padding"
	case allBytesFunc(preview, func(b byte) bool { return unicode.IsSpace(rune(b)) }):
		return "whitespace"
	case looksLikeJSONFragment(preview):
		return "json-fragment"
	case allBytesFunc(preview, func(b byte) bool { return b >= 0x20 && b < 0x7f }):
		return "text"
	default:
		return "binary"
	}
}

func looksLikeJSONFragment(preview []byte) bool {
	for _, b := range preview {
		switch b {
		case '"', '{', '}', '[', ']', ':', ',':
			return true
		}
	}
	return false
}

func hasPrefixBytes(b []byte, prefix string) bool {
	return len(b) >= len(prefix) && string(b[:len(prefix)]) == prefix
}

func allBytesEqual(b []byte, want byte) bool {
	return allBytesFunc(b, func(got byte) bool { return got == want })
}

func allBytesFunc(b []byte, ok func(byte) bool) bool {
	for _, got := range b {
		if !ok(got) {
			return false
		}
	}
	return true
}
