package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"
)

const (
	// The observer is content-blind, so it only needs enough space for one
	// realistic wire frame and its held tail. Rejecting larger declarations
	// keeps a peer from turning an incomplete frame into an unbounded allocation.
	goldenWebSocketMaxFrameBytes    = 16 << 20
	goldenWebSocketMaxBufferedBytes = 32 << 20
)

// goldenWebSocketFrameParser reassembles complete wire frames without
// inspecting their payload. WebSocket frames are transport records, so the
// continuity pacer must never release a partial record to a client.
type goldenWebSocketFrameParser struct {
	pending []byte
}

func (p *goldenWebSocketFrameParser) append(payload []byte) ([][]byte, error) {
	if len(payload) > goldenWebSocketMaxBufferedBytes ||
		len(p.pending) > goldenWebSocketMaxBufferedBytes-len(payload) {
		return nil, errors.New("golden WebSocket frame buffer exceeds limit")
	}
	if len(payload) > 0 {
		p.pending = append(p.pending, payload...)
	}
	frames := make([][]byte, 0, 1)
	for len(p.pending) > 0 {
		frameLength, complete, err := goldenWebSocketFrameLength(p.pending)
		if err != nil {
			return nil, err
		}
		if !complete {
			break
		}
		frame := append([]byte(nil), p.pending[:frameLength]...)
		frames = append(frames, frame)
		p.pending = p.pending[frameLength:]
	}
	if len(p.pending) == 0 {
		p.pending = nil
	}
	return frames, nil
}

func goldenWebSocketFrameLength(payload []byte) (length int, complete bool, err error) {
	if len(payload) < 2 {
		return 0, false, nil
	}
	first := payload[0]
	opcode := first & 0x0f
	payloadLength := uint64(payload[1] & 0x7f)
	if opcode >= 0x8 {
		if first&0x80 == 0 {
			return 0, false, errors.New("golden WebSocket control frame is fragmented")
		}
		if opcode != 0x8 && opcode != 0x9 && opcode != 0xa {
			return 0, false, errors.New("golden WebSocket control opcode is reserved")
		}
		if payloadLength >= 126 {
			return 0, false, errors.New("golden WebSocket control frame is oversized")
		}
	}
	headerLength := 2
	switch payloadLength {
	case 126:
		if len(payload) < headerLength+2 {
			return 0, false, nil
		}
		payloadLength = uint64(binary.BigEndian.Uint16(payload[headerLength : headerLength+2]))
		headerLength += 2
	case 127:
		if len(payload) < headerLength+8 {
			return 0, false, nil
		}
		extended := binary.BigEndian.Uint64(payload[headerLength : headerLength+8])
		if extended&(uint64(1)<<63) != 0 {
			return 0, false, errors.New("golden WebSocket frame length overflows 63 bits")
		}
		payloadLength = extended
		headerLength += 8
	}
	if payload[1]&0x80 != 0 {
		headerLength += 4
	}
	maxInt := uint64(^uint(0) >> 1)
	if payloadLength > maxInt-uint64(headerLength) {
		return 0, false, errors.New("golden WebSocket frame length overflows int")
	}
	frameLength := headerLength + int(payloadLength)
	if frameLength > goldenWebSocketMaxFrameBytes {
		return 0, false, errors.New("golden WebSocket frame exceeds size limit")
	}
	if len(payload) < frameLength {
		return 0, false, nil
	}
	return frameLength, true, nil
}

// goldenWebSocketPacer applies the golden response gate to complete
// WebSocket frames. Writes only parse and enqueue; a single writer goroutine
// owns delivery, so upstream reads continue while data frames are paced.
type goldenWebSocketPacer struct {
	base          *goldenResponsePacer
	mu            sync.Mutex
	parser        goldenWebSocketFrameParser
	pending       [][]byte
	pendingBytes  int
	started       bool
	sink          func([]byte) (int, error)
	ctx           context.Context
	notify        chan struct{}
	controlWake   chan struct{}
	writerDone    chan struct{}
	writerStarted bool
	closing       bool
	writerErr     error
}

func newGoldenWebSocketPacer(base *goldenResponsePacer) *goldenWebSocketPacer {
	if base == nil {
		return nil
	}
	return &goldenWebSocketPacer{
		base:        base,
		notify:      make(chan struct{}, 1),
		controlWake: make(chan struct{}, 1),
		writerDone:  make(chan struct{}),
	}
}

func (p *goldenWebSocketPacer) releaseRequest() {
	if p != nil {
		p.base.releaseRequest()
		p.signal()
	}
}

func (p *goldenWebSocketPacer) hasPayload() bool {
	return p != nil && p.base.hasPayload()
}

