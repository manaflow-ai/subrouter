package proxy

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// bedrockUsage holds the token counts reported by a Bedrock/Anthropic response.
type bedrockUsage struct {
	InputTokens      int                  `json:"input_tokens"`
	OutputTokens     int                  `json:"output_tokens"`
	CacheReadTokens  int                  `json:"cache_read_input_tokens"`
	CacheWriteTokens int                  `json:"cache_creation_input_tokens"`
	CacheCreation    bedrockCacheCreation `json:"cache_creation"`
}

// bedrockCacheCreation splits cache writes by TTL, which price differently
// (5m = 1.25x input, 1h = 2x input).
type bedrockCacheCreation struct {
	Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
	Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
}

func (u bedrockUsage) empty() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheReadTokens == 0 && u.CacheWriteTokens == 0
}

// bedrockPricing is USD per million tokens. cacheWrite is the 5-minute rate
// (1.25x input); cacheWrite1h is the 1-hour rate (2x input).
type bedrockPricing struct {
	input        float64
	output       float64
	cacheRead    float64
	cacheWrite   float64
	cacheWrite1h float64
}

// bedrockPriceFor returns pricing for a Bedrock model id / inference profile.
// Cache read is 0.1x input and cache write (5m) is 1.25x input, per Anthropic
// list pricing. Unknown models price at 0 so cost is never overstated.
func bedrockPriceFor(model string) bedrockPricing {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "fable"):
		return bedrockPricing{input: 10, output: 50, cacheRead: 1, cacheWrite: 12.5, cacheWrite1h: 20}
	case strings.Contains(m, "opus"):
		return bedrockPricing{input: 5, output: 25, cacheRead: 0.5, cacheWrite: 6.25, cacheWrite1h: 10}
	case strings.Contains(m, "sonnet"):
		return bedrockPricing{input: 3, output: 15, cacheRead: 0.3, cacheWrite: 3.75, cacheWrite1h: 6}
	case strings.Contains(m, "haiku"):
		return bedrockPricing{input: 1, output: 5, cacheRead: 0.1, cacheWrite: 1.25, cacheWrite1h: 2}
	default:
		return bedrockPricing{}
	}
}

func (u bedrockUsage) costUSD(model string) float64 {
	p := bedrockPriceFor(model)
	// When the response reports the per-TTL split, price each bucket at its
	// own rate; otherwise everything wrote at the 5m rate.
	writeCost := float64(u.CacheWriteTokens) * p.cacheWrite
	if u.CacheCreation.Ephemeral5m+u.CacheCreation.Ephemeral1h > 0 {
		writeCost = float64(u.CacheCreation.Ephemeral5m)*p.cacheWrite +
			float64(u.CacheCreation.Ephemeral1h)*p.cacheWrite1h
	}
	return (float64(u.InputTokens)*p.input +
		float64(u.OutputTokens)*p.output +
		float64(u.CacheReadTokens)*p.cacheRead +
		writeCost) / 1_000_000
}

// bedrockCostRecord is one JSONL line in the Bedrock cost log.
type bedrockCostRecord struct {
	Timestamp  string       `json:"timestamp"`
	Model      string       `json:"model"`
	Region     string       `json:"region,omitempty"`
	Status     int          `json:"status"`
	Usage      bedrockUsage `json:"usage"`
	CostUSD    float64      `json:"cost_usd_estimate"`
	DurationMs int64        `json:"duration_ms"`
}

var bedrockCostMu sync.Mutex

