package sentryreport

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
)

func TestScrubEventRemovesAuthorizationHeaders(t *testing.T) {
	event := sentry.NewEvent()
	event.Request = &sentry.Request{
		URL:         "https://chatgpt.com/backend-api/codex/responses",
		QueryString: "token=eyJhbGciOiJIUzI1NiJ9.payload.sig",
		Data:        `{"messages":[{"role":"user","content":"secret prompt"}]}`,
		Cookies:     "session=abc",
		Headers: map[string]string{
			"Authorization":           "Bearer sk-proj-abcdef1234567890",
			"authorization":           "Bearer srt_0123456789abcdef",
			"X-Api-Key":               "sk-ant-api03-abcdef",
			"Cookie":                  "session=abc",
			"X-Subrouter-Admin-Token": "srt_admintoken123",
			"User-Agent":              "codex-cli",
		},
		Env: map[string]string{"REMOTE_ADDR": "10.0.0.1"},
	}

	scrubbed := scrubEvent(event)

	if scrubbed.Request.Data != "" {
		t.Fatalf("request body must be dropped, got %q", scrubbed.Request.Data)
	}
	if scrubbed.Request.Cookies != "" {
		t.Fatalf("cookies must be dropped, got %q", scrubbed.Request.Cookies)
	}
	if scrubbed.Request.Env != nil {
		t.Fatalf("request env must be dropped, got %v", scrubbed.Request.Env)
	}
	for name := range scrubbed.Request.Headers {
		switch strings.ToLower(name) {
		case "authorization", "x-api-key", "cookie", "x-subrouter-admin-token":
			t.Fatalf("sensitive header %q must be dropped", name)
		}
	}
	if scrubbed.Request.Headers["User-Agent"] != "codex-cli" {
		t.Fatalf("benign header must survive, got %v", scrubbed.Request.Headers)
	}
	if strings.Contains(scrubbed.Request.QueryString, "eyJ") {
		t.Fatalf("JWT in query string must be redacted, got %q", scrubbed.Request.QueryString)
	}
}

func TestScrubEventRedactsTokenLikeStrings(t *testing.T) {
	event := sentry.NewEvent()
	event.Message = "refresh failed for sk-proj-abcdef1234567890 using srt_refresh0987654321"
	event.Exception = []sentry.Exception{{
		Type:  "error",
		Value: "invalid_grant: eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIx.abc123 rejected",
	}}
	event.Tags = map[string]string{"account": "Bearer sk-live-abc123456"}
	event.Contexts = map[string]sentry.Context{
		"log_attributes": {
			"error": "token srt_deadbeef00 expired",
			"count": 3,
		},
	}
	event.Breadcrumbs = []*sentry.Breadcrumb{{
		Message: "retried with sk-admin-1234567890",
		Data:    map[string]any{"auth": "Bearer eyJfoo.bar.baz"},
	}}

	scrubbed := scrubEvent(event)

	flat := scrubbed.Message +
		scrubbed.Exception[0].Value +
		scrubbed.Tags["account"] +
		scrubbed.Contexts["log_attributes"]["error"].(string) +
		scrubbed.Breadcrumbs[0].Message +
		scrubbed.Breadcrumbs[0].Data["auth"].(string)
	for _, marker := range []string{"sk-", "srt_", "eyJ", "Bearer "} {
		if strings.Contains(flat, marker) {
			t.Fatalf("token marker %q leaked: %q", marker, flat)
		}
	}
	if !strings.Contains(scrubbed.Message, "[redacted]") {
		t.Fatalf("expected redaction placeholder, got %q", scrubbed.Message)
	}
	if scrubbed.Contexts["log_attributes"]["count"] != 3 {
		t.Fatalf("non-string context values must be preserved, got %v", scrubbed.Contexts["log_attributes"]["count"])
	}
}

