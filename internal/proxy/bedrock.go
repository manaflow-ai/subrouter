package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// BedrockConfig configures the Bedrock signing gateway. When enabled, requests
// to /bedrock/* are re-signed with the team's AWS credentials (SigV4) and
// forwarded to bedrock-runtime, so clients (e.g. Claude Code in Bedrock gateway
// mode with CLAUDE_CODE_SKIP_BEDROCK_AUTH=1) never need AWS credentials.
type BedrockConfig struct {
	// Regions is the ordered set of Bedrock runtime regions to try. List only
	// regions where the target inference profile has model access and TPM quota;
	// Bedrock 4xx model-access failures are terminal and are not retried.
	Regions     []string
	Credentials aws.CredentialsProvider
	Sources     []BedrockCredentialSource
	// GatewayToken, when non-empty, must be presented by clients via the
	// Authorization: Bearer header (Claude Code's ANTHROPIC_AUTH_TOKEN). Empty
	// means the endpoint relies on network-level trust like the rest of the proxy.
	GatewayToken string
	Transport    http.RoundTripper
	// CostLogPath is the JSONL file where per-request token usage and estimated
	// cost are appended. Empty disables cost tracking.
	CostLogPath string
	// Bumper, when set, requests a Service Quotas increase when Bedrock throttles
	// (HTTP 429), deduped per quota with a cooldown.
	Bumper      *bedrockQuotaBumper
	nextAttempt atomic.Uint64
}

type BedrockCredentialSource struct {
	Name        string
	Credentials aws.CredentialsProvider
	Bumper      *bedrockQuotaBumper
}

const bedrockService = "bedrock"

type bedrockAttempt struct {
	Region string
	Source BedrockCredentialSource
}

func (s Server) bedrockHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.Bedrock
		if cfg == nil || !cfg.configured() {
			http.Error(w, "bedrock gateway not configured", http.StatusServiceUnavailable)
			return
		}
		if cfg.GatewayToken != "" && !bedrockGatewayTokenOK(r, cfg.GatewayToken) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		upstreamPath := strings.TrimPrefix(r.URL.Path, "/bedrock")
		if upstreamPath == "" || upstreamPath == "/" {
			http.NotFound(w, r)
			return
		}
		if !strings.HasPrefix(upstreamPath, "/") {
			upstreamPath = "/" + upstreamPath
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, replayablePostMaxBodyBytes))
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}

		headers := http.Header{}
		copyBedrockRequestHeaders(headers, r.Header)
		started := time.Now()
		resp, sourceName, region, err := s.signAndForwardBedrockWithHeaders(r.Context(), r.Method, upstreamPath, r.URL.RawQuery, headers, body)
		if err != nil {
			if s.Logger != nil {
				s.Logger.Error("bedrock upstream request failed", "path", upstreamPath, "remote_addr", clientRemoteIP(r), "user_agent", r.UserAgent(), "error", err)
			}
			http.Error(w, "bedrock upstream request failed", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		for key, values := range resp.Header {
			if isHopByHopHeader(key) {
				continue
			}
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(resp.StatusCode)

		model := bedrockModelFromPath(upstreamPath)
		if resp.StatusCode == http.StatusTooManyRequests {
			if s.Logger != nil {
				s.Logger.Warn("bedrock throttled", "model", model, "path", upstreamPath, "remote_addr", clientRemoteIP(r), "user_agent", r.UserAgent(), "bedrock_source", sourceName, "region", region)
			}
			cfg.onThrottle(sourceName, region, model)
		}
		usage, haveUsage := s.streamBedrockResponse(w, resp)
		if cfg.CostLogPath != "" && model != "" {
			record := bedrockCostRecord{
				Timestamp:  started.UTC().Format(time.RFC3339),
				Model:      model,
				Region:     region,
				Status:     resp.StatusCode,
				DurationMs: time.Since(started).Milliseconds(),
			}
			if haveUsage {
				record.Usage = usage
				record.CostUSD = usage.costUSD(model)
			}
			appendBedrockCostRecord(cfg.CostLogPath, record)
		}
	})
}

const bedrockFableModelID = "us.anthropic.claude-fable-5"

