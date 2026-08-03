package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

type goldenAgentPayloadEvidence struct {
	NonceCount            int
	CompletionMarkerCount int
	NumberedLineCount     int
	NumberedLinesSHA256   string
}

func validateGoldenAgentMessagePayload(text, nonce, marker string, expectedLines int) (goldenAgentPayloadEvidence, error) {
	evidence := goldenAgentPayloadEvidence{
		NonceCount:            strings.Count(text, nonce),
		CompletionMarkerCount: strings.Count(text, marker),
	}
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	normalized = strings.TrimSuffix(normalized, "\n")
	lines := strings.Split(normalized, "\n")
	if expectedLines < 0 || nonce == "" || marker == "" ||
		evidence.NonceCount != 1 || evidence.CompletionMarkerCount != 1 ||
		len(lines) != expectedLines+2 || lines[0] != nonce || lines[len(lines)-1] != marker {
		return evidence, failGolden("agent_payload_invalid")
	}
	hasher := sha256.New()
	for index := 1; index <= expectedLines; index++ {
		expected := fmt.Sprintf("%d x", index)
		if lines[index] != expected {
			return evidence, failGolden("agent_payload_invalid")
		}
		_, _ = io.WriteString(hasher, expected)
		_, _ = io.WriteString(hasher, "\n")
		evidence.NumberedLineCount++
	}
	evidence.NumberedLinesSHA256 = hex.EncodeToString(hasher.Sum(nil))
	return evidence, nil
}

func observeGoldenAgentMessage(session *goldenSession, text string) {
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	normalized = strings.TrimSuffix(normalized, "\n")
	session.mu.Lock()
	defer session.mu.Unlock()
	session.payloadMessageCount++
	session.markerCount += strings.Count(text, session.marker)
	session.nonceCount += strings.Count(text, session.nonce)
	if normalized == "" || session.payloadInvalid {
		session.payloadInvalid = true
		return
	}
	for _, line := range strings.Split(normalized, "\n") {
		switch {
		case session.payloadNextLine == 0:
			if line != session.nonce {
				session.payloadInvalid = true
				return
			}
		case session.payloadNextLine <= session.payloadExpectedLines:
			expected := fmt.Sprintf("%d x", session.payloadNextLine)
			if line != expected {
				session.payloadInvalid = true
				return
			}
			session.payloadNumberedLines++
		case session.payloadNextLine == session.payloadExpectedLines+1:
			if line != session.marker {
				session.payloadInvalid = true
				return
			}
		default:
			session.payloadInvalid = true
			return
		}
		session.payloadNextLine++
	}
	if session.payloadNextLine == session.payloadExpectedLines+2 {
		hasher := sha256.New()
		for index := 1; index <= session.payloadExpectedLines; index++ {
			_, _ = fmt.Fprintf(hasher, "%d x\n", index)
		}
		session.payloadSHA256 = hex.EncodeToString(hasher.Sum(nil))
	}
}
