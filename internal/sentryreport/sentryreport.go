// Package sentryreport wires optional Sentry error reporting into the
// subrouter server. Everything in this package is inert unless Init is called
// with a non-empty DSN, so a server without SENTRY_DSN behaves exactly as
// before.
//
// Privacy contract: this proxy carries auth tokens and full LLM request
// bodies. Events must never include request or response bodies,
// Authorization/X-Api-Key headers, OAuth refresh chains, or token-like
// strings. Enforcement is layered: the slog bridge only forwards an allowlist
// of structured attributes, and scrubEvent runs as BeforeSend on every
// outgoing event as defense in depth.
package sentryreport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
)

// Options configures Init. An empty DSN disables reporting entirely.
type Options struct {
	DSN         string
	Environment string
}

var enabled bool

// Init starts the Sentry client when opts.DSN is non-empty. It reports
// whether reporting is enabled. PII defaults stay off and every event passes
// through scrubEvent before send.
func Init(opts Options) (bool, error) {
	if strings.TrimSpace(opts.DSN) == "" {
		return false, nil
	}
	err := sentry.Init(sentry.ClientOptions{
		Dsn:              strings.TrimSpace(opts.DSN),
		Environment:      opts.Environment,
		Release:          release(),
		SendDefaultPII:   false,
		AttachStacktrace: true,
		BeforeSend: func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
			return scrubEvent(event)
		},
	})
	if err != nil {
		return false, err
	}
	enabled = true
	return true, nil
}

// Enabled reports whether Init succeeded with a DSN.
func Enabled() bool {
	return enabled
}

// Flush drains queued events; call it on shutdown.
func Flush(timeout time.Duration) {
	if enabled {
		sentry.Flush(timeout)
	}
}

func release() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return ""
	}
	return "subrouter@" + info.Main.Version
}

// tokenPattern matches credential-shaped strings that must never leave the
// process: OpenAI/Anthropic style sk- keys, subrouter srt_ tokens, JWTs
// (eyJ...), and any Bearer credential.
var tokenPattern = regexp.MustCompile(`(?i)\b(sk-[A-Za-z0-9._-]{4,}|srt_[A-Za-z0-9._-]{4,}|eyJ[A-Za-z0-9._/+=-]{8,}|Bearer\s+[^\s"']+)`)

// redactTokens replaces token-like substrings with a placeholder.
func redactTokens(value string) string {
	return tokenPattern.ReplaceAllString(value, "[redacted]")
}

// sensitiveHeaders are dropped from any request context that reaches an
// event. scrubEvent also nils the request body unconditionally.
var sensitiveHeaders = map[string]struct{}{
	"authorization":             {},
	"proxy-authorization":       {},
	"cookie":                    {},
	"set-cookie":                {},
	"x-api-key":                 {},
	"x-subrouter-admin-token":   {},
	"chatgpt-account-id":        {},
	"x-subrouter-user-email":    {},
	"openai-organization":       {},
	"anthropic-organization-id": {},
}

// scrubEvent removes bodies, auth headers, and token-like strings from an
// event. It runs as BeforeSend on every event, including panics captured by
// HTTPMiddleware.
func scrubEvent(event *sentry.Event) *sentry.Event {
	if event == nil {
		return nil
	}
	if event.Request != nil {
		event.Request.Data = ""
		event.Request.Cookies = ""
		event.Request.Env = nil
		headers := make(map[string]string, len(event.Request.Headers))
		for name, value := range event.Request.Headers {
			if _, sensitive := sensitiveHeaders[strings.ToLower(name)]; sensitive {
				continue
			}
			headers[name] = redactTokens(value)
		}
		event.Request.Headers = headers
		event.Request.QueryString = redactTokens(event.Request.QueryString)
	}
	event.Message = redactTokens(event.Message)
	for i := range event.Exception {
		event.Exception[i].Value = redactTokens(event.Exception[i].Value)
	}
	for key, value := range event.Tags {
		event.Tags[key] = redactTokens(value)
	}
	for name, contextData := range event.Contexts {
		for key, value := range contextData {
			if text, ok := value.(string); ok {
				event.Contexts[name][key] = redactTokens(text)
			}
		}
	}
	for i := range event.Breadcrumbs {
		if event.Breadcrumbs[i] == nil {
			continue
		}
		event.Breadcrumbs[i].Message = redactTokens(event.Breadcrumbs[i].Message)
		for key, value := range event.Breadcrumbs[i].Data {
			if text, ok := value.(string); ok {
				event.Breadcrumbs[i].Data[key] = redactTokens(text)
			}
		}
	}
	return event
}

