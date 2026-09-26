package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type goldenWebSocketWrites struct {
	mu     sync.Mutex
	writes [][]byte
	notify chan struct{}
}

func newGoldenWebSocketWrites() *goldenWebSocketWrites {
	return &goldenWebSocketWrites{notify: make(chan struct{}, 1)}
}

func (w *goldenWebSocketWrites) write(payload []byte) (int, error) {
	w.mu.Lock()
	w.writes = append(w.writes, append([]byte(nil), payload...))
	w.mu.Unlock()
	select {
	case w.notify <- struct{}{}:
	default:
	}
	return len(payload), nil
}

func (w *goldenWebSocketWrites) snapshot() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([][]byte(nil), w.writes...)
}

func (w *goldenWebSocketWrites) waitForCount(t *testing.T, count int) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		if len(w.snapshot()) >= count {
			return
		}
		select {
		case <-w.notify:
		case <-deadline.C:
			t.Fatalf("timed out waiting for %d WebSocket writes, got %d", count, len(w.snapshot()))
		}
	}
}

func TestGoldenWebSocketPacerKeepsFramesIntactWhileGateIsHeld(t *testing.T) {
	gate := newGoldenResponseGate()
	base := gate.newResponsePacer("0123456789abcdef0123456789abcdef")
	delay := &accumulatingObserverDelay{}
	base.delay = delay
	pacer := newGoldenWebSocketPacer(base)
	if pacer == nil {
		t.Fatal("newGoldenWebSocketPacer returned nil")
	}

	writes := newGoldenWebSocketWrites()
	frames := make([][]byte, 0, 128)
	for index := 0; index < 128; index++ {
		frame := []byte{0x81, 0x01, byte('a' + index%26)}
		frames = append(frames, frame)
	}
	stream := bytes.Join(frames, nil)
	for offset := 0; offset < len(stream); {
		end := offset + 7
		if end > len(stream) {
			end = len(stream)
		}
		if n, err := pacer.write(context.Background(), stream[offset:end], writes.write); err != nil {
			t.Fatalf("write: %v", err)
		} else if n != end-offset {
			t.Fatalf("write count = %d, want %d", n, end-offset)
		}
		offset = end
	}

	writes.waitForCount(t, 1)
	if delay.elapsedDuration() == 0 {
		t.Fatal("pacer did not apply an interval between data frames")
	}
	for index, write := range writes.snapshot() {
		if len(write)%3 != 0 {
			t.Fatalf("write %d split a WebSocket frame: %d bytes", index, len(write))
		}
		for offset := 0; offset < len(write); offset += 3 {
			if write[offset] != 0x81 || write[offset+1] != 0x01 {
				t.Fatalf("write %d contains malformed frame at byte %d: %x", index, offset, write[offset:offset+3])
			}
		}
	}

	gate.releasePacing()
	if err := pacer.waitAndFlush(); err != nil {
		t.Fatalf("waitAndFlush: %v", err)
	}
	var delivered bytes.Buffer
	for _, write := range writes.snapshot() {
		_, _ = delivered.Write(write)
	}
	if !bytes.Equal(delivered.Bytes(), stream) {
		t.Fatalf("delivered stream differs from input: got %d bytes, want %d", delivered.Len(), len(stream))
	}
}

func TestGoldenWebSocketFrameParserSupportsFragmentedExtendedFrames(t *testing.T) {
	parser := goldenWebSocketFrameParser{}
	payload := bytes.Repeat([]byte{'x'}, 126)
	frame := append([]byte{0x82, 126, 0, 126}, payload...)
	frame = append(frame, 0x88, 0x00)

	var frames [][]byte
	for _, chunk := range [][]byte{frame[:1], frame[1:3], frame[3:17], frame[17:]} {
		parsed, err := parser.append(chunk)
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		frames = append(frames, parsed...)
	}
	if len(frames) != 2 {
		t.Fatalf("parsed %d frames, want 2", len(frames))
	}
	if !bytes.Equal(frames[0], frame[:len(frame)-2]) {
		t.Fatalf("extended frame changed during parsing")
	}
	if !bytes.Equal(frames[1], frame[len(frame)-2:]) {
		t.Fatalf("control frame changed during parsing")
	}
	if pending := parser.pending; len(pending) != 0 {
		t.Fatalf("parser retained %d bytes after complete frames", len(pending))
	}
}

