package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// claudeSSEOverloadPeekMaxBytes bounds how much of a Claude SSE stream is
// buffered while looking for a pre-content overloaded_error. message_start and
// pings are small; anything past this is treated as a viable stream.
const claudeSSEOverloadPeekMaxBytes = 64 << 10

// claudeSSEOverloadPeekTimeout bounds how long the response headers are held
// back while peeking. On expiry the stream is handed to the client unchanged
// (the peek keeps its bytes and the body resumes after them), so a slow first
// event only loses the retry, never data.
var claudeSSEOverloadPeekTimeout = 3 * time.Second

// claudeSSEPeek is the result of reading a Claude SSE stream up to its first
// decisive event. The reading goroutine owns the body until done is closed.
type claudeSSEPeek struct {
	done       chan struct{}
	body       io.ReadCloser
	prefix     []byte
	overloaded bool
	denied     bool
	credit     bool
}

func (p *claudeSSEPeek) run() {
	defer close(p.done)
	var pending []byte
	buf := make([]byte, 4096)
	for len(p.prefix) < claudeSSEOverloadPeekMaxBytes {
		n, err := p.body.Read(buf)
		if n > 0 {
			p.prefix = append(p.prefix, buf[:n]...)
			pending = append(pending, buf[:n]...)
			for {
				i := bytes.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				line := bytes.TrimRight(pending[:i], "\r")
				pending = pending[i+1:]
				decided, overloaded, denied, credit := claudeSSELineDecision(line)
				if decided {
					p.overloaded = overloaded
					p.denied = denied
					p.credit = credit
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// claudeSSELineDecision classifies one SSE line. message_start and ping prove
// nothing; an error event decides the stream (overloaded_error is retryable,
// any other error passes through); every other data event is content or the
// end of the message, after which nothing may be replayed.
func claudeSSELineDecision(line []byte) (decided, overloaded, denied, credit bool) {
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return false, false, false, false
	}
	var ev struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(bytes.TrimSpace(data), &ev) != nil {
		return false, false, false, false
	}
	switch ev.Type {
	case "message_start", "ping", "":
		return false, false, false, false
	case "error":
		body := bytes.TrimSpace(data)
		return true, ev.Error.Type == "overloaded_error", persistentAuthorizationDenialJSON(body), claudeCreditExhaustedMessage(body)
	}
	return true, false, false, false
}

// claudeStreamOverloaded reports whether a 2xx Claude SSE response carries an
// overloaded_error before any content. It always leaves response.Body
// readable from the first byte, so a non-overloaded (or undecided) stream is
// forwarded exactly as upstream sent it.
func claudeStreamOverloaded(response *http.Response) bool {
	if response == nil || response.Body == nil || response.StatusCode < 200 || response.StatusCode >= 300 ||
		!strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		return false
	}
	peek := &claudeSSEPeek{done: make(chan struct{}), body: response.Body}
	go peek.run()
	timer := time.NewTimer(claudeSSEOverloadPeekTimeout)
	defer timer.Stop()
	select {
	case <-peek.done:
		response.Body = &claudeSSEPeekBody{ReadCloser: prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(peek.prefix), peek.body), Closer: peek.body}, denied: peek.denied, credit: peek.credit}
		return peek.overloaded
	case <-timer.C:
		// Hand the stream over undecided. The reader waits for the peek
		// goroutine to finish its in-flight read before replaying its bytes;
		// closing the body unblocks that read.
		response.Body = &claudeSSEDeferredBody{peek: peek}
		return false
	}
}

type claudeSSEPeekBody struct {
	io.ReadCloser
	denied bool
	credit bool
}

func claudeStreamPersistentAuthorizationDenied(response *http.Response) bool {
	if response == nil {
		return false
	}
	peek, ok := response.Body.(*claudeSSEPeekBody)
	return ok && peek.denied
}

func claudeStreamCreditExhausted(response *http.Response) bool {
	if response == nil {
		return false
	}
	peek, ok := response.Body.(*claudeSSEPeekBody)
	return ok && peek.credit
}

// claudeSSEDeferredBody replays an undecided peek once its goroutine is done.
type claudeSSEDeferredBody struct {
	peek   *claudeSSEPeek
	reader io.Reader
}

func (b *claudeSSEDeferredBody) Read(p []byte) (int, error) {
	if b.reader == nil {
		<-b.peek.done
		b.reader = io.MultiReader(bytes.NewReader(b.peek.prefix), b.peek.body)
	}
	return b.reader.Read(p)
}

func (b *claudeSSEDeferredBody) Close() error {
	return b.peek.body.Close()
}

// claudeOverloadRerouteCandidate picks the single alternate subscription
// account an overloaded request may try after its first same-account retries,
// when the reroute is opted in (Server.ClaudeOverloadReroute); by default the
// request never leaves its account on overload. Overload is usually API-wide, so this is deliberately narrow: OAuth
// accounts only, and only one with new-session headroom, so a sustained outage
// costs at most one extra upstream request rather than a pool fan-out.
func (t usageLimitRetryTransport) claudeOverloadRerouteCandidate(ctx context.Context, tried map[string]struct{}) (accounts.Account, bool) {
	next, reservation, ok := t.claudeOverloadRerouteCandidateWithReservation(ctx, tried)
	reservation.Release()
	return next, ok
}

// claudeOverloadRerouteCandidateWithReservation holds the selected replacement
// in the live placement load until its caller dispatches or abandons the retry.
func (t usageLimitRetryTransport) claudeOverloadRerouteCandidateWithReservation(ctx context.Context, tried map[string]struct{}) (accounts.Account, *selectacct.PlacementReservation, bool) {
	if t.server == nil {
		return accounts.Account{}, nil, false
	}
	next, reservation, err := t.server.oauthRetryCandidateWithReservation(ctx, t.provider, t.agent, t.session, t.userEmail, t.poolModel, tried, true, false)
	if err != nil || next.ID == "" {
		reservation.Release()
		return accounts.Account{}, nil, false
	}
	if !t.server.scheduler().ForModel(t.poolModel).UsableForNewSession(schedulerAccountProvider(next.Provider), next.ID) {
		reservation.Release()
		return accounts.Account{}, nil, false
	}
	return next, reservation, true
}
