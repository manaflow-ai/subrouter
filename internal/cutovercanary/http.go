package cutovercanary

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/session"
)

type apiClient struct {
	base    *url.URL
	client  *http.Client
	token   string
	maxBody int64
}

func newAPIClient(config HTTPConfig, allowRemote bool) (*apiClient, error) {
	u, err := url.Parse(config.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid canary base URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("canary base URL must use HTTP or HTTPS")
	}
	if !allowRemote && !isLoopbackHost(u.Hostname()) {
		return nil, errors.New("live routed canary base URL must be loopback")
	}
	if allowRemote && config.AdminTokenFile != "" && u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return nil, errors.New("remote admin token requires HTTPS")
	}
	if config.TimeoutSeconds < 1 || config.TimeoutSeconds > 120 {
		return nil, errors.New("canary HTTP timeout must be 1..120 seconds")
	}
	if config.MaxResponseBytes < 256 || config.MaxResponseBytes > 4<<20 {
		return nil, errors.New("canary response cap must be 256 bytes..4 MiB")
	}
	var token string
	if config.AdminTokenFile != "" {
		b, err := readPrivateFile(config.AdminTokenFile, 16<<10)
		if err != nil {
			return nil, errors.New("cannot read canary admin token")
		}
		token = strings.TrimSpace(string(b))
		if token == "" || strings.ContainsAny(token, "\r\n") {
			return nil, errors.New("invalid canary admin token")
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &apiClient{
		base: u,
		client: &http.Client{
			Timeout:       time.Duration(config.TimeoutSeconds) * time.Second,
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		token: token, maxBody: config.MaxResponseBytes,
	}, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *apiClient) request(ctx context.Context, method, path string, body []byte, headers map[string]string) (int, []byte, error) {
	reference, err := url.Parse(path)
	if err != nil {
		return 0, nil, errors.New("invalid canary request path")
	}
	u := c.base.ResolveReference(reference)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if c.token != "" && strings.HasPrefix(path, "/_subrouter/") {
		req.Header.Set("X-Subrouter-Admin-Token", c.token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, errors.New("canary HTTP request failed")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
	if err != nil {
		return 0, nil, errors.New("canary HTTP response read failed")
	}
	if int64(len(b)) > c.maxBody {
		return 0, nil, errors.New("canary HTTP response exceeded cap")
	}
	return resp.StatusCode, b, nil
}

type healthResponse struct {
	OK bool `json:"ok"`
}

type readyResponse struct {
	OK       bool `json:"ok"`
	Draining bool `json:"draining"`
}

func (c *apiClient) probe(ctx context.Context) (PeerProbeResult, error) {
	result := PeerProbeResult{Schema: PeerProbeSchema}
	status, body, err := c.request(ctx, http.MethodGet, "/_subrouter/health", nil, nil)
	if err != nil || status != http.StatusOK {
		return result, errors.New("health probe failed")
	}
	var health healthResponse
	if err := json.Unmarshal(body, &health); err != nil || !health.OK {
		return result, errors.New("health proof invalid")
	}
	result.HealthOK = true
	status, body, err = c.request(ctx, http.MethodGet, "/_subrouter/ready", nil, nil)
	if err != nil || status != http.StatusOK {
		return result, errors.New("readiness probe failed")
	}
	var ready readyResponse
	if err := json.Unmarshal(body, &ready); err != nil || !ready.OK || ready.Draining {
		return result, errors.New("readiness proof invalid")
	}
	result.ReadyOK = true
	result.Draining = ready.Draining
	result.OK = true
	return result, nil
}

func (c *apiClient) sessions(ctx context.Context) ([]session.Assignment, error) {
	statuses, err := c.sessionStatuses(ctx)
	if err != nil {
		return nil, err
	}
	assignments := make([]session.Assignment, 0, len(statuses))
	for _, status := range statuses {
		assignments = append(assignments, status.Assignment)
	}
	return assignments, nil
}

type sessionStatus struct {
	session.Assignment
	Active *bool `json:"active"`
}

type cutoverChallengeRegistration struct {
	Schema       string `json:"schema"`
	AgentType    string `json:"agent_type"`
	SessionID    string `json:"session_id"`
	InputSHA256  string `json:"input_sha256"`
	MarkerSHA256 string `json:"marker_sha256"`
	NotBefore    string `json:"not_before"`
	ExpiresAt    string `json:"expires_at"`
}

func (c *apiClient) setCutoverChallenge(ctx context.Context, method string, registration cutoverChallengeRegistration) error {
	if method != http.MethodPost && method != http.MethodDelete {
		return errors.New("invalid cutover challenge operation")
	}
	body, err := json.Marshal(registration)
	if err != nil {
		return errors.New("cannot encode cutover challenge")
	}
	status, _, err := c.request(ctx, method, "/_subrouter/cutover-challenge", body, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return errors.New("cutover challenge registration rejected")
	}
	return nil
}

func (c *apiClient) sessionStatuses(ctx context.Context) ([]sessionStatus, error) {
	status, body, err := c.request(ctx, http.MethodGet, "/_subrouter/sessions", nil, nil)
	if err != nil || status != http.StatusOK {
		return nil, errors.New("session inventory unavailable")
	}
	var assignments []sessionStatus
	if err := json.Unmarshal(body, &assignments); err != nil {
		return nil, errors.New("session inventory invalid")
	}
	return assignments, nil
}

func (c *apiClient) deleteSession(ctx context.Context, agent, id string) error {
	path := "/_subrouter/sessions?agent_type=" + url.QueryEscape(agent) + "&session_id=" + url.QueryEscape(id)
	status, _, err := c.request(ctx, http.MethodDelete, path, nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusNotFound {
		return errors.New("session cleanup rejected")
	}
	return nil
}

func findSession(all []session.Assignment, agent, id string) (session.Assignment, bool) {
	for _, assignment := range all {
		if assignment.AgentType == agent && assignment.SessionID == id {
			return assignment, true
		}
	}
	return session.Assignment{}, false
}

func findSessionStatus(all []sessionStatus, agent, id string) (sessionStatus, bool) {
	for _, status := range all {
		if status.AgentType == agent && status.SessionID == id {
			return status, true
		}
	}
	return sessionStatus{}, false
}

func markerResponse(body []byte, marker string) bool {
	if len(body) == 0 || marker == "" {
		return false
	}
	var root map[string]any
	if json.Unmarshal(body, &root) == nil {
		status, _ := root["status"].(string)
		return status == "completed" && exactResponseOutput(root) == marker
	}
	completed := 0
	var deltas strings.Builder
	var finals strings.Builder
	for _, raw := range bytes.Split(body, []byte("\n")) {
		line := bytes.TrimSpace(raw)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		var event map[string]any
		if json.Unmarshal(data, &event) != nil {
			return false
		}
		typeName, _ := event["type"].(string)
		if typeName == "response.completed" || typeName == "response.done" {
			completed++
		}
		if typeName == "response.failed" || typeName == "response.incomplete" || typeName == "error" {
			return false
		}
		if typeName == "response.output_text.delta" {
			delta, ok := event["delta"].(string)
			if !ok {
				return false
			}
			deltas.WriteString(delta)
		}
		if typeName == "response.output_item.done" {
			if item, ok := event["item"].(map[string]any); ok {
				finals.WriteString(exactResponseOutput(item))
			}
		}
	}
	if completed != 1 {
		return false
	}
	if deltas.Len() == 0 && finals.Len() == 0 {
		return false
	}
	if deltas.Len() > 0 && deltas.String() != marker {
		return false
	}
	if finals.Len() > 0 && finals.String() != marker {
		return false
	}
	return true
}

func exactResponseOutput(value map[string]any) string {
	var out strings.Builder
	var visit func(any)
	visit = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			if kind, _ := typed["type"].(string); kind == "output_text" {
				if text, ok := typed["text"].(string); ok {
					out.WriteString(text)
				}
				return
			}
			for _, key := range []string{"output", "content"} {
				if child, ok := typed[key]; ok {
					visit(child)
				}
			}
		case []any:
			for _, child := range typed {
				visit(child)
			}
		}
	}
	visit(value)
	return out.String()
}

func hashProof(nonce, value string) string {
	sum := sha256.Sum256([]byte(nonce + "\x00" + value))
	return hex.EncodeToString(sum[:])
}

func requestMarkerHash(marker string) string {
	sum := sha256.Sum256([]byte(marker))
	return hex.EncodeToString(sum[:])
}

func responseRequest(model, marker string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"model":  model,
		"store":  false,
		"stream": true,
		"input": []map[string]any{{
			"type": "message",
			"role": "user",
			"content": []map[string]any{{
				"type": "input_text",
				"text": "Reply with exactly " + marker,
			}},
		}},
	})
}

func (c *apiClient) routedTurn(ctx context.Context, sessionID, model, marker string) (int, error) {
	status, response, err := c.liveTurn(ctx, sessionID, model, marker, "", true)
	if err != nil {
		return status, err
	}
	if status < 200 || status >= 300 || !markerResponse(response, marker) {
		return status, fmt.Errorf("routed canary response not proven")
	}
	return status, nil
}

func claudeRequest(model, marker string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"model": model, "max_tokens": 256,
		"system": []map[string]any{{
			"type": "text", "text": "You are Claude Code, Anthropic's official CLI for Claude.",
		}},
		"messages": []map[string]any{{"role": "user", "content": "Reply with exactly " + marker}},
	})
}