func appendBedrockCostRecord(path string, record bedrockCostRecord) {
	if strings.TrimSpace(path) == "" {
		return
	}
	line, err := json.Marshal(record)
	if err != nil {
		return
	}
	bedrockCostMu.Lock()
	defer bedrockCostMu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// parseBedrockInvokeUsage extracts usage from a non-streaming /invoke JSON body.
func parseBedrockInvokeUsage(body []byte) (bedrockUsage, bool) {
	var payload struct {
		Usage bedrockUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return bedrockUsage{}, false
	}
	if payload.Usage.empty() {
		return bedrockUsage{}, false
	}
	return payload.Usage, true
}

// bedrockStreamUsageWriter is an io.Writer that parses the AWS event-stream
// emitted by invoke-with-response-stream, extracting token usage without
// altering the bytes forwarded to the client. All errors are swallowed so a
// malformed frame never disrupts the proxied stream.
type bedrockStreamUsageWriter struct {
	buf   []byte
	usage bedrockUsage
	got   bool
}

func newBedrockStreamUsageWriter() *bedrockStreamUsageWriter {
	return &bedrockStreamUsageWriter{}
}

func (p *bedrockStreamUsageWriter) Write(chunk []byte) (int, error) {
	p.buf = append(p.buf, chunk...)
	p.consumeFrames()
	return len(chunk), nil
}

func (p *bedrockStreamUsageWriter) consumeFrames() {
	for {
		if len(p.buf) < 12 {
			return
		}
		total := int(binary.BigEndian.Uint32(p.buf[0:4]))
		headersLen := int(binary.BigEndian.Uint32(p.buf[4:8]))
		if total < 16 || total > 64<<20 {
			// Not a sane frame; drop everything to avoid unbounded growth.
			p.buf = nil
			return
		}
		if len(p.buf) < total {
			return
		}
		payloadStart := 12 + headersLen
		payloadEnd := total - 4
		if payloadStart <= payloadEnd && payloadEnd <= len(p.buf) {
			p.parsePayload(p.buf[payloadStart:payloadEnd])
		}
		p.buf = p.buf[total:]
	}
}

func (p *bedrockStreamUsageWriter) parsePayload(payload []byte) {
	var wrapper struct {
		Bytes string `json:"bytes"`
	}
	if err := json.Unmarshal(payload, &wrapper); err != nil || wrapper.Bytes == "" {
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(wrapper.Bytes)
	if err != nil {
		return
	}
	var event struct {
		Type    string `json:"type"`
		Message struct {
			Usage bedrockUsage `json:"usage"`
		} `json:"message"`
		Usage bedrockUsage `json:"usage"`
	}
	if err := json.Unmarshal(decoded, &event); err != nil {
		return
	}
	switch event.Type {
	case "message_start":
		p.usage.InputTokens = event.Message.Usage.InputTokens
		p.usage.CacheReadTokens = event.Message.Usage.CacheReadTokens
		p.usage.CacheWriteTokens = event.Message.Usage.CacheWriteTokens
		// Without the TTL split, costUSD falls back to pricing every cache
		// write at the 5m rate and understates 1h writes by ~1.6x.
		p.usage.CacheCreation = event.Message.Usage.CacheCreation
		p.got = true
	case "message_delta":
		if event.Usage.OutputTokens > 0 {
			p.usage.OutputTokens = event.Usage.OutputTokens
			p.got = true
		}
	}
}

func (p *bedrockStreamUsageWriter) Usage() (bedrockUsage, bool) {
	return p.usage, p.got && !p.usage.empty()
}

// bedrockCostSummary aggregates the cost log for reporting.
type bedrockCostSummary struct {
	Requests         int                        `json:"requests"`
	TotalUSD         float64                    `json:"total_usd"`
	TodayUSD         float64                    `json:"today_usd"`
	Week7dUSD        float64                    `json:"week_7d_usd"`
	Month30dUSD      float64                    `json:"month_30d_usd"`
	InputTokens      int64                      `json:"input_tokens"`
	OutputTokens     int64                      `json:"output_tokens"`
	CacheReadTokens  int64                      `json:"cache_read_tokens"`
	CacheWriteTokens int64                      `json:"cache_write_tokens"`
	Throttled        int                        `json:"throttled"`
	LastThrottle     string                     `json:"last_throttle,omitempty"`
	ByModel          map[string]bedrockModelAgg `json:"by_model"`
}

type bedrockModelAgg struct {
	Requests     int     `json:"requests"`
	TotalUSD     float64 `json:"total_usd"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
}

func summarizeBedrockCost(path string) bedrockCostSummary {
	summary := bedrockCostSummary{ByModel: map[string]bedrockModelAgg{}}
	bedrockCostMu.Lock()
	body, err := os.ReadFile(path)
	bedrockCostMu.Unlock()
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
		var record bedrockCostRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		summary.Requests++
		summary.TotalUSD += record.CostUSD
		summary.InputTokens += int64(record.Usage.InputTokens)
		summary.OutputTokens += int64(record.Usage.OutputTokens)
		summary.CacheReadTokens += int64(record.Usage.CacheReadTokens)
		summary.CacheWriteTokens += int64(record.Usage.CacheWriteTokens)
		if record.Status == 429 {
			summary.Throttled++
			if record.Timestamp > summary.LastThrottle {
				summary.LastThrottle = record.Timestamp
			}
		}
		if ts, err := time.Parse(time.RFC3339, record.Timestamp); err == nil {
			if ts.After(startToday) {
				summary.TodayUSD += record.CostUSD
			}
			if ts.After(now.AddDate(0, 0, -7)) {
				summary.Week7dUSD += record.CostUSD
			}
			if ts.After(now.AddDate(0, 0, -30)) {
				summary.Month30dUSD += record.CostUSD
			}
		}
		model := bedrockModelShortName(record.Model)
		agg := summary.ByModel[model]
		agg.Requests++
		agg.TotalUSD += record.CostUSD
		agg.InputTokens += int64(record.Usage.InputTokens)
		agg.OutputTokens += int64(record.Usage.OutputTokens)
		summary.ByModel[model] = agg
	}
	return summary
}

func bedrockModelShortName(model string) string {
	m := model
	if idx := strings.LastIndex(m, "."); idx >= 0 {
		m = m[idx+1:]
	}
	return m
}

// sortedBedrockModels returns model keys sorted by descending spend for stable
// display.
func sortedBedrockModels(byModel map[string]bedrockModelAgg) []string {
	keys := make([]string, 0, len(byModel))
	for k := range byModel {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if byModel[keys[i]].TotalUSD != byModel[keys[j]].TotalUSD {
			return byModel[keys[i]].TotalUSD > byModel[keys[j]].TotalUSD
		}
		return keys[i] < keys[j]
	})
	return keys
}
