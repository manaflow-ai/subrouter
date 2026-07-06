package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
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
	started := time.Now()
	resp, sourceName, region, err := s.signAndForwardBedrock(ctx, http.MethodPost, path, newBody)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		if s.Logger != nil {
			s.Logger.Warn("bedrock throttled", "model", bedrockFableModelID, "path", path, "bedrock_source", sourceName, "region", region)
		}
		cfg.onThrottle(sourceName, region, bedrockFableModelID)
	}

	if stream && resp.StatusCode == http.StatusOK {
		pr, pw := io.Pipe()
		go func() {
			usage, haveUsage := transcodeBedrockToSSE(pw, resp.Body)
			_ = resp.Body.Close()
			_ = pw.Close()
			s.recordClaudeFableBedrockCost(started, region, http.StatusOK, usage, haveUsage)
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

// transcodeBedrockToSSE converts a Bedrock invoke-with-response-stream body (AWS
// event-stream framing wrapping Anthropic event JSON) into Anthropic Messages
// SSE, which is what a Claude client on the OAuth path expects. It also extracts
// token usage for cost tracking. w may be an http.ResponseWriter (flushed per
// event) or a plain writer like an io.Pipe (each Write hands the event to the
// reader directly).
func transcodeBedrockToSSE(w io.Writer, src io.Reader) (bedrockUsage, bool) {
	flusher, _ := w.(http.Flusher)
	var scanner bedrockFrameScanner
	var usage bedrockUsage
	var got bool
	emit := func(inner []byte) {
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
			usage.InputTokens = ev.Message.Usage.InputTokens
			usage.CacheReadTokens = ev.Message.Usage.CacheReadTokens
			usage.CacheWriteTokens = ev.Message.Usage.CacheWriteTokens
			got = true
		case "message_delta":
			if ev.Usage.OutputTokens > 0 {
				usage.OutputTokens = ev.Usage.OutputTokens
				got = true
			}
		}
		eventType := ev.Type
		if eventType == "" {
			eventType = "message"
		}
		_, _ = w.Write([]byte("event: " + eventType + "\ndata: "))
		_, _ = w.Write(inner)
		_, _ = w.Write([]byte("\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
	buf := make([]byte, 32*1024)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			scanner.feed(buf[:n], emit)
		}
		if readErr != nil {
			break
		}
	}
	return usage, got && !usage.empty()
}

// bedrockFrameScanner parses AWS event-stream frames incrementally and yields
// each frame's decoded inner payload (the Anthropic event JSON).
type bedrockFrameScanner struct{ buf []byte }

func (s *bedrockFrameScanner) feed(data []byte, emit func([]byte)) {
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
		if payloadStart <= payloadEnd && payloadEnd <= len(s.buf) {
			var wrap struct {
				Bytes string `json:"bytes"`
			}
			if json.Unmarshal(s.buf[payloadStart:payloadEnd], &wrap) == nil && wrap.Bytes != "" {
				if decoded, err := base64.StdEncoding.DecodeString(wrap.Bytes); err == nil {
					emit(decoded)
				}
			}
		}
		s.buf = s.buf[total:]
	}
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
