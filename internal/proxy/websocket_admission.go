package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// Idle Responses sockets must leave room for a second connection, such as
// the one Codex opens to compact its approval reviewer's context.
type webSocketTurnAdmission struct {
	mu       sync.Mutex
	gate     *admissionControl
	ref      *selectacct.SchedulerRef
	provider accounts.Provider
	account  string
	who      admissionSessionKey
	releases []func()
}

var webSocketAdmissionBuffers = newWebSocketByteBudget(256 << 20)

// Responses must not compete with queued inputs for buffer space: terminal
// events release the slots those inputs are waiting for.
var webSocketAdmissionResponseBuffers = newWebSocketByteBudget(256 << 20)

var errWebSocketAdmissionBufferFull = errors.New("websocket admission buffer budget exhausted")

type webSocketQueuedMessage struct {
	opcode  int
	body    []byte
	release func()
}

// Keep reading while a turn waits for admission, including pipelined creates
// and the client's close frame. Both queued payload bytes and message count
// are bounded. Fail fast on overflow so queued input cannot block responses
// that release admission slots.
type webSocketClientMessages struct {
	ctx      context.Context
	cancel   context.CancelFunc
	messages chan webSocketQueuedMessage
	mu       sync.Mutex
	err      error
}

func newWebSocketClientMessages(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, budget *webSocketByteBudget) *webSocketClientMessages {
	q := &webSocketClientMessages{ctx: ctx, cancel: cancel, messages: make(chan webSocketQueuedMessage, defaultAdmissionQueue)}
	go func() {
		defer func() {
			close(q.messages)
			for message := range q.messages {
				message.release()
			}
		}()
		for {
			opcode, reader, err := conn.NextReader()
			if err != nil {
				q.fail(err)
				return
			}
			message, err := readWebSocketQueuedMessage(ctx, opcode, reader, budget)
			if err != nil {
				q.fail(err)
				return
			}
			select {
			case q.messages <- message:
			case <-ctx.Done():
				message.release()
				return
			default:
				message.release()
				q.fail(errAdmissionQueueFull)
				return
			}
		}
	}()
	return q
}

func (q *webSocketClientMessages) fail(err error) {
	q.mu.Lock()
	q.err = err
	q.mu.Unlock()
	q.cancel()
}

func (q *webSocketClientMessages) failure() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return q.err
	}
	return q.ctx.Err()
}

func readWebSocketQueuedMessage(ctx context.Context, opcode int, reader io.Reader, budget *webSocketByteBudget) (webSocketQueuedMessage, error) {
	chunk, releaseChunk, err := webSocketForwardBuffers.acquire(ctx)
	if err != nil {
		return webSocketQueuedMessage{}, err
	}
	defer releaseChunk()
	var body bytes.Buffer
	var reserved int64
	var once sync.Once
	release := func() { once.Do(func() { budget.release(reserved) }) }
	for {
		n, err := reader.Read(chunk)
		if n > 0 {
			if reserved+int64(n) > maxWebSocketMessageBytes {
				release()
				return webSocketQueuedMessage{}, websocket.ErrReadLimit
			}
			if !budget.tryReserve(int64(n)) {
				release()
				return webSocketQueuedMessage{}, errWebSocketAdmissionBufferFull
			}
			reserved += int64(n)
			body.Write(chunk[:n])
		}
		if err == io.EOF {
			return webSocketQueuedMessage{opcode: opcode, body: body.Bytes(), release: release}, nil
		}
		if err != nil {
			release()
			return webSocketQueuedMessage{}, err
		}
	}
}

func (a *webSocketTurnAdmission) begin(ctx context.Context, model string) error {
	lease, err := a.gate.acquireFor(ctx, admissionKeyFor(a.provider, a.account, model), a.who)
	if err != nil {
		return err
	}
	releaseInflight := a.ref.BeginInflight(a.provider, a.account)
	a.mu.Lock()
	a.releases = append(a.releases, func() {
		releaseInflight()
		if lease != nil {
			lease.release()
		}
	})
	a.mu.Unlock()
	return nil
}

func (a *webSocketTurnAdmission) finish() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.releases) > 0 {
		a.releases[0]()
		a.releases[0] = nil
		a.releases = a.releases[1:]
	}
}

func (a *webSocketTurnAdmission) close() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, release := range a.releases {
		release()
	}
	a.releases = nil
}