// claudeFableBedrockResponse forwards a Fable Messages request to Bedrock and
// returns a native Anthropic-shaped response: SSE (transcoded from AWS
// event-stream framing) for streaming requests, JSON otherwise. Cost is
// recorded like the /bedrock/* gateway path.
func (s Server) claudeFableBedrockResponse(ctx context.Context, body []byte) (*http.Response, error) {
	cfg := s.Bedrock
	// Bedrock's Anthropic schema rejects OAuth-only request fields with a 400
	// ("context_management: Extra inputs are not permitted"), which used to
	// push every context-editing Claude Code request straight to the API key.
	body = stripClaudeUnsupportedFields(body)
	var strippedTools int
	body, strippedTools = stripBedrockUnsupportedTools(body)
	if strippedTools > 0 && s.Logger != nil {
		s.Logger.Warn("stripped bedrock-unsupported server tools from claude-fable request", "count", strippedTools)
	}
	if !s.ClaudeFableCacheTTLUpgradeOff {
		body = upgradeEphemeralCacheTTL(body)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	stream := false
	if raw, ok := payload["stream"]; ok {
		_ = json.Unmarshal(raw, &stream)
	}
	// Bedrock takes the model in the URL and streaming via the endpoint; the body
	// must carry anthropic_version and must not carry model/stream.
	delete(payload, "model")
	delete(payload, "stream")
	payload["anthropic_version"] = json.RawMessage(`"bedrock-2023-05-31"`)
	newBody, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	endpoint := "invoke"
	if stream {
		endpoint = "invoke-with-response-stream"
	}
	path := "/model/" + bedrockFableModelID + "/" + endpoint
	var resp *http.Response
	var sourceName, region string
	started := time.Now()
	for attempt := 1; ; attempt++ {
		started = time.Now()
		resp, sourceName, region, err = s.signAndForwardBedrock(ctx, http.MethodPost, path, newBody)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			if s.Logger != nil {
				s.Logger.Warn("bedrock throttled", "model", bedrockFableModelID, "path", path, "bedrock_source", sourceName, "region", region)
			}
			cfg.onThrottle(sourceName, region, bedrockFableModelID)
		}
		if !stream || resp.StatusCode != http.StatusOK {
			break
		}
		// The peek's forced deadline only fires between reads. A stream that
		// goes completely silent blocks inside Read where no check can run, so
		// a watchdog closes the body after the deadline plus a grace period
		// (giving a frame-carrying stream time to force-commit first). The
		// close errors the blocked Read and the attempt fails retryably. If
		// the watchdog raced a commit, the body is already unusable: downgrade
		// to the same retryable failure instead of streaming a closed body.
		//
		// The FINAL attempt rides without the watchdog. Before the first
		// frame, a hung stream and a long prefill are indistinguishable:
		// Bedrock sends message_start only after prefill, so a large
		// cache-miss prompt is silent for minutes on every attempt (seen
		// live 2026-08-26: one request watchdog-killed at 135s on attempts
		// 1 and 2, abandoned by the client mid-attempt 3). Killing the last
		// attempt makes such requests permanently unservable and re-bills
		// the prefill each round; riding lets the client's own context
		// bound the wait, and a client cancel still errors the blocked Read
		// through the request context.
		// Non-final attempts split the silence deadline by whether the
		// stream has answered at all (production 2026-08-26: stalled
		// attempts held ~1KB of early frames then went silent, and two
		// 135s kills exhausted the client's patience before the riding
		// final attempt began):
		//   - zero bytes yet: the initial deadline (forced-commit + grace)
		//     tolerates long prefill, which is silent by nature;
		//   - after first bytes: prefill is over, so silence past the
		//     shorter idle deadline is a stall; killing it fast leaves the
		//     client patience for the retries and the riding final attempt.
		// The watchdog binds the attempt-local body at construction, so a
		// callback that fires as its peek fails can never close a later
		// attempt's body.
		finalAttempt := attempt >= claudeFableBedrockStreamAttempts
		var peekWatchdog *bedrockIdleWatchdogReader
		peekSrc := io.Reader(resp.Body)
		if !finalAttempt {
			peekWatchdog = newBedrockIdleWatchdogReader(resp.Body, resp.Body, claudeFableBedrockPeekForceCommitAfter+claudeFableBedrockPeekSilenceGrace, claudeFableBedrockPeekIdleTimeout)
			peekSrc = peekWatchdog
		}
		peek := peekBedrockStreamUntilCommit(peekSrc)
		if peekWatchdog != nil {
			peekWatchdog.stop()
			if peekWatchdog.hasFired() && peek.outcome == bedrockPeekCommit {
				peek = bedrockStreamPeek{outcome: bedrockPeekReadErr, readErr: errBedrockPeekWatchdog, peeked: peek.peeked}
			}
		}
		if peek.outcome == bedrockPeekCommit {
			pr, pw := io.Pipe()
			streamStarted := started
			streamRegion := region
			streamSource := sourceName
			body := resp.Body
			peeked := peek.peeked
			commitReason := peek.commitReason
			commitAt := peek.commitAt
			go func() {
				// Committed streams have no forced deadline: a stream that goes
				// silent blocks inside Read until the client's own stall
				// detector cancels the request minutes later. The watchdog
				// closes the body after a bounded silence so the blocked Read
				// fails now and the client gets an in-band retryable error
				// instead of a dead connection.
				watchdog := newBedrockIdleWatchdogReader(io.MultiReader(bytes.NewReader(peeked), body), body, claudeFableBedrockStreamIdleTimeout, claudeFableBedrockStreamIdleTimeout)
				result := transcodeBedrockToSSESince(pw, watchdog, s.Logger, streamSource, streamRegion, streamStarted, commitReason, commitAt)
				watchdog.stop()
				_ = body.Close()
				_ = pw.Close()
				s.recordClaudeFableBedrockCost(streamStarted, streamRegion, http.StatusOK, result.Usage, result.HaveUsage)
			}()
			return &http.Response{
				Status:        "200 OK",
				StatusCode:    http.StatusOK,
				Proto:         "HTTP/1.1",
				ProtoMajor:    1,
				ProtoMinor:    1,
				Header:        http.Header{"Content-Type": {"text/event-stream"}, "Cache-Control": {"no-cache"}},
				Body:          pr,
				ContentLength: -1,
			}, nil
		}
		retryable := bedrockPeekFailureRetryable(peek)
		// Error bodies only (never message content): the failing frame is an
		// exception or an Anthropic error event, so logging it is safe.
		if s.Logger != nil {
			// elapsed_ms and peeked_bytes separate a hung connection (long
			// elapsed, zero bytes) from an upstream that answered and shed.
			attrs := []any{"bedrock_source", sourceName, "region", region, "saw_message_stop", false, "attempt", attempt, "retryable", retryable, "elapsed_ms", time.Since(started).Milliseconds(), "peeked_bytes", len(peek.peeked)}
			if peek.errorFrame.exceptionType != "" {
				attrs = append(attrs, "exception_type", peek.errorFrame.exceptionType)
			}
			if len(peek.errorFrame.payload) > 0 {
				attrs = append(attrs, "message", bedrockLogPreview(peek.errorFrame.payload))
			}
			if peek.readErr != nil {
				attrs = append(attrs, "read_err", peek.readErr)
			}
			s.Logger.Warn("claude-fable bedrock stream failed before content", attrs...)
		}
		_ = resp.Body.Close()
		s.recordClaudeFableBedrockCost(started, region, http.StatusServiceUnavailable, bedrockUsage{}, false)
		if retryable && attempt < claudeFableBedrockStreamAttempts && bedrockRetryBackoff(ctx, attempt) {
			// A fresh signAndForwardBedrock call starts from the next
			// credential source/region rotation, so the retry prefers a
			// different endpoint when more than one is configured.
			continue
		}
		errBody := peek.errorFrame.payload
		if len(errBody) == 0 {
			errBody = []byte(`{"type":"error","error":{"type":"api_error","message":"Bedrock stream failed before content"}}`)
		}
		return &http.Response{
			Status:        "503 Service Unavailable",
			StatusCode:    http.StatusServiceUnavailable,
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        http.Header{"Content-Type": {"application/json"}},
			Body:          io.NopCloser(bytes.NewReader(errBody)),
			ContentLength: int64(len(errBody)),
		}, nil
	}
	// Non-stream success or any error status: Bedrock answers Anthropic-format JSON.
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, replayablePostMaxBodyBytes))
	_ = resp.Body.Close()
	if (resp.StatusCode < 200 || resp.StatusCode >= 300) && s.Logger != nil {
		// Error bodies only (no user content): without this the concrete
		// Bedrock failure ("Too many tokens...", validation errors) is
		// invisible once the chain falls through to the API key.
		preview := respBody
		if len(preview) > 512 {
			preview = preview[:512]
		}
		s.Logger.Warn("claude-fable bedrock error response", "status", resp.StatusCode, "bedrock_source", sourceName, "region", region, "body", string(preview))
	}
	usage, haveUsage := parseBedrockInvokeUsage(respBody)
	s.recordClaudeFableBedrockCost(started, region, resp.StatusCode, usage, haveUsage)
	return &http.Response{
		Status:        resp.Status,
		StatusCode:    resp.StatusCode,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(respBody)),
		ContentLength: int64(len(respBody)),
	}, nil
}

