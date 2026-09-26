package transcript

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type Usage struct {
	Requests          int64 `json:"requests"`
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	ReasoningTokens   int64 `json:"reasoning_tokens"`
	TotalTokens       int64 `json:"total_tokens"`
	PayloadBytes      int64 `json:"payload_bytes"`
}

type UsageGroup struct {
	Key   string `json:"key"`
	Usage Usage  `json:"usage"`
}

type UsageBucket struct {
	Start string `json:"start"`
	Label string `json:"label"`
	Usage Usage  `json:"usage"`
}

type Analytics struct {
	Totals            Usage         `json:"totals"`
	ByUser            []UsageGroup  `json:"by_user"`
	ByAccount         []UsageGroup  `json:"by_account"`
	ByModel           []UsageGroup  `json:"by_model"`
	Timeline          []UsageBucket `json:"timeline"`
	MaxTimelineTokens int64         `json:"max_timeline_tokens"`
	MaxUserTokens     int64         `json:"max_user_tokens"`
	MaxAccountTokens  int64         `json:"max_account_tokens"`
}

type usageRecord struct {
	Timestamp time.Time
	User      string
	Account   string
	Model     string
	Usage     Usage
}

type RawEvent struct {
	Timestamp  string         `json:"timestamp"`
	Type       string         `json:"type"`
	Payload    map[string]any `json:"payload"`
	BodyText   string         `json:"body_text,omitempty"`
	BodyBase64 string         `json:"body_base64,omitempty"`
}

func Analyze(dir string) (Analytics, error) {
	records, err := readUsageRecords(dir)
	if err != nil {
		return Analytics{}, err
	}
	return buildAnalytics(records), nil
}

func ReadRawSession(dir, agentType, sessionID string) ([]RawEvent, error) {
	path := SessionPath(dir, agentType, sessionID)
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var events []RawEvent
	chunks := bodyChunkAccumulator{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, err
		}
		raw := RawEvent{
			Timestamp: event.Timestamp,
			Type:      event.Type,
			Payload:   clonePayload(event.Payload),
		}
		delete(raw.Payload, "body_base64")
		if body, ok := chunks.bodyFromEvent(event); ok {
			if utf8.Valid(body) {
				raw.BodyText = string(body)
			} else {
				raw.BodyBase64 = base64.StdEncoding.EncodeToString(body)
			}
		}
		events = append(events, raw)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func readUsageRecords(dir string) ([]usageRecord, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, nil
	}

	root := filepath.Join(dir, "by-agent")
	var records []usageRecord
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		fileRecords, err := usageRecordsFromFile(path)
		if err != nil {
			return err
		}
		records = append(records, fileRecords...)
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return records, err
}