func exactClaudeMarkerResponse(body []byte, marker string) bool {
	var response struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if marker == "" || json.Unmarshal(body, &response) != nil || response.Type != "message" {
		return false
	}
	matched := false
	for _, content := range response.Content {
		switch content.Type {
		case "thinking", "redacted_thinking":
			// Claude's default reasoning mode can emit internal blocks before the
			// visible answer. They do not weaken the exact visible marker proof.
		case "text":
			if matched || content.Text != marker {
				return false
			}
			matched = true
		default:
			return false
		}
	}
	return matched
}

func (c *apiClient) claudeTurn(ctx context.Context, sessionID, model, marker string) (int, error) {
	body, err := claudeRequest(model, marker)
	if err != nil {
		return 0, err
	}
	headers := map[string]string{
		"Authorization":            "Bearer subrouter",
		"Content-Type":             "application/json",
		"anthropic-beta":           "claude-code-20250219,oauth-2025-04-20",
		"anthropic-version":        "2023-06-01",
		"User-Agent":               "claude-cli/2.1.199 (external, cli)",
		"x-app":                    "cli",
		"X-Subrouter-Agent":        "claude",
		"X-Claude-Code-Session-Id": sessionID,
		"X-Subrouter-No-Retry":     "true",
	}
	status, response, err := c.request(ctx, http.MethodPost, "/v1/messages", body, headers)
	if err != nil {
		return status, err
	}
	if status < 200 || status >= 300 || !exactClaudeMarkerResponse(response, marker) {
		return status, errors.New("routed Claude canary response not proven")
	}
	return status, nil
}