// upgradeEphemeralCacheTTL rewrites every bare cache_control
// {"type":"ephemeral"} to the 1-hour TTL on the Bedrock path. Cost-log data
// (2026-08-20): 88% of daily cache-write spend came from 819 requests that
// re-wrote their whole context after the 5-minute cache expired during a long
// tool run, and 89% of rewrite-causing gaps were under an hour. 1h writes cost
// 1.6x the 5m rate, so the upgrade pays whenever more than 43% of rewrites
// fall inside the hour. An explicit client-set ttl is respected untouched.
// The byte-literal match is safe: inside a JSON string value the quotes would
// be escaped, so the pattern can only match real cache_control objects.
func upgradeEphemeralCacheTTL(body []byte) []byte {
	return bytes.ReplaceAll(body,
		[]byte(`"cache_control":{"type":"ephemeral"}`),
		[]byte(`"cache_control":{"type":"ephemeral","ttl":"1h"}`))
}

// bedrockUnsupportedToolPrefixes lists Anthropic server-side tool types that
// Bedrock rejects with a 400 ("tool type 'web_search_20250305' is not
// supported for this model"). The direct Anthropic API supports them, so this
// filter runs only on the Bedrock path; stripping the definition just means
// the model never calls the tool, which is a strict improvement over failing
// the whole request. Client tools (input_schema, custom types) and the
// Bedrock-supported computer/text_editor/bash types pass through untouched.
var bedrockUnsupportedToolPrefixes = []string{"web_search", "web_fetch", "code_execution"}

// stripBedrockUnsupportedTools removes server tools Bedrock rejects from the
// request's tools array, returning the possibly rebuilt body and how many
// entries were dropped.
func stripBedrockUnsupportedTools(body []byte) ([]byte, int) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return body, 0
	}
	rawTools, ok := payload["tools"]
	if !ok {
		return body, 0
	}
	var tools []json.RawMessage
	if json.Unmarshal(rawTools, &tools) != nil {
		return body, 0
	}
	kept := make([]json.RawMessage, 0, len(tools))
	dropped := 0
	for _, tool := range tools {
		var meta struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(tool, &meta)
		unsupported := false
		for _, prefix := range bedrockUnsupportedToolPrefixes {
			if strings.HasPrefix(meta.Type, prefix) {
				unsupported = true
				break
			}
		}
		if unsupported {
			dropped++
			continue
		}
		kept = append(kept, tool)
	}
	if dropped == 0 {
		return body, 0
	}
	if len(kept) == 0 {
		delete(payload, "tools")
	} else if rebuilt, err := json.Marshal(kept); err == nil {
		payload["tools"] = rebuilt
	} else {
		return body, 0
	}
	rebuilt, err := json.Marshal(payload)
	if err != nil {
		return body, 0
	}
	return rebuilt, dropped
}

func (s Server) recordClaudeFableBedrockCost(started time.Time, region string, status int, usage bedrockUsage, haveUsage bool) {
	cfg := s.Bedrock
	if cfg == nil || cfg.CostLogPath == "" {
		return
	}
	record := bedrockCostRecord{
		Timestamp:  started.UTC().Format(time.RFC3339),
		Model:      bedrockFableModelID,
		Region:     region,
		Status:     status,
		DurationMs: time.Since(started).Milliseconds(),
	}
	if haveUsage {
		record.Usage = usage
		record.CostUSD = usage.costUSD(bedrockFableModelID)
	}
	appendBedrockCostRecord(cfg.CostLogPath, record)
}

// signAndForwardBedrock SigV4-signs a JSON body to bedrock-runtime and returns
// the raw response plus the Bedrock source name that handled it.
func (s Server) signAndForwardBedrock(ctx context.Context, method, upstreamPath string, body []byte) (*http.Response, string, string, error) {
	headers := http.Header{"Content-Type": []string{"application/json"}}
	return s.signAndForwardBedrockWithHeaders(ctx, method, upstreamPath, "", headers, body)
}

func (s Server) signAndForwardBedrockWithHeaders(ctx context.Context, method, upstreamPath, rawQuery string, headers http.Header, body []byte) (*http.Response, string, string, error) {
	cfg := s.Bedrock
	attempts := cfg.orderedAttempts()
	var firstErr error
	for i, attempt := range attempts {
		resp, err := s.signAndForwardBedrockWithSource(ctx, attempt.Source, attempt.Region, method, upstreamPath, rawQuery, headers, body)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if s.Logger != nil {
				s.Logger.Warn("bedrock source failed", "bedrock_source", attempt.Source.Name, "region", attempt.Region, "path", upstreamPath, "error", err)
			}
			continue
		}
		// Rotate to the next credential source on a throttle (429, per-account
		// TPM) or a Bedrock-side 5xx (e.g. 503 "Bedrock is unable to process
		// your request"): both are specific to this source's account/endpoint
		// and another source may serve the request fine.
		if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && i+1 < len(attempts) {
			if s.Logger != nil {
				s.Logger.Warn("bedrock source unusable, retrying next source", "bedrock_source", attempt.Source.Name, "region", attempt.Region, "path", upstreamPath, "status", resp.StatusCode)
			}
			if resp.StatusCode == http.StatusTooManyRequests {
				cfg.onThrottle(attempt.Source.Name, attempt.Region, bedrockModelFromPath(upstreamPath))
			}
			resp.Body.Close()
			continue
		}
		return resp, attempt.Source.Name, attempt.Region, nil
	}
	if firstErr != nil {
		return nil, "", "", firstErr
	}
	return nil, "", "", io.ErrUnexpectedEOF
}