func usageRecordsFromFile(path string) ([]usageRecord, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var records []usageRecord
	var user string
	var account string
	chunks := bodyChunkAccumulator{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, err
		}
		if nextUser, ok := eventUser(event.Payload); ok {
			user = nextUser
		}
		if nextAccount, ok := stringValue(event.Payload["account"]); ok {
			account = nextAccount
		}
		body, ok := chunks.bodyFromEvent(event)
		if !ok {
			continue
		}
		eventTime, _ := time.Parse(time.RFC3339Nano, event.Timestamp)
		for _, usage := range extractUsageRecords(body) {
			usage.Timestamp = eventTime
			if usage.User == "" {
				usage.User = user
			}
			if usage.Account == "" {
				usage.Account = account
			}
			if bytesValue, ok := numericValue(event.Payload["bytes"]); ok {
				usage.Usage.PayloadBytes = bytesValue
			}
			records = append(records, usage)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

func extractUsageRecords(body []byte) []usageRecord {
	var records []usageRecord
	for _, payload := range jsonPayloads(body) {
		record, ok := usageRecordFromPayload(payload)
		if ok {
			records = append(records, record)
		}
	}
	return records
}

type bodyChunkAccumulator struct {
	streams map[string]*bodyChunkStream
}

type bodyChunkStream struct {
	chunks map[int][]byte
	order  []int
}

func (a *bodyChunkAccumulator) bodyFromEvent(event Event) ([]byte, bool) {
	if encoded, ok := stringValue(event.Payload["body_base64"]); ok {
		body, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, false
		}
		if isBodyChunk(event) {
			a.addChunk(event, body)
			return nil, false
		}
		return body, true
	}
	if isBodyChunkedSummary(event) {
		streamID, ok := stringValue(event.Payload["stream_id"])
		if !ok {
			return nil, false
		}
		return a.take(streamID)
	}
	return nil, false
}

func (a *bodyChunkAccumulator) addChunk(event Event, body []byte) {
	streamID, ok := stringValue(event.Payload["stream_id"])
	if !ok {
		return
	}
	if a.streams == nil {
		a.streams = map[string]*bodyChunkStream{}
	}
	stream := a.streams[streamID]
	if stream == nil {
		stream = &bodyChunkStream{chunks: map[int][]byte{}}
		a.streams[streamID] = stream
	}
	index := len(stream.order)
	if value, ok := numericValue(event.Payload["chunk_index"]); ok {
		index = int(value)
	}
	if _, exists := stream.chunks[index]; !exists {
		stream.order = append(stream.order, index)
	}
	stream.chunks[index] = append([]byte(nil), body...)
}

func (a *bodyChunkAccumulator) take(streamID string) ([]byte, bool) {
	if a.streams == nil {
		return nil, false
	}
	stream := a.streams[streamID]
	if stream == nil {
		return nil, false
	}
	delete(a.streams, streamID)
	sort.Ints(stream.order)
	var body bytes.Buffer
	for _, index := range stream.order {
		body.Write(stream.chunks[index])
	}
	return body.Bytes(), true
}

func isBodyChunk(event Event) bool {
	return boolField(event.Payload, "body_chunk") || strings.HasSuffix(event.Type, "_chunk")
}

func isBodyChunkedSummary(event Event) bool {
	return boolField(event.Payload, "body_chunked")
}

func boolField(values map[string]any, key string) bool {
	value, _ := values[key].(bool)
	return value
}

func jsonPayloads(body []byte) []map[string]any {
	var payload map[string]any
	if json.Unmarshal(body, &payload) == nil {
		return []map[string]any{payload}
	}

	var payloads []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		line = strings.TrimPrefix(line, "data:")
		line = strings.TrimSpace(line)
		if line == "" || line == "[DONE]" {
			continue
		}
		var item map[string]any
		if json.Unmarshal([]byte(line), &item) == nil {
			payloads = append(payloads, item)
		}
	}
	return payloads
}