func TestGoldenWebSocketFrameParserRejectsInvalidLength(t *testing.T) {
	parser := goldenWebSocketFrameParser{}
	_, err := parser.append([]byte{0x82, 0x7f, 0x80, 0, 0, 0, 0, 0, 0, 0})
	if err == nil {
		t.Fatal("parser accepted a frame with the reserved 64-bit length bit set")
	}
	if err == io.EOF {
		t.Fatal("parser reported an incomplete frame instead of invalid length")
	}
}

func TestGoldenWebSocketFrameParserRejectsMalformedControlFrame(t *testing.T) {
	for _, test := range []struct {
		name  string
		frame []byte
	}{
		{name: "fragmented ping", frame: []byte{0x09, 0x00}},
		{name: "oversized ping", frame: []byte{0x89, 126, 0, 126}},
	} {
		t.Run(test.name, func(t *testing.T) {
			parser := goldenWebSocketFrameParser{}
			if _, err := parser.append(test.frame); err == nil {
				t.Fatal("parser accepted a malformed control frame")
			}
		})
	}
}

type blockingObserverDelay struct {
	started chan struct{}
	release chan struct{}
}

func (delay *blockingObserverDelay) wait(
	ctx context.Context,
	_ <-chan struct{},
	_ <-chan struct{},
	wake <-chan struct{},
	duration time.Duration,
) (bool, error) {
	_ = duration
	select {
	case <-delay.started:
	default:
		close(delay.started)
	}
	select {
	case <-delay.release:
		return false, nil
	case <-wake:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func TestGoldenWebSocketPacerDoesNotBlockReadsDuringPacingDelay(t *testing.T) {
	gate := newGoldenResponseGate()
	base := gate.newResponsePacer("read-ahead-test")
	delay := &blockingObserverDelay{started: make(chan struct{}), release: make(chan struct{})}
	base.delay = delay
	pacer := newGoldenWebSocketPacer(base)
	controlWritten := make(chan struct{}, 1)
	sink := func(payload []byte) (int, error) {
		if bytes.Equal(payload, []byte{0x89, 0x00}) {
			controlWritten <- struct{}{}
		}
		return len(payload), nil
	}
	data := make([]byte, 0, 100*3)
	for index := 0; index < 100; index++ {
		data = append(data, 0x81, 0x01, byte('a'+index%26))
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := pacer.write(context.Background(), data, sink)
		firstDone <- err
	}()
	select {
	case <-delay.started:
	case <-time.After(time.Second):
		t.Fatal("pacer did not enter a data pacing delay")
	}
	secondDone := make(chan error, 1)
	go func() {
		_, err := pacer.write(context.Background(), []byte{0x89, 0x00}, sink)
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("control write: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("control write blocked behind a pacing interval")
	}
	select {
	case <-controlWritten:
	case <-time.After(time.Second):
		t.Fatal("control frame was not delivered while data pacing was held")
	}
	close(delay.release)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("data write: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("data write did not finish after pacing release")
	}
	gate.releasePacing()
	if err := pacer.waitAndFlush(); err != nil {
		t.Fatalf("waitAndFlush: %v", err)
	}
}

func TestGoldenWebSocketPacerFlushesControlFramesBeforeGateRelease(t *testing.T) {
	gate := newGoldenResponseGate()
	base := gate.newResponsePacer("control-frame-test")
	base.delay = &accumulatingObserverDelay{}
	pacer := newGoldenWebSocketPacer(base)
	writes := newGoldenWebSocketWrites()

	// A control frame may arrive split across reads and after a held data frame.
	// The complete data frame must be sent first to preserve wire order, then the
	// control frame must be sent without waiting for the gate.
	if _, err := pacer.write(context.Background(), []byte{0x81, 0x01, 'x'}, writes.write); err != nil {
		t.Fatalf("data write: %v", err)
	}
	if _, err := pacer.write(context.Background(), []byte{0x89}, writes.write); err != nil {
		t.Fatalf("partial control write: %v", err)
	}
	if len(writes.snapshot()) != 0 {
		t.Fatalf("incomplete control frame changed the held tail: %x", writes.snapshot())
	}
	if _, err := pacer.write(context.Background(), []byte{0x00}, writes.write); err != nil {
		t.Fatalf("control write: %v", err)
	}
	writes.waitForCount(t, 2)
	gotWrites := writes.snapshot()
	if !bytes.Equal(gotWrites[0], []byte{0x81, 0x01, 'x'}) || !bytes.Equal(gotWrites[1], []byte{0x89, 0x00}) {
		t.Fatalf("control frame was held behind the gate: %x", gotWrites)
	}

	gate.releasePacing()
	if err := pacer.waitAndFlush(); err != nil {
		t.Fatalf("waitAndFlush: %v", err)
	}
}

func TestGoldenWebSocketPacerKeepsCloseFrameInHeldTail(t *testing.T) {
	gate := newGoldenResponseGate()
	base := gate.newResponsePacer("close-frame-test")
	base.delay = &accumulatingObserverDelay{}
	pacer := newGoldenWebSocketPacer(base)
	writes := newGoldenWebSocketWrites()
	stream := []byte{0x81, 0x01, 'x', 0x88, 0x00}
	if _, err := pacer.write(context.Background(), stream, writes.write); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(writes.snapshot()) != 0 {
		t.Fatalf("close/data tail was exposed before gate release: %x", writes.snapshot())
	}
	gate.releasePacing()
	if err := pacer.waitAndFlush(); err != nil {
		t.Fatalf("waitAndFlush: %v", err)
	}
	var delivered bytes.Buffer
	for _, write := range writes.snapshot() {
		_, _ = delivered.Write(write)
	}
	if !bytes.Equal(delivered.Bytes(), stream) {
		t.Fatalf("close frame was not released after gate release: %x", writes.snapshot())
	}
}

func TestGoldenWebSocketPacerDoesNotDelayQueuedPing(t *testing.T) {
	gate := newGoldenResponseGate()
	base := gate.newResponsePacer("queued-ping-test")
	delay := &accumulatingObserverDelay{}
	base.delay = delay
	pacer := newGoldenWebSocketPacer(base)
	sink := func(payload []byte) (int, error) { return len(payload), nil }
	frames := make([]byte, 0, 300)
	for index := 0; index < 100; index++ {
		frames = append(frames, 0x81, 0x01, byte('a'+index%26))
	}
	if _, err := pacer.write(context.Background(), frames, sink); err != nil {
		t.Fatalf("data write: %v", err)
	}
	beforePing := delay.elapsedDuration()
	if _, err := pacer.write(context.Background(), []byte{0x89, 0x00}, sink); err != nil {
		t.Fatalf("ping write: %v", err)
	}
	if afterPing := delay.elapsedDuration(); afterPing != beforePing {
		t.Fatalf("queued Ping added a pacing delay: before=%s after=%s", beforePing, afterPing)
	}
	gate.releasePacing()
	if err := pacer.waitAndFlush(); err != nil {
		t.Fatalf("waitAndFlush: %v", err)
	}
}

func TestGoldenWebSocketPacerDoesNotDelayControlFrameInSameWrite(t *testing.T) {
	gate := newGoldenResponseGate()
	base := gate.newResponsePacer("same-write-ping-test")
	delay := &accumulatingObserverDelay{}
	base.delay = delay
	pacer := newGoldenWebSocketPacer(base)
	writes := newGoldenWebSocketWrites()

	// The ping is in the same read as enough data to cross the holdback. The
	// pacer must inspect the complete batch before applying a data-frame delay.
	stream := make([]byte, 0, 100*3+2)
	for index := 0; index < 100; index++ {
		stream = append(stream, 0x81, 0x01, byte('a'+index%26))
	}
	stream = append(stream, 0x89, 0x00)
	if _, err := pacer.write(context.Background(), stream, writes.write); err != nil {
		t.Fatalf("write: %v", err)
	}
	writes.waitForCount(t, 101)
	if elapsed := delay.elapsedDuration(); elapsed != 0 {
		t.Fatalf("same-write Ping added a pacing delay: %s", elapsed)
	}
	gotWrites := writes.snapshot()
	if len(gotWrites) != 101 || !bytes.Equal(gotWrites[len(gotWrites)-1], []byte{0x89, 0x00}) {
		t.Fatalf("same-write Ping was not delivered promptly: %x", gotWrites)
	}

	gate.releasePacing()
	if err := pacer.waitAndFlush(); err != nil {
		t.Fatalf("waitAndFlush: %v", err)
	}
}

func TestGoldenWebSocketFrameParserRejectsOversizedFrames(t *testing.T) {
	parser := goldenWebSocketFrameParser{}
	length := uint64(goldenWebSocketMaxFrameBytes)
	header := []byte{0x82, 0x7f, byte(length >> 56), byte(length >> 48), byte(length >> 40), byte(length >> 32), byte(length >> 24), byte(length >> 16), byte(length >> 8), byte(length)}
	if _, err := parser.append(header); err == nil {
		t.Fatal("parser accepted a frame larger than the observer limit")
	}
}

func TestGoldenWebSocketPacerRetainsPartialWriteProgress(t *testing.T) {
	gate := newGoldenResponseGate()
	base := gate.newResponsePacer("partial-write-test")
	pacer := newGoldenWebSocketPacer(base)
	gate.releasePacing()
	frame := []byte{0x82, 0x03, 'a', 'b', 'c'}
	var delivered bytes.Buffer
	first := true
	sink := func(payload []byte) (int, error) {
		if first {
			first = false
			_, _ = delivered.Write(payload[:2])
			return 2, io.ErrClosedPipe
		}
		return delivered.Write(payload)
	}
	if _, err := pacer.write(context.Background(), frame, sink); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := pacer.waitAndFlush(); err == nil {
		t.Fatal("partial write error was lost")
	}
	if !bytes.Equal(delivered.Bytes(), frame[:2]) {
		t.Fatalf("partial write was duplicated or lost: got %x want prefix %x", delivered.Bytes(), frame[:2])
	}
}

func TestCountingConnEmitsClosedEventWhenWebSocketFlushFails(t *testing.T) {
	gate := newGoldenResponseGate()
	base := gate.newResponsePacer("truncated-close-test")
	stats := newObserverStats()
	observation := newObserver(io.Discard, stats)
	peer, connection := net.Pipe()
	t.Cleanup(func() {
		_ = peer.Close()
		_ = connection.Close()
	})

	counted := &countingConn{
		Conn: connection, observer: observation,
		meta:    requestEvidence{connectionID: "truncated-websocket"},
		context: context.Background(), pacer: base,
		websocketPacer: newGoldenWebSocketPacer(base),
	}
	if _, err := counted.Write([]byte{0x81}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	gate.releasePacing()
	if err := counted.Close(); err == nil {
		t.Fatal("Close succeeded for a truncated WebSocket frame")
	}
	closed := stats.closedSnapshot()
	if len(closed) != 1 || closed[0].ConnectionID != "truncated-websocket" {
		t.Fatalf("closed events = %+v, want one truncated-websocket event", closed)
	}
}

type goldenHijackResponseWriter struct {
	*httptest.ResponseRecorder
	connection net.Conn
}

func (w *goldenHijackResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.connection, bufio.NewReadWriter(bufio.NewReader(w.connection), bufio.NewWriter(w.connection)), nil
}

func TestCountingResponseWriterOnlyUsesWebSocketPacerForWebSocketUpgrade(t *testing.T) {
	tests := []struct {
		name      string
		websocket bool
		wantPacer bool
	}{
		{name: "websocket", websocket: true, wantPacer: true},
		{name: "other upgrade", websocket: false, wantPacer: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			peer, connection := net.Pipe()
			defer peer.Close()
			defer connection.Close()
			gate := newGoldenResponseGate()
			observer := newObserver(io.Discard, nil)
			writer := &countingResponseWriter{
				ResponseWriter: &goldenHijackResponseWriter{
					ResponseRecorder: httptest.NewRecorder(), connection: connection,
				},
				observer: observer,
				meta: requestEvidence{
					transport: "websocket", method: http.MethodGet,
					path: "/v1/responses", requestID: "request", connectionID: "connection",
				},
				context:          context.Background(),
				pacer:            gate.newResponsePacer("upgrade-test"),
				websocketUpgrade: test.websocket,
			}
			wrapped, _, err := writer.Hijack()
			if err != nil {
				t.Fatalf("Hijack: %v", err)
			}
			counted, ok := wrapped.(*countingConn)
			if !ok {
				t.Fatalf("Hijack returned %T, want *countingConn", wrapped)
			}
			if got := counted.websocketPacer != nil; got != test.wantPacer {
				t.Fatalf("websocket pacer present = %t, want %t", got, test.wantPacer)
			}
		})
	}
}
