package proxy

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"strings"
	"sync"
)

// Observe each physical AWS response below both the raw gateway and the SSE
// transcoder. Only a clean EOF with complete usage can release a reservation.
// Cancellation, truncation, unknown frames, exceptions and non-200s retain it.
type bedrockBudgetBody struct {
	src                                 io.ReadCloser
	reservation                         *bedrockBudgetReservation
	stream, success                     bool
	mu                                  sync.Mutex
	buf                                 []byte
	invalid, closed, start, delta, stop bool
	input, output                       int64
}

func budgetResponseBody(src io.ReadCloser, r *bedrockBudgetReservation, stream, success bool) io.ReadCloser {
	if r == nil {
		return src
	}
	return &bedrockBudgetBody{src: src, reservation: r, stream: stream, success: success}
}
func (b *bedrockBudgetBody) Read(p []byte) (int, error) {
	n, err := b.src.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed && !b.invalid && b.success {
		if len(b.buf)+n > 32<<20 {
			b.invalid = true
			b.buf = nil
		} else {
			b.buf = append(b.buf, p[:n]...)
			if b.stream {
				b.frames()
			}
			if err == io.EOF {
				if !b.stream {
					b.message()
				}
				if !b.invalid && b.start && b.delta && b.stop && (!b.stream || len(b.buf) == 0) {
					b.reservation.settle(b.input, b.output)
				}
			}
		}
	}
	return n, err
}
func (b *bedrockBudgetBody) Close() error {
	// Interrupt a blocked read without waiting for it to finish.
	err := b.src.Close()
	b.mu.Lock()
	b.closed = true
	b.buf = nil
	b.mu.Unlock()
	return err
}
func budgetUsage(raw json.RawMessage, requireInput, requireOutput bool) (int64, int64, bool) {
	var u struct {
		Input      *int64 `json:"input_tokens"`
		Output     *int64 `json:"output_tokens"`
		CacheRead  int64  `json:"cache_read_input_tokens"`
		CacheWrite int64  `json:"cache_creation_input_tokens"`
	}
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &u) != nil || (requireInput && u.Input == nil) || (requireOutput && u.Output == nil) {
		return 0, 0, false
	}
	input, output := int64(0), int64(0)
	for _, v := range []*int64{u.Input, &u.CacheRead, &u.CacheWrite} {
		if v != nil {
			if *v < 0 || *v > 10000000 {
				return 0, 0, false
			}
			input += *v
		}
	}
	if u.Output != nil {
		output = *u.Output
		if output < 0 || output > 1000000 {
			return 0, 0, false
		}
	}
	return input, output, true
}
func (b *bedrockBudgetBody) message() {
	if rejectDuplicateJSON(b.buf) != nil {
		b.invalid = true
		return
	}
	var m struct {
		Usage      json.RawMessage `json:"usage"`
		StopReason string          `json:"stop_reason"`
	}
	if json.Unmarshal(b.buf, &m) != nil || m.StopReason == "" {
		b.invalid = true
		return
	}
	input, output, ok := budgetUsage(m.Usage, true, true)
	if !ok {
		b.invalid = true
		return
	}
	b.input = input
	b.output = output
	b.start = true
	b.delta = true
	b.stop = true
}
func (b *bedrockBudgetBody) frames() {
	for len(b.buf) >= 12 {
		size := int(binary.BigEndian.Uint32(b.buf[:4]))
		hlen := int(binary.BigEndian.Uint32(b.buf[4:8]))
		if size < 16 || size > 32<<20 || hlen > size-16 || crc32.ChecksumIEEE(b.buf[:8]) != binary.BigEndian.Uint32(b.buf[8:12]) {
			b.invalid = true
			b.buf = nil
			return
		}
		if len(b.buf) < size {
			return
		}
		frame := b.buf[:size]
		b.buf = b.buf[size:]
		if crc32.ChecksumIEEE(frame[:size-4]) != binary.BigEndian.Uint32(frame[size-4:]) {
			b.invalid = true
			return
		}
		headers, ok := parseBedrockEventHeaders(frame[12 : 12+hlen])
		if !ok || headers[":message-type"] != "event" || headers[":event-type"] != "chunk" {
			b.invalid = true
			return
		}
		var wrapper struct {
			Bytes string `json:"bytes"`
		}
		if json.Unmarshal(frame[12+hlen:size-4], &wrapper) != nil {
			b.invalid = true
			return
		}
		inner, err := base64.StdEncoding.DecodeString(wrapper.Bytes)
		if err != nil || rejectDuplicateJSON(inner) != nil {
			b.invalid = true
			return
		}
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Usage json.RawMessage `json:"usage"`
			} `json:"message"`
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal(inner, &ev) != nil || b.stop {
			b.invalid = true
			return
		}
		switch ev.Type {
		case "message_start":
			if b.start {
				b.invalid = true
				return
			}
			input, _, ok := budgetUsage(ev.Message.Usage, true, false)
			if !ok {
				b.invalid = true
				return
			}
			b.input = input
			b.start = true
		case "message_delta":
			if !b.start || b.delta {
				b.invalid = true
				return
			}
			_, output, ok := budgetUsage(ev.Usage, false, true)
			if !ok {
				b.invalid = true
				return
			}
			b.output = output
			b.delta = true
		case "message_stop":
			b.stop = b.start && b.delta
			if !b.stop {
				b.invalid = true
				return
			}
		case "ping", "content_block_start", "content_block_delta", "content_block_stop":
		default:
			b.invalid = true
			return
		}
	}
}
func bedrockBudgetStreamPath(path string) bool {
	return strings.HasSuffix(path, "/invoke-with-response-stream")
}
