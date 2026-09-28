package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func codexRetryRequest(t *testing.T) *http.Request {
	t.Helper()
	body := []byte(`{"model":"gpt-6-astra","input":"keep this model"}`)
	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.example/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-account-0")
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	req.ContentLength = int64(len(body))
	return req
}

func assertCodexRetryAttempt(t *testing.T, req *http.Request, attempt int) {
	t.Helper()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"model":"gpt-6-astra"`)) {
		t.Fatalf("attempt %d changed model: %s", attempt, body)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer test-account-0" {
		t.Fatalf("attempt %d changed account authorization: %q", attempt, got)
	}
}

func fastSameModelTransport(base http.RoundTripper) codexOverloadFailoverTransport {
	config := &CodexOverloadFailoverConfig{}
	fastCapacityGaps(config, time.Millisecond)
	now := time.Unix(1_800_000_000, 0)
	return codexOverloadFailoverTransport{
		base: base,
		server: &Server{
			CodexOverloadFailover: config,
			overloadHeld:          newOverloadHeldGauge(),
		},
		account:   "codex-account-0",
		poolModel: "gpt-6-astra",
		now:       func() time.Time { return now },
		sleep: func(ctx context.Context, gap time.Duration) bool {
			now = now.Add(gap)
			return ctx.Err() == nil
		},
	}
}

func TestCodexSameModelRetryDeadlinesUseInjectedClock(t *testing.T) {
	injectedNow := time.Now().Add(24 * time.Hour)
	config := &CodexOverloadFailoverConfig{
		sameAccountGap: func() time.Duration { return 2 * time.Second },
		persistGap:     func() time.Duration { return 2 * time.Second },
	}
	transport := codexOverloadFailoverTransport{
		server: &Server{CodexOverloadFailover: config},
		budget: newAttemptBudget(2),
		now:    func() time.Time { return injectedNow },
	}
	deadline := injectedNow.Add(time.Second)
	sameAccountLeft, switched := 1, 0
	if _, ok := transport.planDefaultRetry(context.Background(), "codex-account-0", "test", map[string]struct{}{}, &sameAccountLeft, &switched, 1, deadline); ok {
		t.Fatal("default retry planned a wait past the injected-clock deadline")
	}
	if _, ok := transport.planPersistRetry(context.Background(), "codex-account-0", map[string]struct{}{}, deadline); ok {
		t.Fatal("persist retry planned a wait past the injected-clock deadline")
	}
}

// A reset before response headers is safe to replay and must stay on the
// requested model. This is the failure form net/http returns for an upstream
// connection reset before any stream bytes can reach the client.
func TestCodexSameModelRetryRecoversConnectionReset(t *testing.T) {
	var calls int
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		assertCodexRetryAttempt(t, req, calls)
		if calls == 1 {
			return nil, errors.New("read tcp: connection reset by peer")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"status":"completed"}`)), Request: req}, nil
	})

	response, err := fastSameModelTransport(base).RoundTrip(codexRetryRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || calls != 2 {
		t.Fatalf("status=%d calls=%d, want recovery on attempt 2", response.StatusCode, calls)
	}
}

// Headerless Codex rate-limit 429s are transient burst limits. Explicit
// usage_limit_reached payloads remain owned by the account-quota layer.
func TestCodexSameModelRetryRecoversTransient429(t *testing.T) {
	var calls int
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		assertCodexRetryAttempt(t, req, calls)
		status := http.StatusTooManyRequests
		body := `{"error":{"type":"rate_limit_error","message":"slow down"}}`
		if calls == 2 {
			status = http.StatusOK
			body = `{"status":"completed","model":"gpt-6-astra"}`
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})

	response, err := fastSameModelTransport(base).RoundTrip(codexRetryRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || calls != 2 || !bytes.Contains(body, []byte(`"model":"gpt-6-astra"`)) {
		t.Fatalf("status=%d calls=%d body=%s, want same-model recovery on attempt 2", response.StatusCode, calls, body)
	}
}