func (p *goldenWebSocketPacer) write(
	ctx context.Context,
	payload []byte,
	write func([]byte) (int, error),
) (int, error) {
	if p == nil || len(payload) == 0 {
		return write(payload)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.base.payloadSeen.Store(true)
	p.mu.Lock()
	if p.base.wasSuperseded() {
		p.mu.Unlock()
		return len(payload), nil
	}
	if p.writerErr != nil {
		err := p.writerErr
		p.mu.Unlock()
		return 0, err
	}
	if p.closing {
		p.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	if p.sink == nil {
		p.sink = write
		p.ctx = ctx
	}
	bufferedBytes := p.pendingBytes + len(p.parser.pending)
	if bufferedBytes > goldenWebSocketMaxBufferedBytes ||
		len(payload) > goldenWebSocketMaxBufferedBytes-bufferedBytes {
		err := errors.New("golden WebSocket frame buffer exceeds limit")
		p.setWriterErrorLocked(err)
		p.mu.Unlock()
		return 0, err
	}
	frames, err := p.parser.append(payload)
	if err != nil {
		p.setWriterErrorLocked(err)
		p.mu.Unlock()
		return 0, err
	}
	controlPending := goldenWebSocketImmediateControlFramePrefix(p.parser.pending)
	for _, frame := range frames {
		if p.pendingBytes > goldenWebSocketMaxBufferedBytes-len(frame) {
			err := errors.New("golden WebSocket frame buffer exceeds limit")
			p.setWriterErrorLocked(err)
			p.mu.Unlock()
			return 0, err
		}
		p.pending = append(p.pending, frame)
		p.pendingBytes += len(frame)
		controlPending = controlPending || goldenWebSocketImmediateControlFrame(frame)
	}
	p.startWriterLocked()
	p.signalLocked()
	if controlPending {
		p.signalControlLocked()
	}
	p.mu.Unlock()
	return len(payload), nil
}

func (p *goldenWebSocketPacer) startWriterLocked() {
	if p.writerStarted {
		return
	}
	p.writerStarted = true
	go p.runWriter()
}

func (p *goldenWebSocketPacer) signal() {
	select {
	case p.notify <- struct{}{}:
	default:
	}
}

func (p *goldenWebSocketPacer) signalLocked() {
	p.signal()
}

func (p *goldenWebSocketPacer) signalControlLocked() {
	select {
	case p.controlWake <- struct{}{}:
	default:
	}
}

func goldenWebSocketImmediateControlFramePrefix(payload []byte) bool {
	if len(payload) < 2 {
		return false
	}
	opcode := payload[0] & 0x0f
	return payload[0]&0x80 != 0 &&
		(opcode == 0x9 || opcode == 0xa) && payload[1]&0x7f < 126
}

func goldenWebSocketImmediateControlFrame(frame []byte) bool {
	return goldenWebSocketImmediateControlFramePrefix(frame)
}

func (p *goldenWebSocketPacer) hasPendingImmediateControlFrameLocked() bool {
	for _, frame := range p.pending {
		if goldenWebSocketImmediateControlFrame(frame) {
			return true
		}
	}
	return false
}

func goldenWriteAll(write func([]byte) (int, error), payload []byte) (int, error) {
	total := 0
	for len(payload) > 0 {
		n, err := write(payload)
		if n < 0 || n > len(payload) {
			return total, errors.New("golden WebSocket writer returned an invalid byte count")
		}
		total += n
		payload = payload[n:]
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func (p *goldenWebSocketPacer) clearLocked() {
	p.parser.pending = nil
	p.pending = nil
	p.pendingBytes = 0
}

func (p *goldenWebSocketPacer) setWriterErrorLocked(err error) {
	if err == nil || p.writerErr != nil {
		return
	}
	p.writerErr = err
	p.closing = true
	p.base.releaseRequest()
	p.signalLocked()
}

func (p *goldenWebSocketPacer) waitForNotification(ctx context.Context) error {
	select {
	case <-p.notify:
		return nil
	case <-p.base.gateReleased:
		return nil
	case <-p.base.requestReleased:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *goldenWebSocketPacer) waitForInterval(ctx context.Context) (bool, error) {
	return p.base.delay.wait(ctx, p.base.gateReleased, p.base.requestReleased, p.controlWake, p.base.interval)
}

func (p *goldenWebSocketPacer) drainControlWakeLocked() {
	for {
		select {
		case <-p.controlWake:
		default:
			return
		}
	}
}

func (p *goldenWebSocketPacer) shouldFlushFrameLocked() bool {
	if p.base.wasSuperseded() || p.base.isReleased() {
		return true
	}
	// Ping and Pong frames must not wait behind the deployment gate. Flush any
	// preceding data frames too, so the wire order stays intact.
	if p.hasPendingImmediateControlFrameLocked() || goldenWebSocketImmediateControlFramePrefix(p.parser.pending) {
		return true
	}
	// Include an incomplete trailing frame in the held tail. If there is only
	// one complete frame and no trailing bytes, retain it until release so a
	// finite response cannot complete early.
	tailBytes := p.pendingBytes + len(p.parser.pending)
	if tailBytes <= p.base.holdbackBytes {
		return false
	}
	return len(p.pending) > 1 || len(p.parser.pending) > 0
}

func (p *goldenWebSocketPacer) frameNeedsDelayLocked(frame []byte) bool {
	return p.started && !p.base.isReleased() && !goldenWebSocketImmediateControlFrame(frame) &&
		!p.hasPendingImmediateControlFrameLocked() && !goldenWebSocketImmediateControlFramePrefix(p.parser.pending)
}

func (p *goldenWebSocketPacer) runWriter() {
	defer close(p.writerDone)
	for {
		p.mu.Lock()
		if p.writerErr != nil {
			p.mu.Unlock()
			return
		}
		if p.base.wasSuperseded() {
			p.clearLocked()
			p.mu.Unlock()
			return
		}
		if len(p.pending) == 0 {
			if p.closing {
				p.mu.Unlock()
				return
			}
			ctx := p.ctx
			p.mu.Unlock()
			if err := p.waitForNotification(ctx); err != nil {
				p.mu.Lock()
				p.setWriterErrorLocked(err)
				p.mu.Unlock()
				return
			}
			continue
		}
		if !p.shouldFlushFrameLocked() {
			ctx := p.ctx
			p.mu.Unlock()
			if err := p.waitForNotification(ctx); err != nil {
				p.mu.Lock()
				p.setWriterErrorLocked(err)
				p.mu.Unlock()
				return
			}
			continue
		}
		needDelay := p.frameNeedsDelayLocked(p.pending[0])
		if needDelay {
			p.drainControlWakeLocked()
		}
		ctx := p.ctx
		p.mu.Unlock()
		if needDelay {
			woken, err := p.waitForInterval(ctx)
			if err != nil {
				p.mu.Lock()
				p.setWriterErrorLocked(err)
				p.mu.Unlock()
				return
			}
			if woken {
				continue
			}
		}

		p.mu.Lock()
		if p.writerErr != nil {
			p.mu.Unlock()
			return
		}
		if p.base.wasSuperseded() {
			p.clearLocked()
			p.mu.Unlock()
			return
		}
		if len(p.pending) == 0 || !p.shouldFlushFrameLocked() {
			p.mu.Unlock()
			continue
		}
		frame := p.pending[0]
		if p.frameNeedsDelayLocked(frame) {
			p.mu.Unlock()
			continue
		}
		sink := p.sink
		p.mu.Unlock()
		if sink == nil {
			p.mu.Lock()
			p.setWriterErrorLocked(errors.New("golden WebSocket pacing sink is unavailable"))
			p.mu.Unlock()
			return
		}
		written, err := goldenWriteAll(sink, frame)

		p.mu.Lock()
		if written < 0 || written > len(frame) {
			p.setWriterErrorLocked(errors.New("golden WebSocket writer returned an invalid byte count"))
			p.mu.Unlock()
			return
		}
		if written > 0 {
			p.pendingBytes -= written
			if written == len(frame) {
				p.pending[0] = nil
				p.pending = p.pending[1:]
			} else {
				p.pending[0] = append([]byte(nil), frame[written:]...)
			}
		}
		if err != nil {
			p.setWriterErrorLocked(err)
			p.mu.Unlock()
			return
		}
		if written != len(frame) {
			p.setWriterErrorLocked(io.ErrShortWrite)
			p.mu.Unlock()
			return
		}
		p.started = true
		p.signalLocked()
		p.mu.Unlock()
	}
}

func (p *goldenWebSocketPacer) waitAndFlush() error {
	if p == nil {
		return nil
	}
	select {
	case <-p.base.gateReleased:
	case <-p.base.requestReleased:
	}
	p.mu.Lock()
	p.closing = true
	started := p.writerStarted
	done := p.writerDone
	p.signalLocked()
	p.mu.Unlock()
	if started {
		<-done
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.base.wasSuperseded() {
		p.clearLocked()
		return nil
	}
	if p.writerErr != nil {
		return p.writerErr
	}
	if len(p.parser.pending) != 0 {
		return errors.New("golden WebSocket stream ended with an incomplete frame")
	}
	if len(p.pending) != 0 {
		return errors.New("golden WebSocket writer stopped with pending frames")
	}
	return nil
}