func (s Server) signAndForwardBedrockWithSource(ctx context.Context, source BedrockCredentialSource, region, method, upstreamPath, rawQuery string, headers http.Header, body []byte) (*http.Response, error) {
	cfg := s.Bedrock
	host := "bedrock-runtime." + region + ".amazonaws.com"
	target := &url.URL{Scheme: "https", Host: host, Path: upstreamPath, RawQuery: rawQuery}
	outReq, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for key, values := range headers {
		for _, value := range values {
			outReq.Header.Add(key, value)
		}
	}
	if outReq.Header.Get("Content-Type") == "" {
		outReq.Header.Set("Content-Type", "application/json")
	}
	outReq.Host = host
	outReq.ContentLength = int64(len(body))
	creds, err := source.Credentials.Retrieve(ctx)
	if err != nil {
		return nil, err
	}
	if err := v4.NewSigner().SignHTTP(ctx, creds, outReq, sha256Hex(body), bedrockService, region, time.Now()); err != nil {
		return nil, err
	}
	transport := cfg.Transport
	if transport == nil {
		transport = s.Transport
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	return transport.RoundTrip(outReq)
}

func (cfg *BedrockConfig) configured() bool {
	return len(cfg.regions()) > 0 && len(cfg.sources()) > 0
}

func (cfg *BedrockConfig) primaryRegion() string {
	if len(cfg.Regions) == 0 {
		return ""
	}
	return cfg.Regions[0]
}

func (cfg *BedrockConfig) regions() []string {
	var out []string
	for _, region := range cfg.Regions {
		region = strings.TrimSpace(region)
		if region == "" {
			continue
		}
		out = append(out, region)
	}
	return out
}

func (cfg *BedrockConfig) sources() []BedrockCredentialSource {
	var out []BedrockCredentialSource
	for _, source := range cfg.Sources {
		if source.Credentials == nil {
			continue
		}
		name := strings.TrimSpace(source.Name)
		if name == "" {
			name = "default"
		}
		out = append(out, BedrockCredentialSource{Name: name, Credentials: source.Credentials, Bumper: source.Bumper})
	}
	if len(out) == 0 && cfg.Credentials != nil {
		out = append(out, BedrockCredentialSource{Name: "default", Credentials: cfg.Credentials, Bumper: cfg.Bumper})
	}
	return out
}

func (cfg *BedrockConfig) orderedAttempts() []bedrockAttempt {
	regions := cfg.regions()
	sources := cfg.sources()
	if len(regions) == 0 || len(sources) == 0 {
		return nil
	}
	attempts := make([]bedrockAttempt, 0, len(regions)*len(sources))
	for _, region := range regions {
		for _, source := range sources {
			attempts = append(attempts, bedrockAttempt{Region: region, Source: source})
		}
	}
	if len(attempts) <= 1 {
		return attempts
	}
	start := int(cfg.nextAttempt.Add(1)-1) % len(attempts)
	ordered := make([]bedrockAttempt, 0, len(attempts))
	ordered = append(ordered, attempts[start:]...)
	ordered = append(ordered, attempts[:start]...)
	return ordered
}

func (cfg *BedrockConfig) onThrottle(sourceName, region, model string) {
	if model == "" {
		return
	}
	for _, source := range cfg.Sources {
		if source.Name == sourceName && source.Bumper != nil {
			go source.Bumper.onThrottle(region, model)
			return
		}
	}
	if cfg.Bumper != nil {
		go cfg.Bumper.onThrottle(region, model)
	}
}

type bedrockStreamResult struct {
	Usage          bedrockUsage
	HaveUsage      bool
	SawMessageStop bool
	ExceptionType  string
	ReadErr        error
}

type bedrockFrame struct {
	payload       []byte
	messageType   string
	eventType     string
	exceptionType string
}

var errBedrockStreamEmpty = errors.New("bedrock stream ended before first event")

// errBedrockPeekWatchdog marks an attempt whose stream went silent long
// enough that the peek watchdog closed the body out from under it.
var errBedrockPeekWatchdog = errors.New("bedrock stream silent past the peek deadline")

// claudeFableBedrockPeekSilenceGrace is added to the forced-commit deadline
// before the watchdog closes a silent stream's body, so a stream that is
// delivering frames always force-commits between reads first and only a
// stream blocked inside Read is aborted.
var claudeFableBedrockPeekSilenceGrace = 15 * time.Second

// claudeFableBedrockPeekIdleTimeout bounds pre-content silence AFTER the
// stream's first bytes arrived. First bytes mean prefill finished, so a
// silent stream is stalled, not working; healthy silent-thinking gaps
// observed live top out near 40s, and 75s keeps ~2x margin while freeing a
// stalled non-final attempt early enough that the riding final attempt
// starts inside the client's patience (~270s observed). A var, not a const,
// so tests can shrink it.
var claudeFableBedrockPeekIdleTimeout = 75 * time.Second

// errBedrockStreamIdle marks a committed stream whose upstream sent nothing
// for claudeFableBedrockStreamIdleTimeout, so the idle watchdog closed it.
var errBedrockStreamIdle = errors.New("bedrock stream idle mid-response past the watchdog deadline")

// claudeFableBedrockStreamIdleTimeout bounds silence on a committed stream.
// Healthy Fable streams show long frame gaps (production logs: first visible
// delta at 57s while thinking), so this must sit well above those; the client
// stall detector that this watchdog preempts cancels around 300s of silence.
// Any received frame, pings included, resets the clock. A var, not a const,
// so tests can shrink it.
var claudeFableBedrockStreamIdleTimeout = 120 * time.Second

// bedrockIdleWatchdogReader closes closer when src delivers nothing for the
// active timeout, which errors the blocked Read. Two timeouts: `initial`
// applies while ZERO bytes have arrived (prefill patience: Bedrock sends its
// first frame only after prompt prefill, which runs minutes on large
// cache-miss prompts), `idle` applies once any byte has arrived (a stream
// that answered and then went silent is stalled, not prefilling). The fire
// callback re-checks recency under the mutex so a frame that arrives as the
// timer fires reschedules the deadline instead of killing a live stream.
type bedrockIdleWatchdogReader struct {
	src     io.Reader
	closer  io.Closer
	initial time.Duration
	idle    time.Duration

	mu       sync.Mutex
	timer    *time.Timer
	last     time.Time
	sawBytes bool
	fired    bool
	stopped  bool
}

func newBedrockIdleWatchdogReader(src io.Reader, closer io.Closer, initial, idle time.Duration) *bedrockIdleWatchdogReader {
	r := &bedrockIdleWatchdogReader{src: src, closer: closer, initial: initial, idle: idle, last: time.Now()}
	r.timer = time.AfterFunc(initial, r.fire)
	return r
}

func (r *bedrockIdleWatchdogReader) activeTimeout() time.Duration {
	if r.sawBytes {
		return r.idle
	}
	return r.initial
}

func (r *bedrockIdleWatchdogReader) fire() {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	timeout := r.activeTimeout()
	if idle := time.Since(r.last); idle < timeout {
		r.timer.Reset(timeout - idle)
		r.mu.Unlock()
		return
	}
	r.fired = true
	r.mu.Unlock()
	_ = r.closer.Close()
}

func (r *bedrockIdleWatchdogReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	r.mu.Lock()
	if n > 0 && !r.sawBytes {
		r.sawBytes = true
		// The pending timer was armed for the initial deadline; the first
		// bytes switch the contract to the idle deadline, so rearm now or a
		// long initial would mask every idle expiry until it elapsed.
		if !r.stopped && !r.fired {
			r.timer.Reset(r.idle)
		}
	}
	r.last = time.Now()
	fired := r.fired
	r.mu.Unlock()
	if err != nil && fired {
		return n, errBedrockStreamIdle
	}
	return n, err
}

// hasFired reports whether the watchdog closed the body, so a caller whose
// read raced the close can downgrade an apparent success.
func (r *bedrockIdleWatchdogReader) hasFired() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fired
}