func TestEventFromRecordUsesAllowlistedAttrsOnly(t *testing.T) {
	record := slog.NewRecord(time.Now(), slog.LevelError, "proxy request failed", 0)
	record.AddAttrs(
		slog.String("agent", "codex"),
		slog.String("upstream", "chatgpt.com"),
		slog.Int("status", 502),
		slog.String("account", "alice@example.com"),
		slog.String("error", "dial tcp: refused; auth Bearer sk-proj-abc12345"),
		slog.String("body", `{"secret":"payload"}`),
		slog.String("refresh_token", "srt_supersecret1234"),
	)

	event := eventFromRecord(record, nil)

	if event.Tags["agent_type"] != "codex" || event.Tags["upstream"] != "chatgpt.com" || event.Tags["http_status"] != "502" {
		t.Fatalf("expected agent_type/upstream/http_status tags, got %v", event.Tags)
	}
	logContext := event.Contexts["log_attributes"]
	if _, ok := logContext["body"]; ok {
		t.Fatal("body attribute must never be forwarded")
	}
	if _, ok := logContext["refresh_token"]; ok {
		t.Fatal("non-allowlisted attributes must be dropped")
	}
	if _, ok := logContext["account"]; ok {
		t.Fatal("raw account id must never be forwarded")
	}
	if logContext["account_hash"] != accountHash("alice@example.com") {
		t.Fatalf("account must be forwarded as a hash, got %v", logContext["account_hash"])
	}
	if hash, _ := logContext["account_hash"].(string); strings.Contains(hash, "alice") || strings.Contains(hash, "@") {
		t.Fatalf("account hash must not embed the email, got %q", hash)
	}
	errExtra, _ := logContext["error"].(string)
	if strings.Contains(errExtra, "sk-") || strings.Contains(errExtra, "Bearer ") {
		t.Fatalf("token in error attr must be redacted, got %q", errExtra)
	}
	if !strings.Contains(errExtra, "dial tcp: refused") {
		t.Fatalf("error context should survive redaction, got %q", errExtra)
	}
}

func TestShouldForwardLevels(t *testing.T) {
	errRecord := slog.NewRecord(time.Now(), slog.LevelError, "anything", 0)
	if !shouldForward(errRecord) {
		t.Fatal("error records must forward")
	}
	warnListed := slog.NewRecord(time.Now(), slog.LevelWarn, "account refresh failed", 0)
	if !shouldForward(warnListed) {
		t.Fatal("allowlisted warn records must forward")
	}
	warnOther := slog.NewRecord(time.Now(), slog.LevelWarn, "usage fetch failed", 0)
	if shouldForward(warnOther) {
		t.Fatal("other warn records must not forward")
	}
	info := slog.NewRecord(time.Now(), slog.LevelInfo, "proxy request", 0)
	if shouldForward(info) {
		t.Fatal("info records must not forward")
	}
}

func TestLogHandlerDelegatesToBase(t *testing.T) {
	base := slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo})
	handler := NewLogHandler(base)
	if !handler.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("handler must mirror base enablement")
	}
	if handler.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("handler must mirror base enablement for disabled levels")
	}
	record := slog.NewRecord(time.Now(), slog.LevelError, "proxy request failed", 0)
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("handle: %v", err)
	}
	derived := handler.WithAttrs([]slog.Attr{slog.String("agent", "claude")})
	logHandler, ok := derived.(*LogHandler)
	if !ok {
		t.Fatalf("WithAttrs must return *LogHandler, got %T", derived)
	}
	event := eventFromRecord(record, logHandler.attrs)
	if event.Tags["agent_type"] != "claude" {
		t.Fatalf("handler attrs must contribute tags, got %v", event.Tags)
	}
}

func TestInitDisabledWithoutDSN(t *testing.T) {
	on, err := Init(Options{DSN: "   "})
	if err != nil {
		t.Fatalf("empty DSN must not error: %v", err)
	}
	if on {
		t.Fatal("empty DSN must leave reporting disabled")
	}
}
