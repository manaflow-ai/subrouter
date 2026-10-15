package transcript

import (
	"bytes"
	"encoding/json"
)

// usageScanner reads token usage, the stop reason and the model out of a
// response as it streams: Anthropic SSE (message_start, message_delta),
// OpenAI Responses events (response.completed) over SSE or websocket, chat
// completions, and plain JSON bodies.
type usageScanner struct {
	carry      []byte
	input      int64
	inputExtra bool
	cacheRead  int64
	cacheWrite int64
	output     int64
	reasoning  int64
	cached     int64
	stop       string
	model      string
	done       bool
	anthropic  bool
}

type usageResult struct {
	input, cached, cacheWrite, output, reasoning int64
	stop, model                                  string
	done                                         bool
}

// usageCarryMax bounds a partial line kept between chunks.
const usageCarryMax = 4 << 20

func (u *usageScanner) feed(data []byte) {
	for len(data) > 0 {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			if len(u.carry)+len(data) <= usageCarryMax {
				u.carry = append(u.carry, data...)
			} else {
				u.carry = nil
			}
			return
		}
		line := data[:newline]
		if len(u.carry) > 0 {
			line = append(u.carry, line...)
			u.carry = nil
		}
		u.line(line)
		data = data[newline+1:]
	}
}

// feedMessage reads one whole JSON message (a websocket frame).
func (u *usageScanner) feedMessage(message []byte) {
	u.parse(message)
}

// endMessage reads what feed held back as one message.
func (u *usageScanner) endMessage() {
	if len(u.carry) > 0 {
		u.parse(u.carry)
		u.carry = nil
	}
}

func (u *usageScanner) line(line []byte) {
	line = bytes.TrimSpace(line)
	line = bytes.TrimPrefix(line, []byte("data:"))
	u.parse(bytes.TrimSpace(line))
}

var usageMarkers = [][]byte{[]byte(`"usage"`), []byte(`"stop_reason"`), []byte(`"finish_reason"`), []byte(`"model"`), []byte(`response.completed`)}

type usageFields struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	PromptTokens             int64 `json:"prompt_tokens"`
	CompletionTokens         int64 `json:"completion_tokens"`
	InputTokensDetails       struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (u *usageScanner) parse(message []byte) {
	if len(message) == 0 || message[0] != '{' {
		return
	}
	interesting := false
	for _, marker := range usageMarkers {
		if bytes.Contains(message, marker) {
			interesting = true
			break
		}
	}
	if !interesting {
		return
	}
	var event struct {
		Type       string       `json:"type"`
		Model      string       `json:"model"`
		StopReason string       `json:"stop_reason"`
		Usage      *usageFields `json:"usage"`
		Message    *struct {
			Model      string       `json:"model"`
			StopReason string       `json:"stop_reason"`
			Usage      *usageFields `json:"usage"`
		} `json:"message"`
		Delta *struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Response *struct {
			Model             string       `json:"model"`
			Status            string       `json:"status"`
			Usage             *usageFields `json:"usage"`
			IncompleteDetails *struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
		} `json:"response"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(message, &event) != nil {
		return
	}
	if event.Model != "" {
		u.model = event.Model
	}
	if event.StopReason != "" {
		u.stop = event.StopReason
	}
	u.add(event.Usage)
	if event.Message != nil {
		u.anthropic = true
		if event.Message.Model != "" {
			u.model = event.Message.Model
		}
		if event.Message.StopReason != "" {
			u.stop = event.Message.StopReason
		}
		u.add(event.Message.Usage)
	}
	if event.Delta != nil && event.Delta.StopReason != "" {
		u.stop = event.Delta.StopReason
	}
	if event.Type == "message_stop" {
		u.done = true
	}
	if event.Response != nil {
		if event.Response.Model != "" {
			u.model = event.Response.Model
		}
		u.add(event.Response.Usage)
		switch event.Type {
		case "response.completed", "response.incomplete", "response.failed":
			u.done = true
			u.stop = event.Response.Status
			if event.Response.IncompleteDetails != nil && event.Response.IncompleteDetails.Reason != "" {
				u.stop = event.Response.Status + ":" + event.Response.IncompleteDetails.Reason
			}
		}
	}
	for _, choice := range event.Choices {
		if choice.FinishReason != "" {
			u.stop = choice.FinishReason
		}
	}
}

// add keeps the largest value seen per field: streams repeat cumulative
// counts (Anthropic sends input at message_start and output at the end).
func (u *usageScanner) add(fields *usageFields) {
	if fields == nil {
		return
	}
	if fields.CacheReadInputTokens > 0 || fields.CacheCreationInputTokens > 0 {
		u.anthropic = true
	}
	u.input = max(u.input, fields.InputTokens, fields.PromptTokens)
	u.output = max(u.output, fields.OutputTokens, fields.CompletionTokens)
	u.cacheRead = max(u.cacheRead, fields.CacheReadInputTokens)
	u.cacheWrite = max(u.cacheWrite, fields.CacheCreationInputTokens)
	u.cached = max(u.cached, fields.InputTokensDetails.CachedTokens, fields.PromptTokensDetails.CachedTokens)
	u.reasoning = max(u.reasoning, fields.OutputTokensDetails.ReasoningTokens, fields.CompletionTokensDetails.ReasoningTokens)
}

func (u *usageScanner) result() usageResult {
	u.endMessage()
	result := usageResult{output: u.output, reasoning: u.reasoning, stop: u.stop, model: u.model, done: u.done}
	if u.anthropic {
		// Anthropic reports uncached input separately from cache reads and
		// writes; the row's input_tokens is the whole prompt.
		result.input = u.input + u.cacheRead + u.cacheWrite
		result.cached = u.cacheRead
		result.cacheWrite = u.cacheWrite
	} else {
		result.input = u.input
		result.cached = u.cached
	}
	return result
}
