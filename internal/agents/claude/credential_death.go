package claude

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Credential death kinds. A dead credential never recovers on its own; it
// leaves routing until a re-login replaces it.
const (
	// CredentialDeathRevoked is Anthropic's 401 authentication_error
	// "OAuth access token has been revoked."
	CredentialDeathRevoked = "revoked"
	// CredentialDeathOrgOAuthDisallowed is Anthropic's 403 permission_error
	// "OAuth authentication is currently not allowed for this organization."
	CredentialDeathOrgOAuthDisallowed = "org_oauth_disallowed"
)

// credentialErrorMessageMax bounds a provider message carried in logs and the
// death ledger.
const credentialErrorMessageMax = 300

// ProbeError is a probe's 401 or 403 answer with the provider's error
// envelope. Its Error text is unchanged from the plain status error the probe
// returned before, so status-text classifiers keep working.
type ProbeError struct {
	StatusCode int
	Status     string
	ErrorType  string
	Message    string
	RequestID  string
}

func (e *ProbeError) Error() string {
	return "fable probe failed: " + e.Status
}

// NewProbeError reads the error envelope from an already decoded body.
func NewProbeError(statusCode int, status string, header http.Header, body []byte) *ProbeError {
	envelope := ParseErrorEnvelope(body)
	if envelope.RequestID == "" {
		envelope.RequestID = strings.TrimSpace(header.Get("request-id"))
	}
	return &ProbeError{
		StatusCode: statusCode, Status: status,
		ErrorType: envelope.Type, Message: envelope.Message, RequestID: envelope.RequestID,
	}
}

// ErrorEnvelope is the safe part of an Anthropic error body: the error
// type, a bounded single-line message, and the request id.
type ErrorEnvelope struct {
	Type      string
	Message   string
	RequestID string
}

// ParseErrorEnvelope reads {"type":"error","error":{"type","message"},
// "request_id"}. A body that is not that JSON yields an empty envelope; the
// raw body is never returned.
func ParseErrorEnvelope(body []byte) ErrorEnvelope {
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return ErrorEnvelope{}
	}
	message := strings.Join(strings.Fields(payload.Error.Message), " ")
	if len(message) > credentialErrorMessageMax {
		message = message[:credentialErrorMessageMax]
	}
	return ErrorEnvelope{
		Type:      boundedToken(payload.Error.Type),
		Message:   message,
		RequestID: boundedToken(payload.RequestID),
	}
}

func boundedToken(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 128 {
		value = value[:128]
	}
	return value
}

// ClassifyCredentialDeath names the death a Claude OAuth error answer
// proves, or "" when it proves none. Only these two exact provider answers
// count: any other 401 or 403 may be request-scoped or transient.
func ClassifyCredentialDeath(statusCode int, errorType, message string) string {
	lower := strings.ToLower(strings.Join(strings.Fields(message), " "))
	switch statusCode {
	case http.StatusUnauthorized:
		if strings.Contains(lower, "has been revoked") {
			return CredentialDeathRevoked
		}
	case http.StatusForbidden:
		if errorType == "permission_error" &&
			strings.HasPrefix(lower, "oauth authentication is currently not allowed for this org") {
			return CredentialDeathOrgOAuthDisallowed
		}
	}
	return ""
}
