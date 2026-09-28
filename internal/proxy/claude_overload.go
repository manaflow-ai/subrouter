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
	done           chan struct{}
	body           io.ReadCloser
	prefix         []byte
	overloaded     bool
	retryableReset bool
	readErr        error
}

func (p *claudeSSEPeek) run() {
	defer close(p.done)
	var pending []byte
	var eventData [][]byte
	var eventName string
	eventStarted := false
	buf := make([]byte, 4096)
	for len(p.prefix) < claudeSSEOverloadPeekMaxBytes {
		n, err := p.body.Read(buf)
		p.readErr = err
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
				if len(line) == 0 {
					if !eventStarted {
						continue
					}
					decided, overloaded := claudeSSEEventDecision(eventName, eventData)
					eventData = nil
					eventName = ""
					eventStarted = false
					if decided {
						p.overloaded = overloaded
						return
					}
					continue
				}
				eventStarted = true
				if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
					eventData = append(eventData, bytes.TrimSpace(data))
				} else if name, ok := bytes.CutPrefix(line, []byte("event:")); ok {
					eventName = strings.TrimSpace(string(name))
				}
			}
		}
		if err != nil {
			// A retry is safe only when the failed read followed no bytes or
			// complete lifecycle-only events. Any unfinished frame is ambiguous:
			// it may contain output that the client must see exactly once.
			p.retryableReset = retryablePostTransportError(err) && len(pending) == 0 && !eventStarted
			return
		}
	}
}

// claudeSSEEventDecision classifies one complete SSE event. message_start,
// ping, and comment-only events prove nothing; an error event decides the
// stream (overloaded_error is retryable, any other error passes through).
// Every other data event, including malformed or multi-line JSON, is treated
// as visible or ambiguous output after which the request must not be replayed.
func claudeSSEEventDecision(eventName string, dataLines [][]byte) (decided, overloaded bool) {
	switch eventName {
	case "", "message_start", "ping":
		// Only the explicit pre-content lifecycle events are safe to ignore.
	case "error":
		// A complete error event needs data to prove it is retryable overload.
	default:
		return true, false
	}
	if len(dataLines) == 0 {
		if eventName == "error" {
			return true, false
		}
		return false, false
	}
	data := bytes.Join(dataLines, []byte("\n"))
	var ev struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return true, false
	}
	switch ev.Type {
	case "message_start", "ping":
		return false, false
	case "error":
		return true, ev.Error.Type == "overloaded_error"
	}
	return true, false
}

// claudeSSEPeekBody replays everything consumed by the peek and then the
// terminal read error it observed. A transport body is allowed to return bytes
// and an error together, so retaining only the underlying body can silently
// turn a real reset into EOF after the peek consumed it.
type claudeSSEPeekBody struct {
	prefix []byte
	offset int
	err    error
	body   io.ReadCloser
}

func (b *claudeSSEPeekBody) Read(dst []byte) (int, error) {
	if b.offset < len(b.prefix) {
		n := copy(dst, b.prefix[b.offset:])
		b.offset += n
		return n, nil
	}
	if b.err != nil {
		err := b.err
		b.err = nil
		return 0, err
	}
	return b.body.Read(dst)
}

func (b *claudeSSEPeekBody) Close() error { return b.body.Close() }

type claudeStreamPeekResult struct {
	overloaded     bool
	retryableReset bool
	readErr        error
}

// claudeStreamPeek identifies a pre-content overload or unambiguous transport
// reset in a 2xx Claude SSE response. It always leaves response.Body readable
// from the first byte, with a consumed terminal read error restored, so a
// non-retryable stream is forwarded exactly as upstream sent it.
func claudeStreamPeek(response *http.Response) claudeStreamPeekResult {
	if response == nil || response.Body == nil || response.StatusCode < 200 || response.StatusCode >= 300 ||
		!strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		return claudeStreamPeekResult{}
	}
	peek := &claudeSSEPeek{done: make(chan struct{}), body: response.Body}
	go peek.run()
	timer := time.NewTimer(claudeSSEOverloadPeekTimeout)
	defer timer.Stop()
	select {
	case <-peek.done:
		if peek.readErr != nil {
			response.Body = &claudeSSEPeekBody{prefix: peek.prefix, err: peek.readErr, body: peek.body}
		} else {
			response.Body = prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(peek.prefix), peek.body), Closer: peek.body}
		}
		return claudeStreamPeekResult{overloaded: peek.overloaded, retryableReset: peek.retryableReset, readErr: peek.readErr}
	case <-timer.C:
		// Hand the stream over undecided. The reader waits for the peek
		// goroutine to finish its in-flight read before replaying its bytes;
		// closing the body unblocks that read.
		response.Body = &claudeSSEDeferredBody{peek: peek}
		return claudeStreamPeekResult{}
	}
}

func claudeStreamOverloaded(response *http.Response) bool {
	return claudeStreamPeek(response).overloaded
}

// claudeSSEDeferredBody replays an undecided peek once its goroutine is done.
type claudeSSEDeferredBody struct {
	peek   *claudeSSEPeek
	reader io.Reader
}

func (b *claudeSSEDeferredBody) Read(p []byte) (int, error) {
	if b.reader == nil {
		<-b.peek.done
		if b.peek.readErr != nil {
			b.reader = &claudeSSEPeekBody{prefix: b.peek.prefix, err: b.peek.readErr, body: b.peek.body}
		} else {
			b.reader = io.MultiReader(bytes.NewReader(b.peek.prefix), b.peek.body)
		}
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
	attemptReq := req.Clone(withAttemptAccount(req.Context(), next))
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
