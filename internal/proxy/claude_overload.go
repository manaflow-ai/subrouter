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
				decided, overloaded := claudeSSELineDecision(line)
				if decided {
					p.overloaded = overloaded
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
func claudeSSELineDecision(line []byte) (decided, overloaded bool) {
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return false, false
	}
	var ev struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(bytes.TrimSpace(data), &ev) != nil {
		return false, false
	}
	switch ev.Type {
	case "message_start", "ping", "":
		return false, false
	case "error":
		return true, ev.Error.Type == "overloaded_error"
	}
	return true, false
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
		response.Body = prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(peek.prefix), peek.body), Closer: peek.body}
		return peek.overloaded
	case <-timer.C:
		// Hand the stream over undecided. The reader waits for the peek
		// goroutine to finish its in-flight read before replaying its bytes;
		// closing the body unblocks that read.
		response.Body = &claudeSSEDeferredBody{peek: peek}
		return false
	}
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
// account an overloaded request may try once its same-account retries are
// spent. Overload is usually API-wide, so this is deliberately narrow: OAuth
// accounts only, and only one with new-session headroom, so a sustained outage
// costs at most one extra upstream request rather than a pool fan-out.
func (t usageLimitRetryTransport) claudeOverloadRerouteCandidate(ctx context.Context, tried map[string]struct{}) (accounts.Account, bool) {
	if t.server == nil {
		return accounts.Account{}, false
	}
	next, err := t.server.oauthRetryCandidate(ctx, t.provider, t.agent, t.session, t.userEmail, t.poolModel, tried, true, false)
	if err != nil || next.ID == "" {
		return accounts.Account{}, false
	}
	if !t.server.scheduler().ForModel(t.poolModel).UsableForNewSession(schedulerAccountProvider(next.Provider), next.ID) {
		return accounts.Account{}, false
	}
	return next, true
}

// retargetAttempt builds a replay of req for another account: fresh body,
// that account's upstream and auth headers, the same way the usage-limit
// failover path in RoundTrip does.
func (t usageLimitRetryTransport) retargetAttempt(req *http.Request, next accounts.Account) (*http.Request, error) {
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	attemptReq := req.Clone(req.Context())
	attemptReq.Body = body
	attemptReq.GetBody = req.GetBody
	attemptReq.ContentLength = req.ContentLength
	if nextUpstream := t.server.upstreamForRequest(t.path, next); nextUpstream != nil {
		attemptReq.URL.Scheme = nextUpstream.Scheme
		attemptReq.URL.Host = nextUpstream.Host
		attemptReq.URL.User = nextUpstream.User
		attemptReq.URL.Path = joinURLPath(nextUpstream.Path, t.server.pathForUpstream(t.path, next))
		attemptReq.URL.RawPath = ""
	}
	setAccountAuthHeaders(attemptReq.Header, next, t.poolModel)
	return attemptReq, nil
}
