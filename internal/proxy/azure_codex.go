package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// AzureCodexEndpoint is one Azure OpenAI resource that can serve Codex
// Responses traffic. BaseURL is the v1 surface documented for Codex CLI
// (https://<resource>.openai.azure.com/openai/v1), which takes the same request
// body as api.openai.com with model set to the deployment name.
type AzureCodexEndpoint struct {
	// Name labels the endpoint in logs (usually the region or resource name).
	Name string
	// BaseURL is the Azure OpenAI v1 base, e.g.
	// https://my-resource.openai.azure.com/openai/v1.
	BaseURL *url.URL
	// APIKey authenticates the call. Sent as both api-key and Authorization
	// bearer because the v1 surface accepts either.
	APIKey string
	// Deployments maps a requested model to the Azure deployment name. A model
	// with no entry uses its own name, which is how Foundry deployments are
	// named by default.
	Deployments map[string]string
}

// AzureCodexConfig is the Codex fallback route. It serves a Codex Responses
// request from Azure OpenAI after the subscription pool has failed, and pins
// the session to Azure afterwards so the follow-up turns keep hitting the same
// prompt cache instead of flapping between two providers.
type AzureCodexConfig struct {
	Endpoints []AzureCodexEndpoint
	// Transport is the outbound RoundTripper. Nil uses the server transport.
	Transport http.RoundTripper
	// CostLogPath is the JSONL file each served request is priced into. Empty
	// disables cost accounting. Azure is metered, unlike the subscription pool,
	// so a fallback that silently spends money needs the same per-request
	// record the Bedrock gateway keeps.
	CostLogPath string
}

func (c *AzureCodexConfig) configured() bool {
	if c == nil {
		return false
	}
	for _, endpoint := range c.Endpoints {
		if endpoint.BaseURL != nil && endpoint.APIKey != "" {
			return true
		}
	}
	return false
}

// deployment resolves the Azure deployment name for a requested model: an exact
// mapping, then the "*" default, then the model's own name. The default matters
// because Azure lags the ChatGPT model list, so a Codex release the fallback has
// never heard of would otherwise 404 on the one path meant to rescue it.
func (e AzureCodexEndpoint) deployment(model string) string {
	if mapped, ok := e.Deployments[model]; ok && mapped != "" {
		return mapped
	}
	if fallback, ok := e.Deployments[AzureCodexDefaultDeploymentKey]; ok && fallback != "" {
		return fallback
	}
	return model
}

// AzureCodexDefaultDeploymentKey maps every unlisted model onto one deployment.
const AzureCodexDefaultDeploymentKey = "*"

// azureCodexForceHeader makes a request skip the pool and go straight to Azure.
// It is how `sr az codex` and `sr az test` exercise the fallback without
// waiting for the pool to fail.
const azureCodexForceHeader = "X-Subrouter-Azure"

// azureCodexForced reports whether the caller demanded the Azure route.
func azureCodexForced(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.Header.Get(azureCodexForceHeader))) {
	case "force", "1", "true", "yes", "only":
		return true
	}
	return false
}

const (
	// azureCodexPoolRetryBudget is how many retries the Codex pool gets before
	// the request is handed to Azure. Attempt 1 plus this many retries, across
	// account failover (429/usage limit) and transport-level retries (5xx,
	// connection failures) together, so a request cannot spend 5 retries per
	// layer.
	azureCodexPoolRetryBudget = 5
	// azureCodexStickyTTL is how long a session stays pinned to Azure after a
	// successful fallback, refreshed on every request. It matches the 30-minute
	// minimum lifetime Azure gives a cached prompt prefix: pinning for less
	// would send the next turn back to the pool with a cold cache on both
	// sides, pinning for much longer would keep a session off the subscription
	// pool long after the incident that pushed it to Azure.
	azureCodexStickyTTL = 30 * time.Minute
	// azureCodexMaxErrorBodyBytes bounds how much of an Azure error body is
	// read for a log line. Enough for the message field, small enough that a
	// misbehaving endpoint cannot flood memory.
	azureCodexMaxErrorBodyBytes = 512
)

// attemptBudget bounds the retries a single client request may spend on the
// pool before the fallback takes over. Both retry transports draw from the same
// budget, so nested retry loops cannot multiply into 5 retries per layer.
type attemptBudget struct {
	remaining atomic.Int64
}

func newAttemptBudget(retries int) *attemptBudget {
	budget := &attemptBudget{}
	budget.remaining.Store(int64(retries))
	return budget
}

