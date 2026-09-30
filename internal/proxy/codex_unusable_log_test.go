package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// A Codex 429 fails over request-scoped, so a successful failover used to hide
// the refusal entirely. The diagnostic must name the upstream reason and the
// x-codex-* limit headers, leave the body intact for the caller, and never log
// credentials or request content.
func TestCodexUnusableResponseLogsReasonWithoutConsumingBody(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		wants []string
	}{
		{
			name:  "usage limit envelope",
			body:  `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"pro","resets_in_seconds":3600}}`,
			wants: []string{"error_code=usage_limit_reached", `error_message="The usage limit has been reached"`, "plan_type=pro", "resets_in_seconds=3600"},
		},
		{
			name:  "rate limit code",
			body:  `{"error":{"code":"rate_limit_exceeded","message":"Rate limit reached for tokens per min"}}`,
			wants: []string{"error_code=rate_limit_exceeded", "tokens per min"},
		},
		{
			name:  "detail refusal",
			body:  `{"detail":"Too many requests"}`,
			wants: []string{`error_message="Too many requests"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			response := &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header: http.Header{
					"Retry-After":                    []string{"7"},
					"X-Codex-Primary-Used-Percent":   []string{"9"},
					"X-Codex-Primary-Window-Minutes": []string{"10080"},
					"Authorization":                  []string{"Bearer secret-token"},
				},
				Body: io.NopCloser(strings.NewReader(tc.body)),
			}
			transport := usageLimitRetryTransport{
				provider:  accounts.ProviderCodex,
				logger:    slog.New(slog.NewTextHandler(&logs, nil)),
				agent:     "codex",
				session:   "session-1",
				method:    http.MethodPost,
				path:      "/v1/responses",
				upstream:  "chatgpt.com",
				poolModel: "gpt-6-sol",
			}
			transport.logCodexUnusableResponse(response, "account-a")
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != tc.body {
				t.Fatalf("diagnostic logging consumed or changed response body: %q", body)
			}
			got := logs.String()
			for _, want := range append([]string{"codex account unusable upstream response", "account=account-a", "status=429", "retry_after=7", "x-codex-primary-used-percent=9", "x-codex-primary-window-minutes=10080"}, tc.wants...) {
				if !strings.Contains(got, want) {
					t.Fatalf("logs missing %q: %s", want, got)
				}
			}
			if strings.Contains(got, "secret-token") || strings.Contains(strings.ToLower(got), "authorization") {
				t.Fatalf("diagnostic log includes credentials: %s", got)
			}
		})
	}
}
