package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
)

func codexOAuthRequiredPath(path string) bool {
	return chatGPTBackendPath(path) || codexLivePath(path)
}

// Codex 0.161 creates ChatGPT-auth WebRTC calls on the ChatGPT backend, but
// joins them using the same OAuth identity at api.openai.com/v1/live/<call-id>.
// These paths must retain /v1 when forwarded to the API origin.
func codexLivePath(path string) bool {
	if path == "/v1/live" {
		return true
	}
	id, ok := strings.CutPrefix(path, "/v1/live/")
	return ok && validRealtimeCallID(id)
}

func validRealtimeCallID(id string) bool {
	if len(id) == 0 || len(id) > 256 || id == "." || id == ".." {
		return false
	}
	for _, ch := range id {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
			return false
		}
	}
	return true
}

func realtimeCallID(location string) string {
	u, err := url.Parse(location)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.TrimRight(u.Path, "/"), "/")
	id := parts[len(parts)-1]
	if !validRealtimeCallID(id) {
		return ""
	}
	return id
}

const maxRealtimeCallBodyBytes = 8 << 20

// Only the finite SDP/session setup body is buffered. Audio and sideband
// events use the existing full-duplex websocket relay, never this decoder.
func rewriteRealtimeCallBody(request *http.Request) error {
	if request.Body == nil {
		return fmt.Errorf("missing request body")
	}
	mediaType, params, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return fmt.Errorf("expected multipart form data")
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxRealtimeCallBodyBytes+1))
	request.Body.Close()
	if err != nil {
		return fmt.Errorf("cannot read body")
	}
	if len(body) > maxRealtimeCallBodyBytes {
		return fmt.Errorf("body exceeds %d bytes", maxRealtimeCallBodyBytes)
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	fields := map[string][]byte{}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("malformed multipart body")
		}
		name := part.FormName()
		if name != "sdp" && name != "session" || fields[name] != nil {
			return fmt.Errorf("unexpected or duplicate form field")
		}
		fields[name], err = io.ReadAll(part)
		part.Close()
		if err != nil {
			return fmt.Errorf("malformed multipart field")
		}
	}
	var session map[string]json.RawMessage
	if len(fields["sdp"]) == 0 || json.Unmarshal(fields["session"], &session) != nil || session == nil {
		return fmt.Errorf("missing sdp or invalid session object")
	}
	body, err = json.Marshal(struct {
		SDP     string          `json:"sdp"`
		Session json.RawMessage `json:"session"`
	}{string(fields["sdp"]), fields["session"]})
	if err != nil {
		return fmt.Errorf("cannot encode realtime call")
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	request.GetBody = nil
	request.TransferEncoding = nil
	request.Header.Del("Content-Length")
	request.Header.Set("Content-Type", "application/json")
	return nil
}