func TestCodexSameModelRetry429DefaultsTransientUnlessExplicitTerminal(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		bodyless    bool
		wantRetry   bool
	}{
		{name: "bodyless", bodyless: true, wantRetry: true},
		{name: "non json", contentType: "text/plain", body: "upstream rate limited", wantRetry: true},
		{name: "malformed json", contentType: "application/json", body: `{"error":`, wantRetry: true},
		{name: "explicit quota", contentType: "application/json", body: `{"error":{"code":"usage_limit_reached","message":"quota exhausted"}}`},
		{name: "explicit client", contentType: "application/json", body: `{"error":{"code":"invalid_prompt","message":"bad prompt"}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls int
			base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				assertCodexRetryAttempt(t, req, calls)
				if calls > 1 {
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"status":"completed","model":"gpt-6-astra"}`)), Request: req}, nil
				}
				header := make(http.Header)
				if test.contentType != "" {
					header.Set("Content-Type", test.contentType)
				}
				body := io.ReadCloser(http.NoBody)
				if !test.bodyless {
					body = io.NopCloser(strings.NewReader(test.body))
				}
				return &http.Response{StatusCode: http.StatusTooManyRequests, Header: header, Body: body, Request: req}, nil
			})

			response, err := fastSameModelTransport(base).RoundTrip(codexRetryRequest(t))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, readErr := io.ReadAll(response.Body)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if test.wantRetry {
				if response.StatusCode != http.StatusOK || calls != 2 {
					t.Fatalf("status=%d calls=%d, want same-model recovery on attempt 2", response.StatusCode, calls)
				}
				return
			}
			if response.StatusCode != http.StatusTooManyRequests || calls != 1 {
				t.Fatalf("status=%d calls=%d, want terminal 429 without replay", response.StatusCode, calls)
			}
			if got := string(body); got != test.body {
				t.Fatalf("terminal body=%q, want original %q", got, test.body)
			}
		})
	}
}

func TestCodexSameModelRetryRecoversSSEResetBeforeVisibleOutput(t *testing.T) {
	tests := []struct {
		name   string
		stream string
	}{
		{name: "no bytes"},
		{name: "lifecycle only", stream: "data: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.queued\"}\n\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reset := errors.New("connection reset by peer")
			var calls int
			base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				assertCodexRetryAttempt(t, req, calls)
				if calls == 1 {
					body := io.MultiReader(strings.NewReader(test.stream), errorReader{err: reset})
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: readCloser{Reader: body, Closer: io.NopCloser(strings.NewReader(""))}, Request: req}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"status":"completed","model":"gpt-6-astra"}`)), Request: req}, nil
			})

			response, err := fastSameModelTransport(base).RoundTrip(codexRetryRequest(t))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, readErr := io.ReadAll(response.Body)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if response.StatusCode != http.StatusOK || calls != 2 || !bytes.Contains(body, []byte(`"model":"gpt-6-astra"`)) {
				t.Fatalf("status=%d calls=%d body=%s, want same-model recovery on attempt 2", response.StatusCode, calls, body)
			}
		})
	}
}

// Once visible output has been observed, replaying would duplicate it. The
// stream peek must hand the original body (and its reset) to the client and
// make no second upstream attempt.
func TestCodexSameModelRetryDoesNotReplayAfterPartialOutput(t *testing.T) {
	reset := errors.New("connection reset by peer")
	var calls int
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		assertCodexRetryAttempt(t, req, calls)
		stream := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: readCloser{Reader: io.MultiReader(strings.NewReader(stream), errorReader{err: reset}), Closer: io.NopCloser(strings.NewReader(""))}, Request: req}, nil
	})

	response, err := fastSameModelTransport(base).RoundTrip(codexRetryRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(response.Body)
	if !errors.Is(readErr, reset) {
		t.Fatalf("stream error=%v, want original reset", readErr)
	}
	if calls != 1 {
		t.Fatalf("upstream calls=%d, want 1 after visible output", calls)
	}
	if got := strings.Count(string(body), "partial"); got != 1 {
		t.Fatalf("partial output appeared %d times: %q", got, body)
	}
}
