package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The whole point of the trace is to answer one question from the log alone:
// did the failing attempt run on a pooled connection we handed it, or on a
// connection we had just dialed? Without that, "use of closed network
// connection" is unattributable.
func TestUploadTraceDistinguishesReusedFromFreshConnections(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	client := &http.Client{Transport: &http.Transport{}}

	first := newUploadAttemptTrace(0)
	request, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(first.attach(request))
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()

	if got := attrValue(first.attrs(), "conn_reused"); got != false {
		t.Errorf("first request conn_reused = %v, want false", got)
	}
	if got := attrValue(first.attrs(), "got_first_byte"); got != true {
		t.Errorf("first request got_first_byte = %v, want true", got)
	}

	second := newUploadAttemptTrace(0)
	request, err = http.NewRequest(http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err = client.Do(second.attach(request))
	if err != nil {
		t.Fatalf("second request: %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()

	if got := attrValue(second.attrs(), "conn_reused"); got != true {
		t.Errorf("second request conn_reused = %v, want true; a pooled reuse must be visible in the log", got)
	}
}

// A failure with no response byte is the case that has to be distinguishable
// from an upstream that answered and then hung up.
func TestUploadTraceRecordsFailureWithoutResponse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = conn.Close()
	}()

	trace := newUploadAttemptTrace(11)
	request, err := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String(), strings.NewReader("hello world"))
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{}}
	if response, doErr := client.Do(trace.attach(request)); doErr == nil {
		response.Body.Close()
		t.Fatal("request unexpectedly succeeded against a socket that resets")
	}
	<-done
	listener.Close()

	attrs := trace.attrs()
	if got := attrValue(attrs, "got_first_byte"); got != false {
		t.Errorf("got_first_byte = %v, want false; the upstream never answered", got)
	}
	if got := attrValue(attrs, "content_length"); got != int64(11) {
		t.Errorf("content_length = %v, want 11; upload size is what correlates these failures", got)
	}
}

// Failures are logged with the trace attached, so an operator reading the log
// can attribute them without reproducing anything.
func TestExhaustedRequestLogsTraceAttributes(t *testing.T) {
	var logs bytes.Buffer
	transport := replayablePostRetryTransport{
		base:        &http.Transport{},
		logger:      slog.New(slog.NewTextHandler(&logs, nil)),
		method:      http.MethodPost,
		path:        "/responses",
		maxAttempts: 1,
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close() // nothing is listening, so the dial fails

	request, err := http.NewRequest(http.MethodPost, "http://"+addr, strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	if response, roundTripErr := transport.RoundTrip(request); roundTripErr == nil {
		response.Body.Close()
		t.Fatal("request unexpectedly succeeded")
	}

	got := logs.String()
	if !strings.Contains(got, "replayable upstream request exhausted") {
		t.Fatalf("no exhaustion log line; the client-visible 502 would leave no record.\nlogs:\n%s", got)
	}
	for _, want := range []string{"conn_reused=", "got_first_byte=", "content_length=", "attempt_ms="} {
		if !strings.Contains(got, want) {
			t.Errorf("exhaustion log missing %q.\nlogs:\n%s", want, got)
		}
	}
}

func attrValue(attrs []any, key string) any {
	for i := 0; i+1 < len(attrs); i += 2 {
		if name, ok := attrs[i].(string); ok && name == key {
			return attrs[i+1]
		}
	}
	return nil
}
