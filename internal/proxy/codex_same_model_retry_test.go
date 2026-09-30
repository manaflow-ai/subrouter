package proxy

import (
	"bytes"
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
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	req.ContentLength = int64(len(body))
	return req
}

func fastSameModelTransport(base http.RoundTripper) codexOverloadFailoverTransport {
	config := &CodexOverloadFailoverConfig{}
	fastCapacityGaps(config, time.Millisecond)
	return codexOverloadFailoverTransport{
		base: base,
		server: &Server{
			CodexOverloadFailover: config,
			overloadHeld:          newOverloadHeldGauge(),
		},
		account:   "codex-account-0",
		poolModel: "gpt-6-astra",
	}
}

// A reset before response headers is safe to replay and must stay on the
// requested model. This is the failure form net/http returns for an upstream
// connection reset before any stream bytes can reach the client.
func TestCodexSameModelRetryRecoversConnectionReset(t *testing.T) {
	var calls int
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(body, []byte(`"model":"gpt-6-astra"`)) {
			t.Fatalf("attempt %d changed model: %s", calls, body)
		}
		if calls == 1 {
			return nil, errors.New("read tcp: connection reset by peer")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"status":"completed"}`)),
			Request:    req,
		}, nil
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
		status := http.StatusTooManyRequests
		body := `{"error":{"type":"rate_limit_error","message":"slow down"}}`
		if calls == 2 {
			status = http.StatusOK
			body = `{"status":"completed","model":"gpt-6-astra"}`
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
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

// Once visible output has been observed, replaying would duplicate it. The
// stream peek must hand the original body (and its reset) to the client and
// make no second upstream attempt.
func TestCodexSameModelRetryDoesNotReplayAfterPartialOutput(t *testing.T) {
	reset := errors.New("connection reset by peer")
	var calls int
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		stream := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: readCloser{
				Reader: io.MultiReader(strings.NewReader(stream), errorReader{err: reset}),
				Closer: io.NopCloser(strings.NewReader("")),
			},
			Request: req,
		}, nil
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
