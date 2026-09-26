package antigravity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// GeminiRequestToCloudCode adapts the stable, public Gemini generateContent
// request shape to the Cloud Code envelope consumed by Antigravity. The
// project and session values are supplied by the caller because they are
// account/session scoped and must never be guessed or shared across accounts.
// It deliberately preserves unknown Gemini fields inside request so new SDK
// fields are not silently discarded.
func GeminiRequestToCloudCode(body []byte, project, model, sessionID, requestID, trajectoryID string) ([]byte, error) {
	project = strings.TrimSpace(project)
	model = strings.TrimSpace(model)
	sessionID = strings.TrimSpace(sessionID)
	requestID = strings.TrimSpace(requestID)
	trajectoryID = strings.TrimSpace(trajectoryID)
	if project == "" || model == "" || sessionID == "" || requestID == "" {
		return nil, errors.New("Cloud Code envelope requires project, model, session, and request ID")
	}
	var original map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(body), &original); err != nil {
		return nil, fmt.Errorf("decode Gemini request: %w", err)
	}
	if original == nil {
		return nil, errors.New("Gemini request must be a JSON object")
	}
	labels := map[string]string{"model_enum": model, "used_claude": "false", "used_claude_conservative": "false"}
	if trajectoryID != "" {
		labels["trajectory_id"] = trajectoryID
	}
	request := make(map[string]any, len(original)+3)
	for key, value := range original {
		request[key] = value
	}
	request["sessionId"] = sessionID
	request["labels"] = labels
	wrapped := map[string]any{
		"project":     project,
		"requestId":   requestID,
		"request":     request,
		"model":       model,
		"userAgent":   "antigravity",
		"requestType": "agent",
	}
	return json.Marshal(wrapped)
}

// CloudCodeGenerationPath returns the Cloud Code operation path for a Gemini
// request. The caller appends the provider prefix when exposing this through
// the Subrouter proxy.
func CloudCodeGenerationPath(stream bool) string {
	if stream {
		return "/v1internal:streamGenerateContent?alt=sse"
	}
	return "/v1internal:generateContent"
}