func (c *apiClient) liveTurn(ctx context.Context, sessionID, model, marker, forcedAccount string, noRetry bool) (int, []byte, error) {
	body, err := responseRequest(model, marker)
	if err != nil {
		return 0, nil, err
	}
	headers := map[string]string{
		"Authorization":       "Bearer subrouter",
		"Content-Type":        "application/json",
		"X-Subrouter-Agent":   "codex",
		"X-Subrouter-Session": sessionID,
	}
	if forcedAccount != "" {
		headers["X-Subrouter-Account-ID"] = forcedAccount
	}
	if noRetry {
		headers["X-Subrouter-No-Retry"] = "true"
	}
	status, response, err := c.request(ctx, http.MethodPost, "/v1/responses", body, headers)
	if err != nil {
		return status, nil, err
	}
	return status, response, nil
}

type liveUsageStatus struct {
	ID          string                 `json:"id"`
	Provider    accounts.Provider      `json:"provider"`
	AuthMode    accounts.AuthMode      `json:"auth_mode"`
	AuthChecked bool                   `json:"auth_checked"`
	AuthValid   bool                   `json:"auth_valid"`
	Windows     []accounts.UsageWindow `json:"windows"`
}

func (c *apiClient) unavailableCodexAccount(ctx context.Context, accountID string) (bool, error) {
	status, body, err := c.request(ctx, http.MethodGet, "/_subrouter/usage-status", nil, nil)
	if err != nil || status != http.StatusOK {
		return false, errors.New("live usage status unavailable")
	}
	var statuses []liveUsageStatus
	if err := json.Unmarshal(body, &statuses); err != nil {
		return false, errors.New("live usage status invalid")
	}
	for _, account := range statuses {
		if account.ID != accountID || account.Provider != accounts.ProviderCodex || account.AuthMode != accounts.AuthModeOAuth {
			continue
		}
		if !account.AuthChecked || !account.AuthValid {
			return false, errors.New("configured unavailable account credential is not valid")
		}
		for _, window := range account.Windows {
			if window.Feature == "" && window.UsedPercent >= 100 && window.LimitWindowSeconds > 0 {
				return true, nil
			}
		}
		return false, nil
	}
	return false, errors.New("configured unavailable account absent from usage status")
}