// consume claims one retry. It returns false once the budget is spent, which
// tells the caller to stop retrying and let the fallback run. A nil budget is
// unbounded, which is the behaviour when no fallback is configured.
func (b *attemptBudget) consume() bool {
	if b == nil {
		return true
	}
	for {
		remaining := b.remaining.Load()
		if remaining <= 0 {
			return false
		}
		if b.remaining.CompareAndSwap(remaining, remaining-1) {
			return true
		}
	}
}

// azureCodexSticky pins a Codex session to an Azure endpoint. Prompt caching is
// per-deployment and keyed on an identical prefix, so a session that has fallen
// back must keep going to the same place: alternating providers turn by turn
// re-uploads the whole conversation as a cache miss every time.
type azureCodexSticky struct {
	mu      sync.Mutex
	entries map[string]azureCodexStickyEntry
	now     func() time.Time
}

type azureCodexStickyEntry struct {
	endpoint  int
	expiresAt time.Time
}

func newAzureCodexSticky() *azureCodexSticky {
	return &azureCodexSticky{entries: map[string]azureCodexStickyEntry{}}
}

func (s *azureCodexSticky) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// lookup returns the pinned endpoint index for a session and extends the pin,
// because the pin exists to keep a live conversation on one cache.
func (s *azureCodexSticky) lookup(key string) (int, bool) {
	if s == nil || key == "" {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok {
		return 0, false
	}
	now := s.clock()
	if !entry.expiresAt.After(now) {
		delete(s.entries, key)
		return 0, false
	}
	entry.expiresAt = now.Add(azureCodexStickyTTL)
	s.entries[key] = entry
	return entry.endpoint, true
}

func (s *azureCodexSticky) pin(key string, endpoint int) {
	if s == nil || key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	s.sweepLocked(now)
	s.entries[key] = azureCodexStickyEntry{endpoint: endpoint, expiresAt: now.Add(azureCodexStickyTTL)}
}

func (s *azureCodexSticky) unpin(key string) {
	if s == nil || key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key)
}

// sweepLocked drops expired pins. Sessions end without telling the proxy, so
// without this the map grows for the life of the process.
func (s *azureCodexSticky) sweepLocked(now time.Time) {
	for key, entry := range s.entries {
		if !entry.expiresAt.After(now) {
			delete(s.entries, key)
		}
	}
}

func azureCodexSessionKeyFor(agentType, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	return agentType + "\x00" + sessionID
}

// azureCodexRequest reports whether this Codex request can be served by Azure.
// Only the Responses endpoint qualifies: /responses/compact is a ChatGPT
// backend endpoint with no Azure equivalent, and everything else on the Codex
// surface (catalog, alpha/search) is ChatGPT-specific.
func azureCodexRequest(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	_, ok := azureCodexPath(path)
	return ok
}

// azureCodexPath maps a Codex request path onto the Azure v1 surface.
func azureCodexPath(path string) (string, bool) {
	switch path {
	case "/responses", "/v1/responses", "/backend-api/codex/responses":
		return "/responses", true
	default:
		return "", false
	}
}

// azureCodexEndpointIndex spreads sessions across configured endpoints while
// keeping one session on one endpoint, since a prompt cache lives inside a
// single Azure deployment.
func azureCodexEndpointIndex(sessionKey string, count int) int {
	if count <= 1 || sessionKey == "" {
		return 0
	}
	sum := sha256.Sum256([]byte(sessionKey))
	return int(sum[0]) % count
}

// azureCodexCacheKey keeps the prompt_cache_key the client already sent, and
// otherwise derives a stable one from the session so every turn of the same
// conversation routes to the same cached prefix. The session id is hashed
// rather than forwarded: it is internal routing state, not something to hand to
// a third-party provider.
func azureCodexCacheKey(existing, sessionKey string) string {
	if existing != "" {
		return existing
	}
	if sessionKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sessionKey))
	return "sr-" + hex.EncodeToString(sum[:8])
}

// azureCodexChatGPTOnlyFields are fields Codex sends to the ChatGPT backend
// that the Responses API rejects outright ("Unknown parameter: 'session_id'").
// Every Codex turn carries session_id, so without this the fallback answers a
// pool outage with a 400 from a second provider.
var azureCodexChatGPTOnlyFields = []string{"session_id", "conversation_id", "thread_id"}

