package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// azureCodexUsage holds the token counts an Azure Responses reply reports.
// Cached tokens are a subset of input tokens, so they are billed at the cached
// rate and subtracted from the uncached input below.
type azureCodexUsage struct {
	InputTokens      int `json:"input_tokens"`
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
	OutputTokens     int `json:"output_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens"`
}

func (u azureCodexUsage) empty() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && u.CachedTokens == 0
}

// azureCodexPricing is USD per million tokens.
type azureCodexPricing struct {
	input      float64
	cachedRead float64
	output     float64
}

// azureCodexPriceFor returns Azure list pricing for a deployment's underlying
// model. An unknown model prices at 0, the same rule the Bedrock log follows,
// so a model we have not priced can never overstate spend.
func azureCodexPriceFor(model string) azureCodexPricing {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "codex-mini"), strings.Contains(m, "-mini"):
		return azureCodexPricing{input: 0.25, cachedRead: 0.025, output: 2}
	case strings.Contains(m, "-nano"):
		return azureCodexPricing{input: 0.05, cachedRead: 0.005, output: 0.4}
	case strings.Contains(m, "codex"), strings.HasPrefix(m, "gpt-5"):
		return azureCodexPricing{input: 1.25, cachedRead: 0.125, output: 10}
	default:
		return azureCodexPricing{}
	}
}

// costUSD prices one request. Cached tokens are reported inside input_tokens,
// so charging both at the full rate would double-count the cheapest part of a
// Codex turn, which is most of it once the fallback is sticky.
func (u azureCodexUsage) costUSD(model string) float64 {
	price := azureCodexPriceFor(model)
	uncached := u.InputTokens - u.CachedTokens
	if uncached < 0 {
		uncached = 0
	}
	return (float64(uncached)*price.input +
		float64(u.CachedTokens)*price.cachedRead +
		float64(u.OutputTokens)*price.output) / 1_000_000
}

// azureCodexCostRecord is one JSONL line in the Azure cost log.
type azureCodexCostRecord struct {
	Timestamp  string          `json:"timestamp"`
	Endpoint   string          `json:"endpoint"`
	Model      string          `json:"model"`
	Deployment string          `json:"deployment"`
	Reason     string          `json:"reason"`
	Status     int             `json:"status"`
	Usage      azureCodexUsage `json:"usage"`
	CostUSD    float64         `json:"cost_usd_estimate"`
	DurationMs int64           `json:"duration_ms"`
}

var azureCodexCostMu sync.Mutex

func appendAzureCodexCostRecord(path string, record azureCodexCostRecord) {
	if strings.TrimSpace(path) == "" {
		return
	}
	line, err := json.Marshal(record)
	if err != nil {
		return
	}
	azureCodexCostMu.Lock()
	defer azureCodexCostMu.Unlock()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(line, '\n'))
}

// azureCodexUsageFromBody reads usage out of a Responses reply in either shape:
// a plain JSON response, or the final response object of an SSE stream.
func azureCodexUsageFromBody(body []byte) (azureCodexUsage, string, bool) {
	if usage, model, ok := azureCodexUsageFromJSON(body); ok {
		return usage, model, true
	}
	var usage azureCodexUsage
	var model string
	found := false
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64<<10), azureCodexUsageMaxLineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if lineUsage, lineModel, ok := azureCodexUsageFromJSON([]byte(payload)); ok {
			usage, model, found = lineUsage, lineModel, true
		}
	}
	return usage, model, found
}

// azureCodexUsageMaxLineBytes bounds one SSE line. A Responses stream carries
// whole assistant messages in single events, so the default scanner limit is
// too small, but an unbounded one would let a broken stream grow without end.
const azureCodexUsageMaxLineBytes = 8 << 20