// HTTPMiddleware recovers panics from the wrapped handler, reports them to
// Sentry with method/path tags only (never headers or body), and answers 500.
// http.ErrAbortHandler is re-panicked so httputil.ReverseProxy stream aborts
// keep their net/http semantics.
func HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			if err, ok := recovered.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(recovered)
			}
			hub := sentry.CurrentHub().Clone()
			hub.WithScope(func(scope *sentry.Scope) {
				scope.SetTag("http_method", r.Method)
				scope.SetTag("http_path", r.URL.Path)
				hub.RecoverWithContext(r.Context(), recovered)
			})
			Flush(2 * time.Second)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}

// forwardedWarnMessages are the Warn-level records that still represent
// operator-actionable failures (OAuth refresh chains dying). Everything else
// at Warn stays local.
var forwardedWarnMessages = map[string]struct{}{
	"account refresh failed":                                         {},
	"account reload refresh failed":                                  {},
	"retry Claude account refresh failed":                            {},
	"selected Claude account refresh failed, trying another account": {},
}

// attrTags maps slog attribute keys to Sentry tag names. Only these become
// tags; grouping and search stay stable.
var attrTags = map[string]string{
	"agent":    "agent_type",
	"upstream": "upstream",
	"status":   "http_status",
	"method":   "http_method",
	"provider": "provider",
}

// logContextKey is the event context that carries allowlisted non-tag
// attributes.
const logContextKey = "log_attributes"

// attrExtras are the additional attribute keys copied into the event's
// log_attributes context. Every other attribute is dropped: attributes are
// sensitive by default because log call sites include things like response
// bodies. The "account" attribute is handled separately in eventFromRecord:
// account IDs are usually OAuth account emails, so only a short hash goes
// out.
var attrExtras = map[string]struct{}{
	"path":    {},
	"session": {},
	"error":   {},
	"message": {},
	"reason":  {},
	"bytes":   {},
	"signal":  {},
}

// accountHash reduces an account ID (often an email) to a stable non-PII
// fingerprint so events still correlate per account. Match against local logs
// with: printf %s '<account-id>' | shasum -a 256 | cut -c1-12
func accountHash(accountID string) string {
	sum := sha256.Sum256([]byte(accountID))
	return hex.EncodeToString(sum[:])[:12]
}

// LogHandler is a slog.Handler that forwards Error-level records (plus the
// small refresh-failure Warn allowlist) to Sentry while delegating all output
// to the wrapped handler.
type LogHandler struct {
	base  slog.Handler
	attrs []slog.Attr
}

// NewLogHandler wraps base with Sentry forwarding.
func NewLogHandler(base slog.Handler) *LogHandler {
	return &LogHandler{base: base}
}

func (h *LogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.Enabled(ctx, level)
}

func (h *LogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &LogHandler{base: h.base.WithAttrs(attrs), attrs: append(append([]slog.Attr{}, h.attrs...), attrs...)}
}

func (h *LogHandler) WithGroup(name string) slog.Handler {
	// Groups are not used by subrouter's log sites; forward grouped records
	// to the base handler only.
	return &LogHandler{base: h.base.WithGroup(name), attrs: h.attrs}
}

func (h *LogHandler) Handle(ctx context.Context, record slog.Record) error {
	err := h.base.Handle(ctx, record)
	if !enabled {
		return err
	}
	if !shouldForward(record) {
		return err
	}
	event := eventFromRecord(record, h.attrs)
	sentry.CurrentHub().CaptureEvent(event)
	return err
}

func shouldForward(record slog.Record) bool {
	if record.Level >= slog.LevelError {
		return true
	}
	if record.Level >= slog.LevelWarn {
		_, ok := forwardedWarnMessages[record.Message]
		return ok
	}
	return false
}

func eventFromRecord(record slog.Record, handlerAttrs []slog.Attr) *sentry.Event {
	event := sentry.NewEvent()
	event.Message = redactTokens(record.Message)
	event.Logger = "slog"
	if record.Level >= slog.LevelError {
		event.Level = sentry.LevelError
	} else {
		event.Level = sentry.LevelWarning
	}
	logContext := sentry.Context{}
	apply := func(attr slog.Attr) {
		value := attr.Value.Resolve()
		if tag, ok := attrTags[attr.Key]; ok {
			event.Tags[tag] = redactTokens(fmt.Sprint(value.Any()))
			return
		}
		if attr.Key == "account" {
			logContext["account_hash"] = accountHash(fmt.Sprint(value.Any()))
			return
		}
		if _, ok := attrExtras[attr.Key]; ok {
			logContext[attr.Key] = redactTokens(fmt.Sprint(value.Any()))
		}
		// Any other key is intentionally dropped.
	}
	for _, attr := range handlerAttrs {
		apply(attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		apply(attr)
		return true
	})
	if len(logContext) > 0 {
		if event.Contexts == nil {
			event.Contexts = map[string]sentry.Context{}
		}
		event.Contexts[logContextKey] = logContext
	}
	return event
}
