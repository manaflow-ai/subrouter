package proxy

import (
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"
)

// uploadAttemptTrace records what the transport actually did for one attempt at
// a replayable POST. It exists because the failure wording alone cannot tell
// these apart:
//
//   - "use of closed network connection" on a reused pooled connection means we
//     handed the attempt a connection the peer had already closed, which is our
//     bug to fix in pooling.
//   - the same wording on a freshly dialed connection means the upstream hung up
//     mid-upload, which is not something pooling can fix.
//
// Every field is written from httptrace callbacks that run on other goroutines,
// so they are all atomics.
type uploadAttemptTrace struct {
	reused        atomic.Bool
	gotConn       atomic.Bool
	idleMillis    atomic.Int64
	wroteRequest  atomic.Bool
	wroteErr      atomic.Value // string
	firstByte     atomic.Bool
	connectErr    atomic.Value // string
	requestStart  time.Time
	bytesExpected int64
}

func newUploadAttemptTrace(contentLength int64) *uploadAttemptTrace {
	return &uploadAttemptTrace{bytesExpected: contentLength}
}

// attach returns a request carrying the trace. The caller must use the returned
// request; the original is left untouched.
func (t *uploadAttemptTrace) attach(req *http.Request) *http.Request {
	if t == nil || req == nil {
		return req
	}
	t.requestStart = time.Now()
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			t.gotConn.Store(true)
			t.reused.Store(info.Reused)
			t.idleMillis.Store(info.IdleTime.Milliseconds())
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			t.wroteRequest.Store(true)
			if info.Err != nil {
				t.wroteErr.Store(info.Err.Error())
			}
		},
		GotFirstResponseByte: func() {
			t.firstByte.Store(true)
		},
		ConnectDone: func(_, _ string, err error) {
			if err != nil {
				t.connectErr.Store(err.Error())
			}
		},
	}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
}

// attrs returns slog key/value pairs describing the attempt. Only emitted on
// failure paths, so this adds no steady-state log volume.
func (t *uploadAttemptTrace) attrs() []any {
	if t == nil {
		return nil
	}
	attrs := []any{
		"conn_reused", t.reused.Load(),
		"got_conn", t.gotConn.Load(),
		"conn_idle_ms", t.idleMillis.Load(),
		"wrote_request", t.wroteRequest.Load(),
		// Whether any response byte arrived before the failure separates an
		// upstream that rejected the upload early from one that never answered.
		"got_first_byte", t.firstByte.Load(),
		"content_length", t.bytesExpected,
		"attempt_ms", time.Since(t.requestStart).Milliseconds(),
	}
	if wrote, ok := t.wroteErr.Load().(string); ok && wrote != "" {
		attrs = append(attrs, "wrote_request_err", wrote)
	}
	if connect, ok := t.connectErr.Load().(string); ok && connect != "" {
		attrs = append(attrs, "connect_err", connect)
	}
	return attrs
}