func azureCodexUsageFromJSON(body []byte) (azureCodexUsage, string, bool) {
	var payload struct {
		Object   string `json:"object"`
		Model    string `json:"model"`
		Response *struct {
			Model string `json:"model"`
			Usage *struct {
				InputTokens        int `json:"input_tokens"`
				OutputTokens       int `json:"output_tokens"`
				InputTokensDetails struct {
					CachedTokens     int `json:"cached_tokens"`
					CacheWriteTokens int `json:"cache_write_tokens"`
				} `json:"input_tokens_details"`
				OutputTokensDetails struct {
					ReasoningTokens int `json:"reasoning_tokens"`
				} `json:"output_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
		Usage *struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails struct {
				CachedTokens     int `json:"cached_tokens"`
				CacheWriteTokens int `json:"cache_write_tokens"`
			} `json:"input_tokens_details"`
			OutputTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return azureCodexUsage{}, "", false
	}
	model := payload.Model
	usage := payload.Usage
	if payload.Response != nil {
		if payload.Response.Model != "" {
			model = payload.Response.Model
		}
		if payload.Response.Usage != nil {
			usage = payload.Response.Usage
		}
	}
	if usage == nil {
		return azureCodexUsage{}, "", false
	}
	result := azureCodexUsage{
		InputTokens:      usage.InputTokens,
		CachedTokens:     usage.InputTokensDetails.CachedTokens,
		CacheWriteTokens: usage.InputTokensDetails.CacheWriteTokens,
		OutputTokens:     usage.OutputTokens,
		ReasoningTokens:  usage.OutputTokensDetails.ReasoningTokens,
	}
	if result.empty() {
		return azureCodexUsage{}, "", false
	}
	return result, model, true
}

// azureCodexCostBody wraps an Azure response body so usage is recorded when the
// response finishes, whichever path streams it. It buffers only the tail
// because the usage block arrives in the final event, so a long stream costs a
// bounded amount of memory rather than a copy of the whole conversation.
type azureCodexCostBody struct {
	inner    io.ReadCloser
	tail     []byte
	record   azureCodexCostRecord
	path     string
	started  time.Time
	now      func() time.Time
	once     sync.Once
	maxBytes int
}

// azureCodexCostTailBytes is how much of the end of a response is kept for
// usage parsing. The final SSE event of a Responses stream carries the whole
// response object, including the input it echoes back, so this is generous.
const azureCodexCostTailBytes = 512 << 10

func newAzureCodexCostBody(inner io.ReadCloser, path string, record azureCodexCostRecord, now func() time.Time) io.ReadCloser {
	if strings.TrimSpace(path) == "" {
		return inner
	}
	if now == nil {
		now = time.Now
	}
	return &azureCodexCostBody{
		inner:    inner,
		record:   record,
		path:     path,
		started:  now(),
		now:      now,
		maxBytes: azureCodexCostTailBytes,
	}
}

func (b *azureCodexCostBody) Read(p []byte) (int, error) {
	n, err := b.inner.Read(p)
	if n > 0 {
		b.appendTail(p[:n])
	}
	if err == io.EOF {
		b.finish()
	}
	return n, err
}

func (b *azureCodexCostBody) appendTail(chunk []byte) {
	b.tail = append(b.tail, chunk...)
	if len(b.tail) > b.maxBytes {
		b.tail = b.tail[len(b.tail)-b.maxBytes:]
	}
}

func (b *azureCodexCostBody) Close() error {
	err := b.inner.Close()
	b.finish()
	return err
}

// finish records the request once, whether the reader hit EOF or the client
// hung up early. A dropped stream still cost money on the provider side.
func (b *azureCodexCostBody) finish() {
	b.once.Do(func() {
		record := b.record
		record.Timestamp = b.now().UTC().Format(time.RFC3339)
		record.DurationMs = b.now().Sub(b.started).Milliseconds()
		if usage, model, ok := azureCodexUsageFromBody(b.tail); ok {
			record.Usage = usage
			if model != "" {
				record.Deployment = model
			}
		}
		record.CostUSD = record.Usage.costUSD(record.Deployment)
		appendAzureCodexCostRecord(b.path, record)
	})
}

type azureCodexModelAgg struct {
	Requests     int     `json:"requests"`
	TotalUSD     float64 `json:"total_usd"`
	InputTokens  int64   `json:"input_tokens"`
	CachedTokens int64   `json:"cached_tokens"`
	OutputTokens int64   `json:"output_tokens"`
}

type azureCodexCostSummary struct {
	Requests     int                           `json:"requests"`
	TotalUSD     float64                       `json:"total_usd"`
	TodayUSD     float64                       `json:"today_usd"`
	Week7dUSD    float64                       `json:"week_7d_usd"`
	Month30dUSD  float64                       `json:"month_30d_usd"`
	InputTokens  int64                         `json:"input_tokens"`
	CachedTokens int64                         `json:"cached_tokens"`
	OutputTokens int64                         `json:"output_tokens"`
	ByModel      map[string]azureCodexModelAgg `json:"by_model"`
	ByReason     map[string]int                `json:"by_reason"`
}

func summarizeAzureCodexCost(path string) azureCodexCostSummary {
	summary := azureCodexCostSummary{
		ByModel:  map[string]azureCodexModelAgg{},
		ByReason: map[string]int{},
	}
	azureCodexCostMu.Lock()
	body, err := os.ReadFile(path)
	azureCodexCostMu.Unlock()
	if err != nil {
		return summary
	}
	now := time.Now()
	startToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var record azureCodexCostRecord
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		summary.Requests++
		summary.TotalUSD += record.CostUSD
		summary.InputTokens += int64(record.Usage.InputTokens)
		summary.CachedTokens += int64(record.Usage.CachedTokens)
		summary.OutputTokens += int64(record.Usage.OutputTokens)
		if record.Reason != "" {
			summary.ByReason[record.Reason]++
		}
		if timestamp, err := time.Parse(time.RFC3339, record.Timestamp); err == nil {
			if timestamp.After(startToday) {
				summary.TodayUSD += record.CostUSD
			}
			if timestamp.After(now.AddDate(0, 0, -7)) {
				summary.Week7dUSD += record.CostUSD
			}
			if timestamp.After(now.AddDate(0, 0, -30)) {
				summary.Month30dUSD += record.CostUSD
			}
		}
		model := record.Deployment
		if model == "" {
			model = record.Model
		}
		agg := summary.ByModel[model]
		agg.Requests++
		agg.TotalUSD += record.CostUSD
		agg.InputTokens += int64(record.Usage.InputTokens)
		agg.CachedTokens += int64(record.Usage.CachedTokens)
		agg.OutputTokens += int64(record.Usage.OutputTokens)
		summary.ByModel[model] = agg
	}
	return summary
}

func (s Server) handleAzureCodexCost(w http.ResponseWriter, _ *http.Request) {
	path := ""
	if s.AzureCodex != nil {
		path = s.AzureCodex.CostLogPath
	}
	writeJSON(w, summarizeAzureCodexCost(path))
}