// stop disarms the watchdog once the transcode loop has returned, so the
// timer cannot fire while the caller is closing the body itself.
func (r *bedrockIdleWatchdogReader) stop() {
	r.mu.Lock()
	r.stopped = true
	r.timer.Stop()
	r.mu.Unlock()
}

// claudeFableBedrockStreamAttempts bounds how many times a streaming Fable
// request is re-sent to Bedrock when the stream fails before any content has
// been forwarded to the client. Nothing was committed yet, so the request is
// safely replayable.
const claudeFableBedrockStreamAttempts = 3

type bedrockPeekOutcome int

const (
	// bedrockPeekCommit: the stream produced its first content block (or
	// finished), so it is viable and must be forwarded as-is from here on.
	bedrockPeekCommit bedrockPeekOutcome = iota
	// bedrockPeekError: an exception frame or in-band Anthropic error event
	// arrived before any content. The response is still fully replayable.
	bedrockPeekError
	// bedrockPeekReadErr: the connection died before the stream proved viable.
	bedrockPeekReadErr
)

type bedrockStreamPeek struct {
	outcome      bedrockPeekOutcome
	errorFrame   bedrockFrame
	readErr      error
	peeked       []byte
	commitReason string
	commitAt     time.Duration
}

// bedrockFrameDecision reports whether a frame decides the stream's fate.
// Errors and exceptions are failures. The first content delta, a usage delta,
// or message_stop proves the stream viable. message_start, content_block_start,
// and pings prove nothing: Bedrock regularly opens a message (and even a
// content block) and then delivers an overloaded_error, so the peek window has
// to extend past them.
// claudeFableBedrockCommitWindow bounds how long the peek keeps buffering
// early thinking deltas before committing anyway. Production depth telemetry
// (2026-08-18) put most overload sheds within the first seconds of the stream,
// all during thinking with zero visible tokens. Fable adaptive thinking often
// delivers its FIRST thinking_delta only after seconds of server-side thought,
// so a short window expires before that delta arrives and commits on it (seen
// live: committed on a late first delta, shed 743ms later). Surfaced sheds on
// 2026-08-19/20 clustered at 5-20s, mostly during thinking; twenty seconds
// buys absorption of that whole cluster. The cost is visible thinking starting
// up to this much later on thinking-heavy responses; visible text still
// commits instantly, so answers without long thinking pay nothing. Tool-input
// deltas are buffered too (since 2026-08-26): clients neither render nor
// execute a tool call before the message ends, so gating them is free and
// absorbs the stall cluster that begins seconds into tool-JSON emission. A
// var, not a const, so tests can shrink it.
var claudeFableBedrockCommitWindow = 20 * time.Second

// The peek loop otherwise runs until a decisive frame arrives, and nothing
// guarantees one ever does: a stream of pings, unparseable frames, or endless
// thinking whose window anchor never trips buffers raw bytes without bound
// while the client has received no response headers at all — silence
// indistinguishable from a hang, for as long as the client waits. Past either
// bound the stream has proved it is alive, just not classifiable, so commit
// and let any later shed surface as an in-band error event instead of
// buffering forever. Vars, not consts, so tests can shrink them.
var claudeFableBedrockPeekMaxBytes = 8 << 20
var claudeFableBedrockPeekForceCommitAfter = 120 * time.Second