func usageRecordFromPayload(payload map[string]any) (usageRecord, bool) {
	var container map[string]any
	var model string
	if response, ok := mapValue(payload["response"]); ok {
		container = response
		model, _ = stringValue(response["model"])
	} else {
		container = payload
		model, _ = stringValue(payload["model"])
	}

	usageMap, ok := mapValue(container["usage"])
	if !ok {
		return usageRecord{}, false
	}
	usage := Usage{
		InputTokens:  numberField(usageMap, "input_tokens", "prompt_tokens"),
		OutputTokens: numberField(usageMap, "output_tokens", "completion_tokens"),
		TotalTokens:  numberField(usageMap, "total_tokens"),
	}
	if details, ok := mapValue(usageMap["input_tokens_details"]); ok {
		usage.CachedInputTokens = numberField(details, "cached_tokens")
	}
	if details, ok := mapValue(usageMap["output_tokens_details"]); ok {
		usage.ReasoningTokens = numberField(details, "reasoning_tokens")
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	if usage.TotalTokens == 0 && usage.InputTokens == 0 && usage.OutputTokens == 0 {
		return usageRecord{}, false
	}
	usage.Requests = 1
	return usageRecord{Model: model, Usage: usage}, true
}

func buildAnalytics(records []usageRecord) Analytics {
	analytics := Analytics{}
	byUser := map[string]Usage{}
	byAccount := map[string]Usage{}
	byModel := map[string]Usage{}

	var first, last time.Time
	for _, record := range records {
		analytics.Totals.add(record.Usage)
		byUser[orUnknown(record.User)] = addUsage(byUser[orUnknown(record.User)], record.Usage)
		byAccount[orUnknown(record.Account)] = addUsage(byAccount[orUnknown(record.Account)], record.Usage)
		byModel[orUnknown(record.Model)] = addUsage(byModel[orUnknown(record.Model)], record.Usage)
		if !record.Timestamp.IsZero() {
			if first.IsZero() || record.Timestamp.Before(first) {
				first = record.Timestamp
			}
			if last.IsZero() || record.Timestamp.After(last) {
				last = record.Timestamp
			}
		}
	}

	analytics.ByUser = sortedUsageGroups(byUser)
	analytics.ByAccount = sortedUsageGroups(byAccount)
	analytics.ByModel = sortedUsageGroups(byModel)
	analytics.Timeline = buildTimeline(records, first, last)
	for _, bucket := range analytics.Timeline {
		if bucket.Usage.TotalTokens > analytics.MaxTimelineTokens {
			analytics.MaxTimelineTokens = bucket.Usage.TotalTokens
		}
	}
	for _, group := range analytics.ByUser {
		if group.Usage.TotalTokens > analytics.MaxUserTokens {
			analytics.MaxUserTokens = group.Usage.TotalTokens
		}
	}
	for _, group := range analytics.ByAccount {
		if group.Usage.TotalTokens > analytics.MaxAccountTokens {
			analytics.MaxAccountTokens = group.Usage.TotalTokens
		}
	}
	return analytics
}

func buildTimeline(records []usageRecord, first, last time.Time) []UsageBucket {
	if first.IsZero() || last.IsZero() {
		return nil
	}
	size := bucketSize(last.Sub(first))
	start := first.Truncate(size)
	buckets := map[time.Time]Usage{}
	for _, record := range records {
		if record.Timestamp.IsZero() {
			continue
		}
		bucketStart := record.Timestamp.Truncate(size)
		buckets[bucketStart] = addUsage(buckets[bucketStart], record.Usage)
	}

	var out []UsageBucket
	for cursor := start; !cursor.After(last); cursor = cursor.Add(size) {
		usage := buckets[cursor]
		out = append(out, UsageBucket{
			Start: cursor.Format(time.RFC3339),
			Label: cursor.Format(bucketLabelFormat(size)),
			Usage: usage,
		})
	}
	return out
}

func bucketSize(span time.Duration) time.Duration {
	switch {
	case span <= 2*time.Hour:
		return time.Minute
	case span <= 48*time.Hour:
		return time.Hour
	default:
		return 24 * time.Hour
	}
}

func bucketLabelFormat(size time.Duration) string {
	if size < time.Hour {
		return "15:04"
	}
	if size < 24*time.Hour {
		return "Jan 02 15:04"
	}
	return "Jan 02"
}

func sortedUsageGroups(groups map[string]Usage) []UsageGroup {
	out := make([]UsageGroup, 0, len(groups))
	for key, usage := range groups {
		out = append(out, UsageGroup{Key: key, Usage: usage})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Usage.TotalTokens == out[j].Usage.TotalTokens {
			return out[i].Key < out[j].Key
		}
		return out[i].Usage.TotalTokens > out[j].Usage.TotalTokens
	})
	return out
}

func addUsage(left, right Usage) Usage {
	left.add(right)
	return left
}

func (u *Usage) add(other Usage) {
	u.Requests += other.Requests
	u.InputTokens += other.InputTokens
	u.CachedInputTokens += other.CachedInputTokens
	u.OutputTokens += other.OutputTokens
	u.ReasoningTokens += other.ReasoningTokens
	u.TotalTokens += other.TotalTokens
	u.PayloadBytes += other.PayloadBytes
}

func numberField(values map[string]any, names ...string) int64 {
	for _, name := range names {
		if value, ok := numericValue(values[name]); ok {
			return value
		}
	}
	return 0
}

func mapValue(value any) (map[string]any, bool) {
	out, ok := value.(map[string]any)
	return out, ok
}

func clonePayload(payload map[string]any) map[string]any {
	out := make(map[string]any, len(payload))
	for key, value := range payload {
		out[key] = value
	}
	return out
}

func orUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "(unknown)"
	}
	return value
}
