package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// errCodexWebSocketTurnRetry says an upstream Codex websocket turn failed
// for a server-side reason (capacity, overload) before the client saw any of
// its output, on an autonomous connection. The failure is not delivered and
// the client socket stays open: the relay replays the turn's response.create
// upstream itself. Closing 1012 instead would make Codex reconnect and print
// "continue (the model was at capacity)" once per failure.
var errCodexWebSocketTurnRetry = errors.New("codex websocket turn is retried behind the open client socket")

const (
	codexWebSocketTurnRetryBase = 500 * time.Millisecond
	codexWebSocketTurnRetryMax  = 30 * time.Second
)

// codexWebSocketTurnRetryDelay is the wait before the attempt'th replay:
// exponential from codexWebSocketTurnRetryBase, capped at
// codexWebSocketTurnRetryMax, jittered into its upper half so sessions that
// failed together do not replay together.
func codexWebSocketTurnRetryDelay(attempt int) time.Duration {
	d := codexWebSocketTurnRetryMax
	if attempt < 16 {
		d = min(codexWebSocketTurnRetryBase<<attempt, codexWebSocketTurnRetryMax)
	}
	return d/2 + rand.N(d/2+1)
}

func (s Server) webSocketTurnRetryDelay(attempt int) time.Duration {
	if s.codexWebSocketTurnRetryDelay != nil {
		return s.codexWebSocketTurnRetryDelay(attempt)
	}
	return codexWebSocketTurnRetryDelay(attempt)
}

// webSocketUpstreamLink is the upstream side of one proxied websocket. Data
// frames to the upstream are written through it so an autonomous retry can
// resend a turn, or swap in a freshly dialed connection, without racing the
// client-to-upstream copy.
type webSocketUpstreamLink struct {
	mu     sync.Mutex
	conn   *websocket.Conn
	closed bool
	// done closes when the client side is finished; a retry waiting on a
	// backoff stops there.
	done chan struct{}
	// redial opens a new upstream connection on the same account, nil when
	// the connection is not autonomous.
	redial func(context.Context) (*websocket.Conn, error)
}

func newWebSocketUpstreamLink(conn *websocket.Conn) *webSocketUpstreamLink {
	return &webSocketUpstreamLink{conn: conn, done: make(chan struct{})}
}

func (l *webSocketUpstreamLink) current() *websocket.Conn {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn
}

// openWriter holds the link until the returned writer closes, so a resend or
// swap never interleaves with a forwarded client message.
func (l *webSocketUpstreamLink) openWriter(messageType int) (io.WriteCloser, error) {
	l.mu.Lock()
	writer, err := l.conn.NextWriter(messageType)
	if err != nil {
		l.mu.Unlock()
		return nil, err
	}
	return &lockedWebSocketWriter{WriteCloser: writer, unlock: l.mu.Unlock}, nil
}

func (l *webSocketUpstreamLink) send(body []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return websocket.ErrCloseSent
	}
	return l.conn.WriteMessage(websocket.TextMessage, body)
}

// swap replaces the upstream connection; false (and conn closed) when the
// client already went away.
func (l *webSocketUpstreamLink) swap(conn *websocket.Conn) bool {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		_ = conn.Close()
		return false
	}
	old := l.conn
	l.conn = conn
	l.mu.Unlock()
	_ = old.Close()
	return true
}

func (l *webSocketUpstreamLink) close() {
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.done)
	}
	conn := l.conn
	l.mu.Unlock()
	_ = conn.Close()
}

// wait sleeps d unless the request or the client ends first.
func (l *webSocketUpstreamLink) wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-l.done:
		return false
	case <-timer.C:
		return true
	}
}

type lockedWebSocketWriter struct {
	io.WriteCloser
	once   sync.Once
	unlock func()
}

func (w *lockedWebSocketWriter) Close() error {
	err := w.WriteCloser.Close()
	w.once.Do(w.unlock)
	return err
}