func quotaFailureResponse(status int, body []byte) bool {
	if status != http.StatusTooManyRequests && status != http.StatusForbidden {
		return false
	}
	var values []any
	var root any
	if json.Unmarshal(body, &root) == nil {
		values = append(values, root)
	} else {
		for _, raw := range bytes.Split(body, []byte("\n")) {
			line := bytes.TrimSpace(raw)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			var event any
			if json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))), &event) == nil {
				values = append(values, event)
			}
		}
	}
	for _, value := range values {
		if quotaFailureValue(value) {
			return true
		}
	}
	return false
}

func quotaFailureValue(value any) bool {
	event, ok := value.(map[string]any)
	if !ok {
		return false
	}
	eventType := strings.ToLower(strings.TrimSpace(jsonString(event["type"])))
	code, message, nestedFailure := quotaFailureFields(event)
	failureShaped := eventType == "error" || eventType == "response.failed" || quotaFailureCode(eventType) || code != "" || nestedFailure
	if !failureShaped {
		return false
	}
	if quotaFailureCode(eventType) || quotaFailureCode(code) {
		return true
	}
	lower := strings.ToLower(message)
	return strings.Contains(lower, "usage limit") &&
		(strings.Contains(lower, "reached") || strings.Contains(lower, "hit") || strings.Contains(lower, "exceeded"))
}

func quotaFailureFields(event map[string]any) (code, message string, nestedFailure bool) {
	code = strings.ToLower(strings.TrimSpace(jsonString(event["code"])))
	if code == "" {
		eventType := strings.ToLower(strings.TrimSpace(jsonString(event["type"])))
		if quotaFailureCode(eventType) {
			code = eventType
		}
	}
	message = strings.TrimSpace(jsonString(event["message"]))
	for _, key := range []string{"error", "response"} {
		switch nested := event[key].(type) {
		case map[string]any:
			nestedCode, nestedMessage, _ := quotaFailureFields(nested)
			if code == "" {
				code = nestedCode
			}
			if message == "" {
				message = nestedMessage
			}
			nestedFailure = true
		case string:
			if key == "error" {
				if message == "" {
					message = strings.TrimSpace(nested)
				}
				nestedFailure = true
			}
		}
	}
	return code, message, nestedFailure
}

func quotaFailureCode(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "usage_limit_reached", "insufficient_quota", "usage_not_included", "quota_exceeded", "rate_limit_exceeded":
		return true
	default:
		return false
	}
}

func jsonString(value any) string {
	text, _ := value.(string)
	return text
}
