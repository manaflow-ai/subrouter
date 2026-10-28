package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Codex treats ServerOverloaded as terminal unless the streamed error carries
// HTTP retry advice. After output, the harness must own retry and tool state;
// the gateway cannot safely replay that request itself.
func codexCapacityClientRetryAdvice(body []byte) ([]byte, bool) {
	if !codexCapacityFailureJSON(body) {
		return body, false
	}
	var event map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if decoder.Decode(&event) != nil {
		return body, false
	}
	container := event
	if response, ok := event["response"].(map[string]any); ok {
		container = response
	}
	err, ok := container["error"].(map[string]any)
	if !ok {
		return body, false
	}
	headers, ok := err["headers"].(map[string]any)
	if !ok {
		headers = make(map[string]any)
		err["headers"] = headers
	}
	for key, value := range headers {
		if !strings.EqualFold(key, "Retry-After") {
			continue
		}
		if text, ok := value.(string); ok {
			valid := true
			for i := range text {
				if text[i] != '\t' && (text[i] < ' ' || text[i] >= 127) {
					valid = false
					break
				}
			}
			text = strings.TrimSpace(text)
			if _, parseErr := strconv.ParseUint(text, 10, 64); valid && parseErr == nil {
				return body, true
			}
			if _, parseErr := http.ParseTime(text); valid && parseErr == nil {
				return body, true
			}
		}
		delete(headers, key)
	}
	wait := codexRetryHintJSON(body)
	seconds := int64(wait / time.Second)
	if wait%time.Second > 0 {
		seconds++
	}
	seconds = max(int64(1), seconds)
	headers["Retry-After"] = strconv.FormatInt(seconds, 10)
	encoded, encodeErr := json.Marshal(event)
	if encodeErr != nil {
		return body, false
	}
	return encoded, true
}