// bedrockFrameDecision reports whether a frame decides the stream's fate at
// the given elapsed time since the stream opened. Errors and exceptions are
// failures. A text delta, a usage delta, or message_stop proves the stream
// viable immediately. Thinking and tool-input deltas prove it only once the
// commit window has elapsed: Bedrock regularly sheds or stalls a stream in
// the first seconds of thinking OR of tool-JSON emission (production
// 2026-08-26: four of seven watchdog aborts began idling 3-7s after an
// input_json_delta commit), and holding those frames back keeps the request
// replayable. Buffering tool input is free for the client: no client renders
// or executes a tool call before the message ends, and the typical tool-call
// turn commits on its message_delta anyway. message_start,
// content_block_start/stop, and pings prove nothing.
// sinceFirstBuffered is how long buffered-class deltas have been flowing
// (zero until the first one arrives): the commit window is anchored there,
// not at stream start, because delay only begins to cost once there is
// something to hold back. Fable regularly thinks silently for 30s+ before
// its first delta (seen live: first delta at 38.6s, shed 3s later), and a
// stream-start anchor expires the window during that silence for no benefit.
func bedrockFrameDecision(frame bedrockFrame, sinceFirstBuffered time.Duration) (decisive, failure bool, reason string) {
	if strings.EqualFold(frame.messageType, "exception") {
		return true, true, "exception"
	}
	var ev struct {
		Type  string `json:"type"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			Thinking    string `json:"thinking"`
		} `json:"delta"`
	}
	_ = json.Unmarshal(frame.payload, &ev)
	switch ev.Type {
	case "error":
		return true, true, "error"
	case "message_delta", "message_stop":
		return true, false, ev.Type
	case "content_block_delta":
		switch ev.Delta.Type {
		case "thinking_delta", "signature_delta", "input_json_delta":
			if sinceFirstBuffered >= claudeFableBedrockCommitWindow {
				return true, false, "window_expired_" + ev.Delta.Type
			}
			return false, false, ""
		case "text_delta":
			// Bedrock primes every block with an EMPTY first delta (seen in
			// transcripts: text_delta{text:""}). An empty delta carries
			// nothing the client needs, so it proves nothing; committing on
			// it made a shed one frame later non-replayable. Only a delta
			// with payload commits.
			if ev.Delta.Text != "" {
				return true, false, "text_delta"
			}
			return false, false, ""
		}
		// Unknown delta types arrive with new betas and are usually invisible
		// (redacted_thinking-shaped). Committing on one instantly reintroduces
		// the unreplayable post-commit shed the window exists to absorb, so
		// gate them like thinking deltas; the peek's forced deadline backstops
		// a stream made only of them.
		if sinceFirstBuffered >= claudeFableBedrockCommitWindow {
			return true, false, "window_expired_delta_" + ev.Delta.Type
		}
		return false, false, ""
	}
	return false, false, ""
}

// peekBedrockStreamUntilCommit buffers a Bedrock response stream until it
// either proves viable (first content block) or fails (error, exception, or
// read error). Nothing is forwarded to the client while peeking, so a failed
// stream can be retried without duplicating output. peeked carries every raw
// byte read, for replay into the transcoder on commit.
// bedrockFrameIsBufferedDelta reports whether a frame belongs to the class
// the commit window buffers (and therefore anchors it): every
// content_block_delta except live-rendered text.
func bedrockFrameIsBufferedDelta(frame bedrockFrame) bool {
	var ev struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
		} `json:"delta"`
	}
	if json.Unmarshal(frame.payload, &ev) != nil || ev.Type != "content_block_delta" {
		return false
	}
	// thinking_delta, signature_delta, input_json_delta, and any future
	// invisible delta type: all are buffered by the commit window, so all
	// anchor it. Only text_delta streams to the client live.
	return ev.Delta.Type != "text_delta"
}

func peekBedrockStreamUntilCommit(src io.Reader) bedrockStreamPeek {
	var scanner bedrockFrameScanner
	var peeked bytes.Buffer
	started := time.Now()
	decided := false
	failed := false
	commitReason := ""
	var commitAt time.Duration
	var firstBufferedAt time.Time
	var errorFrame bedrockFrame
	emit := func(frame bedrockFrame) {
		if decided {
			return
		}
		if firstBufferedAt.IsZero() && bedrockFrameIsBufferedDelta(frame) {
			firstBufferedAt = time.Now()
		}
		var sinceFirstBuffered time.Duration
		if !firstBufferedAt.IsZero() {
			sinceFirstBuffered = time.Since(firstBufferedAt)
		}
		decisive, failure, reason := bedrockFrameDecision(frame, sinceFirstBuffered)
		if !decisive {
			return
		}
		decided = true
		failed = failure
		commitReason = reason
		commitAt = time.Since(started)
		if failure {
			errorFrame = frame
		}
	}
	buf := make([]byte, 32*1024)
	for !decided {
		n, readErr := src.Read(buf)
		if n > 0 {
			peeked.Write(buf[:n])
			scanner.feed(buf[:n], emit)
		}
		if decided {
			break
		}
		if readErr != nil {
			if readErr == io.EOF {
				readErr = errBedrockStreamEmpty
			}
			return bedrockStreamPeek{outcome: bedrockPeekReadErr, readErr: readErr, peeked: peeked.Bytes()}
		}
		if peeked.Len() >= claudeFableBedrockPeekMaxBytes {
			decided = true
			commitReason = "forced_buffer_cap"
			commitAt = time.Since(started)
			break
		}
		if time.Since(started) >= claudeFableBedrockPeekForceCommitAfter {
			decided = true
			commitReason = "forced_deadline"
			commitAt = time.Since(started)
			break
		}
	}
	if failed {
		return bedrockStreamPeek{outcome: bedrockPeekError, errorFrame: errorFrame, peeked: peeked.Bytes()}
	}
	return bedrockStreamPeek{outcome: bedrockPeekCommit, peeked: peeked.Bytes(), commitReason: commitReason, commitAt: commitAt}
}

// bedrockPeekFailureRetryable reports whether a fresh attempt may succeed:
// transport errors, throttles, and capacity errors qualify; validation and
// auth errors fail identically everywhere and do not.
func bedrockPeekFailureRetryable(peek bedrockStreamPeek) bool {
	if peek.outcome == bedrockPeekReadErr {
		return true
	}
	frame := peek.errorFrame
	if strings.EqualFold(frame.messageType, "exception") {
		t := strings.ToLower(frame.exceptionType)
		return strings.Contains(t, "throttl") ||
			strings.Contains(t, "serviceunavailable") ||
			strings.Contains(t, "internalserver") ||
			strings.Contains(t, "modelnotready") ||
			strings.Contains(t, "timeout")
	}
	var ev struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(frame.payload, &ev) == nil && ev.Type == "error" {
		switch ev.Error.Type {
		case "overloaded_error", "api_error", "rate_limit_error", "internal_server_error":
			return true
		}
	}
	return false
}

// bedrockRetryBackoff spaces retries (200ms, then 400ms) so a momentary
// capacity burst is not hit three times within the same millisecond. Returns
// false when the caller's context is done, in which case retrying is moot.
func bedrockRetryBackoff(ctx context.Context, attempt int) bool {
	timer := time.NewTimer(time.Duration(attempt) * 200 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// transcodeBedrockToSSE converts a Bedrock invoke-with-response-stream body (AWS
// event-stream framing wrapping Anthropic event JSON) into Anthropic Messages
// SSE, which is what a Claude client on the OAuth path expects. It also extracts
// token usage for cost tracking and reports stream anomalies. w may be an
// http.ResponseWriter (flushed per event) or a plain writer like an io.Pipe
// (each Write hands the event to the reader directly).
func transcodeBedrockToSSE(w io.Writer, src io.Reader, logger *slog.Logger, bedrockSource, region string) bedrockStreamResult {
	return transcodeBedrockToSSESince(w, src, logger, bedrockSource, region, time.Now(), "", 0)
}

// transcodeBedrockToSSESince is transcodeBedrockToSSE with an explicit stream
// start time, so elapsed_ms in the death logs is stream-relative (including
// time spent in the pre-commit peek) rather than transcode-relative.
func transcodeBedrockToSSESince(w io.Writer, src io.Reader, logger *slog.Logger, bedrockSource, region string, started time.Time, commitReason string, commitAt time.Duration) bedrockStreamResult {
	flusher, _ := w.(http.Flusher)
	var scanner bedrockFrameScanner
	var result bedrockStreamResult
	eventsForwarded := 0
	// output_tokens_so_far is always 0 mid-stream (usage arrives in the final
	// message_delta), so the death logs also count visible progress directly.
	contentDeltasForwarded := 0
	sawTextBlock := false
	exceptionHandled := false
	finalize := func() bedrockStreamResult {
		result.HaveUsage = result.HaveUsage && !result.Usage.empty()
		return result
	}
	writeErrorEvent := func(errorType, message string) {
		payload, _ := json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]string{
				"type":    errorType,
				"message": message,
			},
		})
		_, _ = w.Write([]byte("event: error\ndata: "))
		_, _ = w.Write(payload)
		_, _ = w.Write([]byte("\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
	emit := func(frame bedrockFrame) {
		if exceptionHandled {
			return
		}
		if strings.EqualFold(frame.messageType, "exception") {
			result.ExceptionType = frame.exceptionType
			exceptionHandled = true
			if logger != nil {
				logger.Warn("claude-fable bedrock mid-stream exception", "exception_type", frame.exceptionType, "message", bedrockLogPreview(frame.payload), "bedrock_source", bedrockSource, "region", region, "saw_message_stop", result.SawMessageStop)
			}
			if strings.Contains(strings.ToLower(frame.exceptionType), "throttl") {
				writeErrorEvent("overloaded_error", "Bedrock throttled mid-stream")
			} else {
				message := "Bedrock stream error"
				if frame.exceptionType != "" {
					message += ": " + frame.exceptionType
				}
				writeErrorEvent("api_error", message)
			}
			return
		}
		inner := frame.payload
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Usage bedrockUsage `json:"usage"`
			} `json:"message"`
			Usage bedrockUsage `json:"usage"`
		}
		_ = json.Unmarshal(inner, &ev)
		switch ev.Type {
		case "message_start":
			result.Usage.InputTokens = ev.Message.Usage.InputTokens
			result.Usage.CacheReadTokens = ev.Message.Usage.CacheReadTokens
			result.Usage.CacheWriteTokens = ev.Message.Usage.CacheWriteTokens
			// Without the per-TTL split the cost log prices every write at
			// the 5m rate, understating 1h writes by 1.6x.
			result.Usage.CacheCreation = ev.Message.Usage.CacheCreation
			result.HaveUsage = true
		case "message_delta":
			if ev.Usage.OutputTokens > 0 {
				result.Usage.OutputTokens = ev.Usage.OutputTokens
				result.HaveUsage = true
			}
		case "message_stop":
			result.SawMessageStop = true
		case "content_block_delta":
			contentDeltasForwarded++
		case "content_block_start":
			var block struct {
				ContentBlock struct {
					Type string `json:"type"`
				} `json:"content_block"`
			}
			if json.Unmarshal(inner, &block) == nil && block.ContentBlock.Type == "text" {
				sawTextBlock = true
			}
		case "error":
			if logger != nil {
				// Depth telemetry: how far into the committed stream the
				// upstream died. events_forwarded and saw_text_block separate
				// admission-time sheds (retryable pre-commit, handled by the
				// peek) from mid-generation sheds (not replayable).
				logger.Warn("claude-fable bedrock in-band error", "exception_type", "", "message", bedrockLogPreview(inner), "bedrock_source", bedrockSource, "region", region, "saw_message_stop", result.SawMessageStop, "events_forwarded", eventsForwarded, "content_deltas_forwarded", contentDeltasForwarded, "saw_text_block", sawTextBlock, "output_tokens_so_far", result.Usage.OutputTokens, "elapsed_ms", time.Since(started).Milliseconds(), "commit_reason", commitReason, "commit_at_ms", commitAt.Milliseconds())
			}
		}
		eventType := ev.Type
		if eventType == "" {
			eventType = "message"
		}
		_, _ = w.Write([]byte("event: " + eventType + "\ndata: "))
		_, _ = w.Write(inner)
		_, _ = w.Write([]byte("\n\n"))
		eventsForwarded++
		if flusher != nil {
			flusher.Flush()
		}
	}
	buf := make([]byte, 32*1024)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			scanner.feed(buf[:n], emit)
			if exceptionHandled {
				return finalize()
			}
		}
		if readErr != nil {
			if !result.SawMessageStop && !exceptionHandled {
				result.ReadErr = readErr
				logMessage := "claude-fable bedrock stream truncated"
				if errors.Is(readErr, errBedrockStreamIdle) {
					logMessage = "claude-fable bedrock stream idle, aborted by watchdog"
				}
				if logger != nil {
					logger.Error(logMessage, "bedrock_source", bedrockSource, "region", region, "read_err", readErr, "saw_message_stop", result.SawMessageStop, "exception_type", "", "message", "", "events_forwarded", eventsForwarded, "content_deltas_forwarded", contentDeltasForwarded, "saw_text_block", sawTextBlock, "output_tokens_so_far", result.Usage.OutputTokens, "elapsed_ms", time.Since(started).Milliseconds(), "commit_reason", commitReason, "commit_at_ms", commitAt.Milliseconds())
				}
				if errors.Is(readErr, errBedrockStreamIdle) {
					// overloaded_error is the error type clients already retry
					// with backoff; the request is replayable from their side.
					writeErrorEvent("overloaded_error", "Bedrock stream went idle mid-response")
				} else {
					writeErrorEvent("api_error", "Bedrock stream interrupted")
				}
			} else if readErr != io.EOF {
				result.ReadErr = readErr
			}
			break
		}
	}
	return finalize()
}

// bedrockFrameScanner parses AWS event-stream frames incrementally and yields
// each frame's decoded inner payload (the Anthropic event JSON) with AWS
// event-stream metadata. Exception frames carry a raw JSON error payload instead
// of a {"bytes": "..."} wrapper.
type bedrockFrameScanner struct{ buf []byte }

func (s *bedrockFrameScanner) feed(data []byte, emit func(bedrockFrame)) {
	s.buf = append(s.buf, data...)
	for {
		if len(s.buf) < 12 {
			return
		}
		total := int(binary.BigEndian.Uint32(s.buf[0:4]))
		headersLen := int(binary.BigEndian.Uint32(s.buf[4:8]))
		if total < 16 || total > 64<<20 {
			s.buf = nil
			return
		}
		if len(s.buf) < total {
			return
		}
		payloadStart := 12 + headersLen
		payloadEnd := total - 4
		headers := map[string]string(nil)
		if headersLen >= 0 && 12+headersLen <= payloadEnd {
			headers, _ = parseBedrockEventHeaders(s.buf[12 : 12+headersLen])
		} else {
			payloadStart = 12
		}
		if payloadStart <= payloadEnd && payloadEnd <= len(s.buf) {
			frame := bedrockFrame{
				messageType:   headers[":message-type"],
				eventType:     headers[":event-type"],
				exceptionType: headers[":exception-type"],
			}
			payload := s.buf[payloadStart:payloadEnd]
			if strings.EqualFold(frame.messageType, "exception") {
				frame.payload = append([]byte(nil), payload...)
				emit(frame)
				s.buf = s.buf[total:]
				continue
			}
			var wrap struct {
				Bytes string `json:"bytes"`
			}
			if json.Unmarshal(payload, &wrap) == nil && wrap.Bytes != "" {
				if decoded, err := base64.StdEncoding.DecodeString(wrap.Bytes); err == nil {
					frame.payload = decoded
					emit(frame)
				}
			}
		}
		s.buf = s.buf[total:]
	}
}

func parseBedrockEventHeaders(headers []byte) (map[string]string, bool) {
	out := make(map[string]string)
	for len(headers) > 0 {
		if len(headers) < 2 {
			return nil, false
		}
		nameLen := int(headers[0])
		headers = headers[1:]
		if len(headers) < nameLen+1 {
			return nil, false
		}
		name := string(headers[:nameLen])
		headers = headers[nameLen:]
		valueType := headers[0]
		headers = headers[1:]
		switch valueType {
		case 0, 1:
		case 2:
			if len(headers) < 1 {
				return nil, false
			}
			headers = headers[1:]
		case 3:
			if len(headers) < 2 {
				return nil, false
			}
			headers = headers[2:]
		case 4:
			if len(headers) < 4 {
				return nil, false
			}
			headers = headers[4:]
		case 5, 8:
			if len(headers) < 8 {
				return nil, false
			}
			headers = headers[8:]
		case 6, 7:
			if len(headers) < 2 {
				return nil, false
			}
			valueLen := int(binary.BigEndian.Uint16(headers[:2]))
			headers = headers[2:]
			if len(headers) < valueLen {
				return nil, false
			}
			if valueType == 7 {
				out[name] = string(headers[:valueLen])
			}
			headers = headers[valueLen:]
		case 9:
			if len(headers) < 16 {
				return nil, false
			}
			headers = headers[16:]
		default:
			return nil, false
		}
	}
	return out, true
}

func bedrockLogPreview(payload []byte) string {
	if len(payload) > 256 {
		payload = payload[:256]
	}
	return string(payload)
}

func (s Server) handleBedrockCost(w http.ResponseWriter, r *http.Request) {
	path := ""
	if s.Bedrock != nil {
		path = s.Bedrock.CostLogPath
	}
	writeJSON(w, summarizeBedrockCost(path))
}

func bedrockGatewayTokenOK(r *http.Request, token string) bool {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[len("bearer "):]) == token
	}
	return auth == token
}

// copyBedrockRequestHeaders forwards client headers that Bedrock needs while
// dropping hop-by-hop headers, the client's own Authorization (we re-sign), and
// any pre-existing AWS signing headers.
func copyBedrockRequestHeaders(dst, src http.Header) {
	for key, values := range src {
		lower := strings.ToLower(key)
		if isHopByHopHeader(key) || lower == "authorization" || lower == "host" || lower == "content-length" {
			continue
		}
		if strings.HasPrefix(lower, "x-amz-") {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
	if dst.Get("Content-Type") == "" {
		dst.Set("Content-Type", "application/json")
	}
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// bedrockModelFromPath extracts the model id from /model/<id>/invoke[...] paths.
func bedrockModelFromPath(path string) string {
	trimmed := strings.TrimPrefix(path, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) >= 2 && parts[0] == "model" {
		return parts[1]
	}
	return ""
}

// streamBedrockResponse forwards the upstream response to the client while
// extracting token usage. Event-stream responses are parsed frame-by-frame as
// they flow; non-streaming JSON responses are buffered (they are small) and
// parsed directly. Usage extraction never blocks or corrupts the forwarded
// bytes.
func (s Server) streamBedrockResponse(w http.ResponseWriter, resp *http.Response) (bedrockUsage, bool) {
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(contentType, "eventstream") {
		parser := newBedrockStreamUsageWriter()
		flushingCopy(w, resp.Body, parser)
		return parser.Usage()
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, replayablePostMaxBodyBytes))
	if err == nil && len(body) > 0 {
		_, _ = w.Write(body)
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return parseBedrockInvokeUsage(body)
}

// flushingCopy streams src to the client, flushing after each chunk, and
// mirrors the bytes to sink (used for usage parsing) when sink is non-nil.
func flushingCopy(w http.ResponseWriter, src io.Reader, sink io.Writer) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if sink != nil {
				_, _ = sink.Write(buf[:n])
			}
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

func isHopByHopHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "proxy-connection", "keep-alive", "transfer-encoding",
		"te", "trailer", "upgrade", "proxy-authenticate", "proxy-authorization":
		return true
	}
	return false
}
