package proxy

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/selectacct"
)

const (
	claudeOverageAccountHeader = "X-Subrouter-Overage-Account"
	claudeOverageFloorHeadroom = 0.01
)

type claudeOverageUsage struct {
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
}

func (u claudeOverageUsage) empty() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheCreationInputTokens == 0 && u.CacheReadInputTokens == 0
}

type claudeOverageCostRecord struct {
	Timestamp                string `json:"ts"`
	Account                  string `json:"account"`
	Model                    string `json:"model,omitempty"`
	Status                   int    `json:"status"`
	InputTokens              *int   `json:"input_tokens,omitempty"`
	OutputTokens             *int   `json:"output_tokens,omitempty"`
	CacheCreationInputTokens *int   `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     *int   `json:"cache_read_input_tokens,omitempty"`
}

var claudeOverageCostMu sync.Mutex

func NormalizeClaudeOverageAccounts(raw string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		id := normalizeClaudeOverageAccountID(part)
		if id == "" {
			continue
		}
		out[id] = true
	}
	return out
}

func normalizeClaudeOverageAccountID(accountID string) string {
	return strings.ToLower(strings.TrimSpace(accountID))
}

func (s Server) claudeOverageOptIn(accountID string) bool {
	if len(s.ClaudeOverageAccounts) == 0 {
		return false
	}
	return s.ClaudeOverageAccounts[normalizeClaudeOverageAccountID(accountID)]
}

func claudeOverageServed(status int, header http.Header) bool {
	return status >= 200 && status < 300 && claudeResponseRejected(header)
}

func stripClaudeOverageHeaders(header http.Header) {
	if header == nil {
		return
	}
	for key := range header {
		lower := strings.ToLower(key)
		if lower == "retry-after" || strings.HasPrefix(lower, "anthropic-ratelimit-unified-") {
			header.Del(key)
		}
	}
	header.Del(claudeOverageAccountHeader)
}

func (s Server) markClaudeOverageResponse(resp *http.Response, accountID string) {
	if resp == nil {
		return
	}
	stripClaudeOverageHeaders(resp.Header)
	if resp.Header == nil {
		resp.Header = http.Header{}
	}
	// This marker is consumed and removed by captureResponseBody before
	// ReverseProxy copies headers. Overage applies only to POST /v1/messages, so
	// the GET cache path should never see it; captureResponseBody still strips it
	// before cacheRecorder can store headers.
	resp.Header.Set(claudeOverageAccountHeader, accountID)
}

func applyClaudeOverageFloor(score selectacct.Score, windows []accounts.UsageWindow) selectacct.Score {
	if !claudeExtraUsageAvailable(windows) {
		return score
	}
	if score.Headroom < claudeOverageFloorHeadroom {
		score.Headroom = claudeOverageFloorHeadroom
	}
	if score.ShortHeadroom < claudeOverageFloorHeadroom {
		score.ShortHeadroom = claudeOverageFloorHeadroom
	}
	if len(score.ModelScores) > 0 {
		modelScores := make(map[string]selectacct.Score, len(score.ModelScores))
		for key, modelScore := range score.ModelScores {
			if modelScore.Headroom < claudeOverageFloorHeadroom {
				modelScore.Headroom = claudeOverageFloorHeadroom
			}
			if modelScore.ShortHeadroom < claudeOverageFloorHeadroom {
				modelScore.ShortHeadroom = claudeOverageFloorHeadroom
			}
			modelScores[key] = modelScore
		}
		score.ModelScores = modelScores
	}
	return score
}

func claudeExtraUsageAvailable(windows []accounts.UsageWindow) bool {
	for _, window := range windows {
		if strings.EqualFold(strings.TrimSpace(window.Name), "extra") && window.UsedPercent < 100 {
			return true
		}
	}
	return false
}

func (s Server) logClaudeOverageServed(accountID, model string, status int) {
	if s.Logger != nil {
		s.Logger.Info("claude overage served", "account", accountID, "model", model, "status", status)
	}
}

func (s Server) appendClaudeOverageCostRecord(accountID, model string, status int, usage claudeOverageUsage, usageOK bool) {
	if strings.TrimSpace(s.ClaudeOverageCostLogPath) == "" {
		return
	}
	record := claudeOverageCostRecord{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Account:   accountID,
		Model:     model,
		Status:    status,
	}
	if usageOK {
		record.InputTokens = intPtr(usage.InputTokens)
		record.OutputTokens = intPtr(usage.OutputTokens)
		record.CacheCreationInputTokens = intPtr(usage.CacheCreationInputTokens)
		record.CacheReadInputTokens = intPtr(usage.CacheReadInputTokens)
	}
	appendClaudeOverageCostRecord(s.ClaudeOverageCostLogPath, record)
}

func intPtr(v int) *int {
	return &v
}

func appendClaudeOverageCostRecord(path string, record claudeOverageCostRecord) {
	if strings.TrimSpace(path) == "" {
		return
	}
	line, err := json.Marshal(record)
	if err != nil {
		return
	}
	claudeOverageCostMu.Lock()
	defer claudeOverageCostMu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

func parseClaudeOverageUsage(body []byte) (model string, usage claudeOverageUsage, ok bool) {
	if len(body) == 0 {
		return "", claudeOverageUsage{}, false
	}
	if model, usage, ok = parseClaudeOverageJSONUsage(body); ok {
		return model, usage, true
	}
	return parseClaudeOverageSSEUsage(body)
}

func parseClaudeOverageJSONUsage(body []byte) (string, claudeOverageUsage, bool) {
	var payload struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", claudeOverageUsage{}, false
	}
	usage := claudeOverageUsage{
		InputTokens:              payload.Usage.InputTokens,
		OutputTokens:             payload.Usage.OutputTokens,
		CacheCreationInputTokens: payload.Usage.CacheCreationInputTokens,
		CacheReadInputTokens:     payload.Usage.CacheReadInputTokens,
	}
	if usage.empty() {
		return payload.Model, usage, false
	}
	return payload.Model, usage, true
}

func parseClaudeOverageSSEUsage(body []byte) (string, claudeOverageUsage, bool) {
	var model string
	var usage claudeOverageUsage
	var got bool
	for _, data := range sseDataPayloads(string(body)) {
		var event struct {
			Type    string `json:"type"`
			Message struct {
				Model string `json:"model"`
				Usage struct {
					InputTokens              int `json:"input_tokens"`
					OutputTokens             int `json:"output_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Usage struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		if event.Message.Model != "" {
			model = event.Message.Model
		}
		messageUsage := claudeOverageUsage{
			InputTokens:              event.Message.Usage.InputTokens,
			OutputTokens:             event.Message.Usage.OutputTokens,
			CacheCreationInputTokens: event.Message.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     event.Message.Usage.CacheReadInputTokens,
		}
		deltaUsage := claudeOverageUsage{
			InputTokens:              event.Usage.InputTokens,
			OutputTokens:             event.Usage.OutputTokens,
			CacheCreationInputTokens: event.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     event.Usage.CacheReadInputTokens,
		}
		if !messageUsage.empty() {
			mergeClaudeOverageUsage(&usage, messageUsage)
			got = true
		}
		if !deltaUsage.empty() {
			mergeClaudeOverageUsage(&usage, deltaUsage)
			got = true
		}
	}
	return model, usage, got
}

func sseDataPayloads(body string) []string {
	var payloads []string
	var data []string
	flush := func() {
		if len(data) == 0 {
			return
		}
		payloads = append(payloads, strings.Join(data, "\n"))
		data = nil
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if value != "[DONE]" {
				data = append(data, value)
			}
		}
	}
	flush()
	return payloads
}

func mergeClaudeOverageUsage(dst *claudeOverageUsage, src claudeOverageUsage) {
	if src.InputTokens != 0 {
		dst.InputTokens = src.InputTokens
	}
	if src.OutputTokens != 0 {
		dst.OutputTokens = src.OutputTokens
	}
	if src.CacheCreationInputTokens != 0 {
		dst.CacheCreationInputTokens = src.CacheCreationInputTokens
	}
	if src.CacheReadInputTokens != 0 {
		dst.CacheReadInputTokens = src.CacheReadInputTokens
	}
}