// bufferedWebSocketWriter writes its message to the client only on Close.
// streamWebSocketMessage vetoes a message by never closing its writer, and a
// gorilla writer left open is flushed by the connection's next NextWriter; an
// autonomous connection keeps writing after a veto, so its client messages
// are held until they are known to be deliverable.
type bufferedWebSocketWriter struct {
	conn        *websocket.Conn
	messageType int
	buffer      bytes.Buffer
}

func (w *bufferedWebSocketWriter) Write(p []byte) (int, error) { return w.buffer.Write(p) }

func (w *bufferedWebSocketWriter) Close() error {
	return w.conn.WriteMessage(w.messageType, w.buffer.Bytes())
}

// codexWebSocketCreateChained reports a response.create that continues from
// previous_response_id. The upstream keeps that response on the connection,
// so the turn can be resent on the same socket but not on a new one.
func codexWebSocketCreateChained(body []byte) bool {
	var event map[string]any
	if err := json.Unmarshal(body, &event); err != nil {
		return false
	}
	if stringField(event, "previous_response_id") != "" {
		return true
	}
	response, _ := event["response"].(map[string]any)
	return stringField(response, "previous_response_id") != ""
}

// retryCodexWebSocketTurn replays the turn in flight until it is accepted
// upstream, the client goes away, or the request ends. There is no attempt
// count. connAlive says the upstream connection survived the failure (it
// delivered a failure event), so the turn is resent on it; otherwise, or when
// a resend fails, the link redials the same account, which only a
// self-contained response.create survives. False means the turn cannot be
// retried here and the caller falls back to closing 1012.
func (s Server) retryCodexWebSocketTurn(ctx context.Context, link *webSocketUpstreamLink, modelState *webSocketModelState, agentType, sessionID, accountID string, connAlive bool) bool {
	body := modelState.pendingCreate()
	if body == nil || link.redial == nil {
		return false
	}
	chained := codexWebSocketCreateChained(body)
	for attempt := 0; ; attempt++ {
		if !link.wait(ctx, s.webSocketTurnRetryDelay(attempt)) {
			return false
		}
		if !connAlive {
			if chained {
				return false
			}
			conn, err := link.redial(ctx)
			if err != nil {
				if errors.As(err, new(webSocketRedialCredentialError)) {
					if s.Logger != nil {
						s.Logger.Warn("codex websocket turn retry hit a rejected credential; closing 1012 so the session reconnects",
							"agent", agentType, "session", sessionID, "account", accountID, "attempt", attempt, "error", err)
					}
					return false
				}
				if s.Logger != nil {
					s.Logger.Warn("codex websocket turn retry could not redial upstream; waiting",
						"agent", agentType, "session", sessionID, "account", accountID, "attempt", attempt, "error", err)
				}
				continue
			}
			if !link.swap(conn) {
				return false
			}
		}
		if err := link.send(body); err != nil {
			connAlive = false
			continue
		}
		if s.Logger != nil {
			s.Logger.Info("codex websocket turn resent behind the open client socket",
				"agent", agentType, "session", sessionID, "account", accountID, "attempt", attempt, "redialed", !connAlive)
		}
		return true
	}
}

// webSocketRedialCredentialError is a redial the upstream refused with 401:
// the account's credential is dead, so retrying it cannot succeed.
type webSocketRedialCredentialError struct{ err error }

func (e webSocketRedialCredentialError) Error() string {
	return "upstream rejected the credential: " + e.err.Error()
}
func (e webSocketRedialCredentialError) Unwrap() error { return e.err }

// webSocketUpstreamLost reports a copy error that is the connection itself
// failing, as opposed to a relay decision (quota reroute, Azure divert, a
// bounded capacity retry) or an oversized message, which a redial cannot fix.
func webSocketUpstreamLost(err error) bool {
	return !errors.Is(err, errCodexWebSocketReroute) &&
		!errors.Is(err, errAzureCodexWebSocketDivert) &&
		!errors.Is(err, errCodexWebSocketCapacityRetry) &&
		!errors.Is(err, websocket.ErrReadLimit)
}