// azureCodexPayload rewrites a Codex Responses body for Azure: model becomes the
// deployment name, prompt_cache_key is filled in when the client did not send
// one, and ChatGPT-backend-only fields are dropped. Everything else is
// untouched, because the Azure v1 surface takes the same Responses payload
// Codex already sends to api.openai.com.
func azureCodexPayload(body []byte, deployment, cacheKey string) (map[string]json.RawMessage, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode responses body: %w", err)
	}
	if payload == nil {
		return nil, errors.New("empty responses body")
	}
	encodedModel, err := json.Marshal(deployment)
	if err != nil {
		return nil, err
	}
	payload["model"] = encodedModel
	if key := azureCodexCacheKey(azureCodexStringField(payload, "prompt_cache_key"), cacheKey); key != "" {
		encodedKey, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		payload["prompt_cache_key"] = encodedKey
	}
	for _, field := range azureCodexChatGPTOnlyFields {
		delete(payload, field)
	}
	return payload, nil
}

// azureCodexBody is azureCodexPayload encoded.
func azureCodexBody(body []byte, deployment, cacheKey string) ([]byte, error) {
	payload, err := azureCodexPayload(body, deployment, cacheKey)
	if err != nil {
		return nil, err
	}
	return json.Marshal(payload)
}

// azureCodexRejectedField reads the field an Azure 400 names as the problem.
// Codex ships ahead of the model versions Azure hosts, so a live Codex body
// carries fields and values a slightly older model refuses ("Unknown parameter:
// 'session_id'", "'all_turns' is not supported with gpt-5.3-codex"). The error
// names exactly which field to drop, so retrying without it beats failing the
// one request the pool already refused, and beats guessing the full list in
// advance. Dropping the field restores the provider's default for it.
func azureCodexRejectedField(body []byte) string {
	var payload struct {
		Error struct {
			Code  string `json:"code"`
			Param string `json:"param"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	switch payload.Error.Code {
	case "unknown_parameter", "unsupported_parameter", "unsupported_value":
	default:
		return ""
	}
	param := strings.TrimSpace(payload.Error.Param)
	// Array paths are not dropped: an element of the conversation is content,
	// not a setting, and removing it would change what the model is asked.
	if param == "" || strings.ContainsAny(param, "[]") {
		return ""
	}
	return param
}

// azureCodexDropField removes a dotted field path from a decoded body. It
// reports false when the path does not exist or does not run through plain
// objects, which is the caller's signal to stop retrying.
func azureCodexDropField(payload map[string]json.RawMessage, path string) bool {
	name, rest, nested := strings.Cut(path, ".")
	raw, ok := payload[name]
	if !ok {
		return false
	}
	if !nested {
		delete(payload, name)
		return true
	}
	var child map[string]json.RawMessage
	if json.Unmarshal(raw, &child) != nil || child == nil {
		return false
	}
	if !azureCodexDropField(child, rest) {
		return false
	}
	encoded, err := json.Marshal(child)
	if err != nil {
		return false
	}
	payload[name] = encoded
	return true
}

func azureCodexStringField(payload map[string]json.RawMessage, field string) string {
	raw, ok := payload[field]
	if !ok {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

// azureCodexModel reads the model from a Responses body. The routing model
// header can be missing on a raw request, and the deployment lookup needs the
// model the client actually asked for.
func azureCodexModel(body []byte) string {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	return azureCodexStringField(payload, "model")
}

// azureCodexResponse sends one Codex Responses body to Azure. It starts at the
// preferred endpoint (the session's pin, or its hashed home) and walks the
// remaining endpoints only when one fails outright, so a healthy session never
// migrates off the endpoint holding its cache. The returned index is the
// endpoint that answered.
func (s Server) azureCodexResponse(
	req *http.Request,
	body []byte,
	sessionKey string,
	preferred int,
	reason string,
) (*http.Response, int, bool) {
	if !s.AzureCodex.configured() {
		return nil, 0, false
	}
	path, ok := azureCodexPath(req.URL.Path)
	if !ok {
		return nil, 0, false
	}
	endpoints := s.AzureCodex.Endpoints
	// Codex may compress the request body (zstd). Azure takes plain JSON, and
	// the body has to be parsed anyway to rewrite the model, so decode once
	// here and send the decoded form.
	decoded, err := decodedJSONRequestBody(body, req.Header.Get("Content-Encoding"), s.azureCodexMaxBodyBytes())
	if err != nil {
		if s.Logger != nil {
			s.Logger.Warn("azure codex fallback cannot decode the request body", "error", err)
		}
		return nil, 0, false
	}
	model := azureCodexModel(decoded)
	if model == "" {
		return nil, 0, false
	}
	if preferred < 0 || preferred >= len(endpoints) {
		preferred = azureCodexEndpointIndex(sessionKey, len(endpoints))
	}
	for offset := range endpoints {
		index := (preferred + offset) % len(endpoints)
		endpoint := endpoints[index]
		if endpoint.BaseURL == nil || endpoint.APIKey == "" {
			continue
		}
		response, err := s.azureCodexEndpointResponse(req, endpoint, path, decoded, sessionKey, model)
		if err != nil {
			if req.Context().Err() != nil {
				return nil, 0, false
			}
			if s.Logger != nil {
				s.Logger.Error("azure codex fallback request failed", "endpoint", endpoint.Name, "model", model, "error", err)
			}
			continue
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			response.Body = newAzureCodexCostBody(response.Body, s.AzureCodex.CostLogPath, azureCodexCostRecord{
				Endpoint:   endpoint.Name,
				Model:      model,
				Deployment: endpoint.deployment(model),
				Reason:     reason,
				Status:     response.StatusCode,
			}, nil)
			return response, index, true
		}
		if s.Logger != nil {
			s.Logger.Warn("azure codex fallback endpoint returned an error",
				"endpoint", endpoint.Name,
				"model", model,
				"status", response.StatusCode,
				"body", azureCodexErrorSummary(response))
		}
		_ = response.Body.Close()
	}
	return nil, 0, false
}

// azureCodexEndpointResponse builds and sends the Azure request. Client headers
// are not forwarded: they carry ChatGPT session and account identity that means
// nothing to Azure, and the OAuth Authorization header must never leave for a
// third-party host.
func (s Server) azureCodexEndpointResponse(
	req *http.Request,
	endpoint AzureCodexEndpoint,
	path string,
	body []byte,
	sessionKey string,
	model string,
) (*http.Response, error) {
	payload, err := azureCodexPayload(body, endpoint.deployment(model), sessionKey)
	if err != nil {
		return nil, err
	}
	target := *endpoint.BaseURL
	target.Path = strings.TrimRight(endpoint.BaseURL.Path, "/") + path
	transport := s.AzureCodex.Transport
	if transport == nil {
		transport = s.Transport
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	for attempt := 0; ; attempt++ {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		outReq, err := http.NewRequestWithContext(req.Context(), http.MethodPost, target.String(), bytes.NewReader(encoded))
		if err != nil {
			return nil, err
		}
		outReq.Header.Set("Content-Type", "application/json")
		if accept := req.Header.Get("Accept"); accept != "" {
			outReq.Header.Set("Accept", accept)
		}
		outReq.Header.Set("Api-Key", endpoint.APIKey)
		outReq.Header.Set("Authorization", "Bearer "+endpoint.APIKey)
		outReq.Host = target.Host
		outReq.ContentLength = int64(len(encoded))

		response, err := transport.RoundTrip(outReq)
		if err != nil || response.StatusCode != http.StatusBadRequest || attempt >= azureCodexUnknownParamRetries {
			return response, err
		}
		// Azure names the field it does not know. Drop that one field and send
		// the request again rather than failing the request the pool already
		// refused.
		errorBody, readErr := io.ReadAll(io.LimitReader(response.Body, azureCodexMaxErrorBodyBytes))
		_ = response.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		rejected := azureCodexRejectedField(errorBody)
		if rejected == "" || !azureCodexDropField(payload, rejected) {
			return azureCodexReplayedResponse(response, errorBody), nil
		}
		if s.Logger != nil {
			s.Logger.Warn("azure codex rejected a field; retrying without it",
				"endpoint", endpoint.Name, "model", model, "field", rejected)
		}
	}
}

// azureCodexUnknownParamRetries bounds how many unknown fields are stripped
// before the rejection is returned as-is. Small: a body that needs more than a
// few is not a drifting field name, it is the wrong endpoint.
const azureCodexUnknownParamRetries = 3

// azureCodexReplayedResponse puts an already-read error body back on the
// response so the caller can log and close it like any other.
func azureCodexReplayedResponse(response *http.Response, body []byte) *http.Response {
	response.Body = io.NopCloser(bytes.NewReader(body))
	return response
}

// azureCodexMaxBodyBytes bounds a decoded request body, falling back to the
// replay buffer size when the server sets no explicit limit.
func (s Server) azureCodexMaxBodyBytes() int64 {
	if s.MaxBodyBytes > 0 {
		return s.MaxBodyBytes
	}
	return replayablePostMaxBodyBytes
}

// azureCodexErrorSummary reads a bounded prefix of an Azure error body for one
// log line. Azure reports deployment and parameter mistakes only in the body,
// so a status code alone leaves a misconfigured fallback undiagnosable.
func azureCodexErrorSummary(response *http.Response) string {
	if response == nil || response.Body == nil {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, azureCodexMaxErrorBodyBytes))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}

// serveAzureCodex answers a Codex request from Azure and pins the session on
// success. It returns false without consuming the request body when Azure could
// not answer, so the caller continues down its normal path.
func (s Server) serveAzureCodex(
	w http.ResponseWriter,
	r *http.Request,
	sessionKey string,
	preferred int,
	reason string,
	pin bool,
) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, replayablePostMaxBodyBytes))
	if err != nil {
		return false
	}
	restore := func() {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	}
	response, endpoint, ok := s.azureCodexResponse(r, body, sessionKey, preferred, reason)
	if !ok {
		s.azureCodexSessions.unpin(sessionKey)
		restore()
		return false
	}
	// A forced request does not pin: forcing is per request, and the pin exists
	// to keep an involuntary fallback on the cache it just created.
	if pin {
		s.azureCodexSessions.pin(sessionKey, endpoint)
	}
	if s.Logger != nil {
		s.Logger.Warn("serving codex via azure fallback",
			"reason", reason,
			"session", sessionKey,
			"endpoint", s.AzureCodex.Endpoints[endpoint].Name,
			"status", response.StatusCode)
	}
	defer response.Body.Close()
	for key, values := range response.Header {
		if isHopByHopHeader(key) {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	flushingCopy(w, response.Body, nil)
	return true
}

// azureCodexFallbackTransport hands a Codex request to Azure once the pool has
// spent its retry budget and still failed. It wraps the whole retry stack, so
// by the time it sees a failure the account failover and transport retries
// below it are already finished.
type azureCodexFallbackTransport struct {
	base       http.RoundTripper
	server     *Server
	sessionKey string
	// replayBody returns the original request body for the Azure call. The
	// pool's retry layers have already consumed the reader by this point.
	replayBody func() ([]byte, bool)
}

func (t azureCodexFallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	response, err := base.RoundTrip(req)
	if req.Context().Err() != nil {
		return response, err
	}
	reason := "transport_error"
	if err == nil {
		if !azureCodexPoolFailed(response.StatusCode) {
			return response, nil
		}
		reason = fmt.Sprintf("pool_status_%d", response.StatusCode)
	}
	body, ok := t.replayBody()
	if !ok {
		return response, err
	}
	preferred := -1
	if pinned, found := t.server.azureCodexSessions.lookup(t.sessionKey); found {
		preferred = pinned
	}
	fallback, endpoint, served := t.server.azureCodexResponse(req, body, t.sessionKey, preferred, reason)
	if !served {
		return response, err
	}
	t.server.azureCodexSessions.pin(t.sessionKey, endpoint)
	if t.server.Logger != nil {
		t.server.Logger.Warn("serving codex via azure fallback",
			"reason", reason,
			"session", t.sessionKey,
			"endpoint", t.server.AzureCodex.Endpoints[endpoint].Name,
			"status", fallback.StatusCode)
	}
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	return fallback, nil
}

// azureCodexPoolFailed reports whether a pool response is a failure worth
// paying Azure for. Quota (429), broken credentials (401/403), and upstream
// faults (408/5xx) qualify; a 4xx caused by the request itself does not,
// because Azure would reject it the same way for money.
func azureCodexPoolFailed(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return status >= 500
}

// endpointNames lists the usable endpoints for the health payload.
func (c *AzureCodexConfig) endpointNames() []string {
	if !c.configured() {
		return nil
	}
	names := make([]string, 0, len(c.Endpoints))
	for _, endpoint := range c.Endpoints {
		if endpoint.BaseURL == nil || endpoint.APIKey == "" {
			continue
		}
		names = append(names, endpoint.Name)
	}
	return names
}

// azureCodexForceUnavailableMessage explains why a forced request cannot run,
// naming the specific blocker rather than a generic 503. Each of these is a
// different fix: configure the endpoint, drop the session lease, or stop using
// team credential storage.
func azureCodexForceUnavailableMessage(s Server, leased bool, r *http.Request) string {
	switch {
	case !s.AzureCodex.configured():
		return "azure codex route is not configured; set SUBROUTER_AZURE_CODEX_ENDPOINT and SUBROUTER_AZURE_CODEX_API_KEY on the server"
	case leased:
		return "azure codex route cannot serve a session-leased request, which is bound to one pool account"
	case s.CredentialBroker != nil:
		return "azure codex route is disabled under team credential storage; run 'sr storage local'"
	case !azureCodexRequest(r.Method, r.URL.Path):
		return "azure codex route serves only POST /responses"
	default:
		return "azure codex route is unavailable for this request"
	}
}
