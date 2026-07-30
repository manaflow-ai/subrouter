package proxy

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/session"
)

const (
	defaultSessionLeaseTTL      = 15 * time.Minute
	sessionLeaseRotationGrace   = 30 * time.Second
	sessionLeaseRenewRetryTTL   = 2 * time.Minute
	maxSessionLeaseRequestBytes = 64 << 10
	sessionLeaseTokenType       = "SRLEASE"
	syntheticChatGPTAccountID   = "cloudmux-broker"
)

var (
	errInvalidSessionLease  = errors.New("invalid or expired session lease")
	errSessionLeaseNotFound = errors.New("session lease not found")
)

// sessionLeaseStore keeps short-lived broker credentials in memory. The
// underlying provider credentials remain in Subrouter's account store and are
// never returned to the caller.
type sessionLeaseStore struct {
	mu         sync.Mutex
	byID       map[string]sessionLease
	byScope    map[string]string
	byToken    map[[32]byte]sessionLeaseTokenBinding
	tokensByID map[string]map[[32]byte]struct{}
	now        func() time.Time
	ttl        time.Duration
}

// sessionLeaseTokenBinding separates the short overlap in which a rotated
// token can still authorize an in-flight model request from the longer window
// in which a timed-out renewal can be retried idempotently.
type sessionLeaseTokenBinding struct {
	LeaseID          string
	RequestExpiresAt time.Time
	RenewExpiresAt   time.Time
}

// sessionLease is the server-side binding for one Cloudmux invocation. Token
// is only returned once through the authenticated lease response and is never
// logged.
type sessionLease struct {
	ID             string
	Token          string
	ScopeKey       string
	OrganizationID string
	WorkspaceID    string
	ConversationID string
	InvocationID   string
	SessionKey     string
	Agent          string
	Provider       accounts.Provider
	AccountID      string
	AuthMode       accounts.AuthMode
	Model          string
	ProxyBaseURL   string
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

type sessionLeaseRequest struct {
	OrganizationID string `json:"organizationId"`
	WorkspaceID    string `json:"workspaceId"`
	ConversationID string `json:"conversationId"`
	InvocationID   string `json:"invocationId"`
	AgentSessionID string `json:"agentSessionId"`
	Agent          string `json:"agent"`
	Provider       string `json:"provider,omitempty"`
	Model          string `json:"model,omitempty"`
	ProxyBaseURL   string `json:"proxyBaseUrl,omitempty"`
}

type sessionLeaseResponse struct {
	LeaseID     string                 `json:"leaseId"`
	SessionKey  string                 `json:"sessionKey"`
	ExpiresAt   string                 `json:"expiresAt"`
	Environment map[string]string      `json:"environment"`
	Assignment  sessionLeaseAssignment `json:"assignment"`
	Pi          sessionLeasePiConfig   `json:"pi"`
}

type sessionLeaseAssignment struct {
	AccountID string `json:"accountId"`
	Provider  string `json:"provider"`
	AuthMode  string `json:"authMode"`
	Model     string `json:"model,omitempty"`
	Reason    string `json:"reason"`
}

// sessionLeasePiConfig is enough for the caller to create an isolated Pi
// models.json provider without embedding a provider credential in that file.
// apiKeyEnvironmentVariable resolves to the ephemeral broker token.
type sessionLeasePiConfig struct {
	Provider                  string `json:"provider"`
	API                       string `json:"api"`
	BaseURL                   string `json:"baseUrl"`
	APIKeyEnvironmentVariable string `json:"apiKeyEnvironmentVariable"`
	Model                     string `json:"model,omitempty"`
}

type sessionLeaseTokenHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
}

type sessionLeaseTokenPayload struct {
	Issuer               string                      `json:"iss"`
	Audience             string                      `json:"aud"`
	IssuedAt             int64                       `json:"iat"`
	ExpiresAt            int64                       `json:"exp"`
	Nonce                string                      `json:"jti"`
	CloudmuxSessionLease bool                        `json:"cloudmux_session_lease"`
	OpenAIAuthentication sessionLeaseOpenAIAuthClaim `json:"https://api.openai.com/auth"`
}

type sessionLeaseOpenAIAuthClaim struct {
	ChatGPTAccountID string `json:"chatgpt_account_id"`
}

func newSessionLeaseStore() *sessionLeaseStore {
	return &sessionLeaseStore{
		byID:       make(map[string]sessionLease),
		byScope:    make(map[string]string),
		byToken:    make(map[[32]byte]sessionLeaseTokenBinding),
		tokensByID: make(map[string]map[[32]byte]struct{}),
		now:        time.Now,
		ttl:        defaultSessionLeaseTTL,
	}
}

func (s *sessionLeaseStore) put(template sessionLease) (sessionLease, error) {
	if s == nil {
		return sessionLease{}, errors.New("session lease store is not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	s.removeExpiredLocked(now)
	if existingID := s.byScope[template.ScopeKey]; existingID != "" {
		if existing, ok := s.byID[existingID]; ok {
			return existing, nil
		}
	}
	id, err := randomLeaseValue("lease_", 18)
	if err != nil {
		return sessionLease{}, err
	}
	expiresAt := now.Add(s.ttl)
	token, err := newSessionLeaseToken(now, expiresAt)
	if err != nil {
		return sessionLease{}, err
	}
	template.ID = id
	template.Token = token
	template.CreatedAt = now
	template.ExpiresAt = expiresAt
	s.byID[id] = template
	s.byScope[template.ScopeKey] = id
	s.bindTokenLocked(id, token, expiresAt, expiresAt)
	return template, nil
}

func (s *sessionLeaseStore) resolve(token string) (sessionLease, error) {
	if s == nil || token == "" {
		return sessionLease{}, errInvalidSessionLease
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	s.removeExpiredLocked(now)
	binding, ok := s.byToken[sha256.Sum256([]byte(token))]
	if !ok || !now.Before(binding.RequestExpiresAt) {
		return sessionLease{}, errInvalidSessionLease
	}
	lease, ok := s.byID[binding.LeaseID]
	if !ok {
		return sessionLease{}, errInvalidSessionLease
	}
	return lease, nil
}

// renew rotates a lease token without changing any of its tenant, session,
// provider, account, or model bindings. The caller must prove possession of
// the current token. A recently rotated token returns the already-current
// lease, which makes concurrent calls and retries after a lost response
// idempotent instead of producing out-of-order token-file writes.
func (s *sessionLeaseStore) renew(id, presentedToken string) (sessionLease, error) {
	if s == nil {
		return sessionLease{}, errSessionLeaseNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	s.removeExpiredLocked(now)
	lease, ok := s.byID[id]
	if !ok {
		return sessionLease{}, errSessionLeaseNotFound
	}
	presentedHash := sha256.Sum256([]byte(presentedToken))
	binding, ok := s.byToken[presentedHash]
	if !ok || binding.LeaseID != id || !now.Before(binding.RenewExpiresAt) {
		return sessionLease{}, errInvalidSessionLease
	}
	currentHash := sha256.Sum256([]byte(lease.Token))
	if presentedHash != currentHash {
		return lease, nil
	}

	expiresAt := now.Add(s.ttl)
	token, err := newSessionLeaseToken(now, expiresAt)
	if err != nil {
		return sessionLease{}, err
	}
	oldBinding := s.byToken[currentHash]
	oldBinding.RequestExpiresAt = earlierTime(oldBinding.RequestExpiresAt, now.Add(sessionLeaseRotationGrace))
	oldBinding.RenewExpiresAt = earlierTime(expiresAt, now.Add(sessionLeaseRenewRetryTTL))
	s.byToken[currentHash] = oldBinding

	lease.Token = token
	lease.ExpiresAt = expiresAt
	s.byID[id] = lease
	s.bindTokenLocked(id, token, expiresAt, expiresAt)
	return lease, nil
}

func (s *sessionLeaseStore) release(id string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeExpiredLocked(s.now().UTC())
	lease, ok := s.byID[id]
	if !ok {
		return false
	}
	s.removeLocked(lease)
	return true
}

func (s *sessionLeaseStore) removeExpiredLocked(now time.Time) {
	for _, lease := range s.byID {
		if !now.Before(lease.ExpiresAt) {
			s.removeLocked(lease)
		}
	}
	for tokenHash, binding := range s.byToken {
		if !now.Before(binding.RenewExpiresAt) {
			s.removeTokenBindingLocked(tokenHash, binding.LeaseID)
		}
	}
}

func (s *sessionLeaseStore) removeLocked(lease sessionLease) {
	delete(s.byID, lease.ID)
	for tokenHash := range s.tokensByID[lease.ID] {
		delete(s.byToken, tokenHash)
	}
	delete(s.tokensByID, lease.ID)
	if s.byScope[lease.ScopeKey] == lease.ID {
		delete(s.byScope, lease.ScopeKey)
	}
}

func (s *sessionLeaseStore) bindTokenLocked(id, token string, requestExpiresAt, renewExpiresAt time.Time) {
	tokenHash := sha256.Sum256([]byte(token))
	s.byToken[tokenHash] = sessionLeaseTokenBinding{
		LeaseID:          id,
		RequestExpiresAt: requestExpiresAt,
		RenewExpiresAt:   renewExpiresAt,
	}
	if s.tokensByID[id] == nil {
		s.tokensByID[id] = make(map[[32]byte]struct{})
	}
	s.tokensByID[id][tokenHash] = struct{}{}
}

func (s *sessionLeaseStore) removeTokenBindingLocked(tokenHash [32]byte, id string) {
	delete(s.byToken, tokenHash)
	delete(s.tokensByID[id], tokenHash)
	if len(s.tokensByID[id]) == 0 {
		delete(s.tokensByID, id)
	}
}

func earlierTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func randomLeaseValue(prefix string, size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate session lease: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(value), nil
}

// newSessionLeaseToken returns a JWT-shaped opaque capability. Pi's
// openai-codex-responses adapter decodes the ChatGPT account claim before it
// sends a request, so the public payload contains a constant synthetic account
// ID. Authorization still relies only on exact server-side token-hash lookup.
// The random signature is an opaque capability segment, not a self-verifying
// signature.
func newSessionLeaseToken(issuedAt, expiresAt time.Time) (string, error) {
	nonce, err := randomLeaseValue("", 18)
	if err != nil {
		return "", err
	}
	signature, err := randomLeaseValue("", 32)
	if err != nil {
		return "", err
	}
	header, err := json.Marshal(sessionLeaseTokenHeader{
		Algorithm: "opaque",
		Type:      sessionLeaseTokenType,
	})
	if err != nil {
		return "", fmt.Errorf("encode session lease header: %w", err)
	}
	payload, err := json.Marshal(sessionLeaseTokenPayload{
		Issuer:               "subrouter",
		Audience:             "cloudmux-pi",
		IssuedAt:             issuedAt.Unix(),
		ExpiresAt:            expiresAt.Unix(),
		Nonce:                nonce,
		CloudmuxSessionLease: true,
		OpenAIAuthentication: sessionLeaseOpenAIAuthClaim{
			ChatGPTAccountID: syntheticChatGPTAccountID,
		},
	})
	if err != nil {
		return "", fmt.Errorf("encode session lease payload: %w", err)
	}
	// Pi currently decodes the payload with atob rather than a base64url
	// decoder. Standard unpadded base64 remains JWT-shaped and works with both
	// atob and tolerant server/actor decoders. The signature is never decoded by
	// Pi and can use the URL-safe alphabet.
	return strings.Join([]string{
		base64.RawStdEncoding.EncodeToString(header),
		base64.RawStdEncoding.EncodeToString(payload),
		signature,
	}, "."), nil
}

func (s Server) requireSessionLeaseAdmin(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Loopback remains usable for local self-hosting. Every network caller
		// must present a configured admin token, even when other legacy admin
		// endpoints are running in permissive mode.
		if isLoopbackRemote(r.RemoteAddr) || (strings.TrimSpace(s.AdminToken) != "" && s.authorizeAdmin(r)) {
			next(w, r)
			return
		}
		http.Error(w, "admin token required", http.StatusUnauthorized)
	}
}

func (s Server) handleSessionLeases(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.Lifecycle != nil && s.Lifecycle.Draining() {
		http.Error(w, "subrouter is draining", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSessionLeaseRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request sessionLeaseRequest
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "invalid session lease request", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid session lease request", http.StatusBadRequest)
		return
	}
	request.normalize()
	if err := validateSessionLeaseRequest(request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	provider, model, err := sessionLeaseProvider(request.Provider, request.Model)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	proxyBaseURL, err := sessionLeaseProxyBaseURL(r, request.ProxyBaseURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	routingKey := sessionLeaseRoutingKey(request, provider, model)
	r.Header.Set("X-Subrouter-Agent", request.Agent)
	r.Header.Set("X-Subrouter-Session", routingKey)
	if model != "" {
		r.Header.Set("X-Subrouter-Model", model)
	}
	account, sessionKey, _, err := s.accountForSessionProviderWithOptions(
		provider,
		agentTypeForProviderSession(request.Agent, provider),
		routingKey,
		r,
		accountSelectionOptions{
			allowFableAPIKeyPool: true,
			ignoreForcedAccount:  true,
			oauthOnly:            provider == accounts.ProviderCodex,
		},
	)
	if err != nil {
		http.Error(w, "no account is available for the requested lease", http.StatusServiceUnavailable)
		return
	}
	account, err = s.refreshSelectedAccount(
		r.Context(),
		provider,
		agentTypeForProviderSession(request.Agent, provider),
		sessionKey,
		"",
		r,
		account,
	)
	if err != nil {
		http.Error(w, "no account is available for the requested lease", http.StatusServiceUnavailable)
		return
	}
	lease, err := s.sessionLeases.put(sessionLease{
		ScopeKey:       sessionLeaseScopeKey(request, provider, model),
		OrganizationID: request.OrganizationID,
		WorkspaceID:    request.WorkspaceID,
		ConversationID: request.ConversationID,
		InvocationID:   request.InvocationID,
		SessionKey:     sessionKey,
		Agent:          request.Agent,
		Provider:       provider,
		AccountID:      account.ID,
		AuthMode:       account.AuthMode,
		Model:          model,
		ProxyBaseURL:   proxyBaseURL,
	})
	if err != nil {
		http.Error(w, "create session lease", http.StatusInternalServerError)
		return
	}
	writeSessionLeaseResponse(w, lease)
}

func (s Server) handleSessionLease(w http.ResponseWriter, r *http.Request) {
	relativePath := strings.Trim(strings.TrimPrefix(r.URL.Path, "/internal/v1/session-leases/"), "/")
	parts := strings.Split(relativePath, "/")
	if relativePath == "" || len(parts) > 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	if len(parts) == 2 {
		if parts[1] != "renew" {
			http.NotFound(w, r)
			return
		}
		s.handleSessionLeaseRenew(w, r, id)
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Deletion is idempotent so actor cleanup can retry after a timeout.
	s.sessionLeases.release(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s Server) handleSessionLeaseRenew(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	presentedToken := strings.TrimSpace(r.Header.Get("X-Subrouter-Lease"))
	if !looksLikeSessionLeaseToken(presentedToken) {
		http.Error(w, "session lease token required", http.StatusUnauthorized)
		return
	}
	lease, err := s.sessionLeases.renew(id, presentedToken)
	switch {
	case errors.Is(err, errSessionLeaseNotFound):
		http.NotFound(w, r)
	case errors.Is(err, errInvalidSessionLease):
		http.Error(w, "invalid or expired session lease", http.StatusUnauthorized)
	case err != nil:
		http.Error(w, "renew session lease", http.StatusInternalServerError)
	default:
		writeSessionLeaseResponse(w, lease)
	}
}

func (s Server) resolveSessionLease(r *http.Request) (sessionLease, bool, error) {
	token, presented := presentedSessionLeaseToken(r)
	if !presented {
		return sessionLease{}, false, nil
	}
	lease, err := s.sessionLeases.resolve(token)
	if err != nil {
		return sessionLease{}, true, err
	}
	return lease, true, nil
}

func (lease sessionLease) allowsRequest(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	switch lease.Provider {
	case accounts.ProviderCodex:
		return codexResponsePath(r.URL.Path)
	case accounts.ProviderClaude:
		return r.URL.Path == "/v1/messages"
	case accounts.ProviderKimi:
		return r.URL.Path == "/kimi/v1/messages"
	case accounts.ProviderZAI:
		return r.URL.Path == "/zai/chat/completions"
	default:
		return false
	}
}

func (lease sessionLease) allowsAccount(account accounts.Account) bool {
	return account.ID == lease.AccountID &&
		accountProviderOrCodex(account) == lease.Provider &&
		account.AuthMode == lease.AuthMode
}

// validateRequestModel verifies the model that the provider will receive. It
// deliberately ignores Subrouter routing headers because those are stripped
// before forwarding and therefore cannot prove what the JSON request selects.
func (lease sessionLease) validateRequestModel(r *http.Request) error {
	if lease.Model == "" {
		return nil
	}
	if r == nil {
		return errors.New("request is required")
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return errors.New("request query is invalid")
	}
	for _, value := range query["model"] {
		model := session.NormalizeModel(value)
		if model == "" || model != lease.Model {
			return errors.New("request query model conflicts with lease")
		}
	}
	models, err := requestJSONModels(r, replayablePostMaxBodyBytes)
	if err != nil {
		return err
	}
	if len(models) == 0 {
		return errors.New("request body model is required")
	}
	for _, model := range models {
		if model != lease.Model {
			return errors.New("request body model conflicts with lease")
		}
	}
	return nil
}

// requestJSONModels returns every top-level model value, including duplicate
// JSON keys. Reading into a map would collapse duplicates and allow a parser
// disagreement to bypass a model-bound lease. The request body is restored for
// the normal proxy path after validation.
func requestJSONModels(r *http.Request, maxBytes int64) ([]string, error) {
	if r.Body == nil || maxBytes <= 0 {
		return nil, errors.New("JSON request body is required")
	}
	if contentType := strings.ToLower(r.Header.Get("Content-Type")); !strings.Contains(contentType, "json") {
		return nil, errors.New("JSON request body is required")
	}
	wireBody, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return nil, errors.New("read request body")
	}
	if int64(len(wireBody)) > maxBytes {
		r.Body = prefixReadCloser{
			Reader: io.MultiReader(bytes.NewReader(wireBody), r.Body),
			Closer: r.Body,
		}
		return nil, errors.New("request body is too large to validate")
	}
	if err := r.Body.Close(); err != nil {
		return nil, errors.New("close request body")
	}
	r.Body = io.NopCloser(bytes.NewReader(wireBody))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(wireBody)), nil
	}
	r.ContentLength = int64(len(wireBody))

	body, err := decodedJSONRequestBody(wireBody, r.Header.Get("Content-Encoding"), maxBytes)
	if err != nil {
		return nil, err
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("request body is not valid JSON: %w", err)
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return nil, errors.New("request body must be a JSON object")
	}
	models := make([]string, 0, 1)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("request body key is not valid JSON: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("request body is not valid JSON")
		}
		if key != "model" {
			if err := skipJSONValue(decoder); err != nil {
				return nil, fmt.Errorf("request body value for %q is not valid JSON: %w", key, err)
			}
			continue
		}
		var value string
		if err := decoder.Decode(&value); err != nil {
			return nil, errors.New("request body model must be a string")
		}
		model := session.NormalizeModel(value)
		if model == "" {
			return nil, errors.New("request body model is invalid")
		}
		models = append(models, model)
	}
	end, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("request body object is not valid JSON: %w", err)
	}
	if end != json.Delim('}') {
		return nil, errors.New("request body object is not valid JSON")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("request body has trailing JSON")
	}
	return models, nil
}

func decodedJSONRequestBody(wireBody []byte, contentEncoding string, maxBytes int64) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(contentEncoding)) {
	case "", "identity":
		return wireBody, nil
	case "zstd":
		decoder, err := zstd.NewReader(
			bytes.NewReader(wireBody),
			zstd.WithDecoderMaxMemory(uint64(maxBytes)),
		)
		if err != nil {
			return nil, errors.New("request body has invalid zstd encoding")
		}
		defer decoder.Close()
		body, err := io.ReadAll(io.LimitReader(decoder, maxBytes+1))
		if err != nil {
			if errors.Is(err, zstd.ErrDecoderSizeExceeded) || errors.Is(err, zstd.ErrWindowSizeExceeded) {
				return nil, errors.New("decoded request body is too large to validate")
			}
			return nil, errors.New("request body has invalid zstd encoding")
		}
		if int64(len(body)) > maxBytes {
			return nil, errors.New("decoded request body is too large to validate")
		}
		return body, nil
	default:
		return nil, errors.New("request body content encoding is unsupported")
	}
}

func skipJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		for decoder.More() {
			if _, err := decoder.Token(); err != nil {
				return err
			}
			if err := skipJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := skipJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return errors.New("unexpected JSON delimiter")
	}
}

func presentedSessionLeaseToken(r *http.Request) (string, bool) {
	if explicit := strings.TrimSpace(r.Header.Get("X-Subrouter-Lease")); explicit != "" {
		return explicit, true
	}
	for _, value := range []string{
		bearerToken(r.Header.Get("Authorization")),
		strings.TrimSpace(r.Header.Get("X-Api-Key")),
	} {
		if looksLikeSessionLeaseToken(value) {
			return value, true
		}
	}
	return "", false
}

func looksLikeSessionLeaseToken(value string) bool {
	if value == "" || len(value) > 4096 {
		return false
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return false
	}
	headerBody, err := decodeSessionLeaseTokenSegment(parts[0])
	if err != nil {
		return false
	}
	var header sessionLeaseTokenHeader
	if err := json.Unmarshal(headerBody, &header); err != nil || header.Type != sessionLeaseTokenType {
		return false
	}
	payloadBody, err := decodeSessionLeaseTokenSegment(parts[1])
	if err != nil {
		return false
	}
	var payload sessionLeaseTokenPayload
	return json.Unmarshal(payloadBody, &payload) == nil && payload.CloudmuxSessionLease
}

func decodeSessionLeaseTokenSegment(value string) ([]byte, error) {
	for _, encoding := range []*base64.Encoding{
		base64.RawStdEncoding,
		base64.StdEncoding,
		base64.RawURLEncoding,
		base64.URLEncoding,
	} {
		decoded, err := encoding.DecodeString(value)
		if err == nil {
			return decoded, nil
		}
	}
	return nil, errors.New("invalid session lease token encoding")
}

func bearerToken(value string) string {
	before, after, ok := strings.Cut(strings.TrimSpace(value), " ")
	if !ok || !strings.EqualFold(before, "Bearer") {
		return ""
	}
	return strings.TrimSpace(after)
}

func validateSessionLeaseRequest(request sessionLeaseRequest) error {
	fields := []struct {
		name  string
		value string
	}{
		{"organizationId", request.OrganizationID},
		{"workspaceId", request.WorkspaceID},
		{"conversationId", request.ConversationID},
		{"invocationId", request.InvocationID},
		{"agentSessionId", request.AgentSessionID},
	}
	for _, field := range fields {
		value := strings.TrimSpace(field.value)
		if value == "" || len(value) > 256 {
			return fmt.Errorf("%s is required and must be at most 256 bytes", field.name)
		}
	}
	if request.Agent != "pi" {
		return errors.New("agent must be pi")
	}
	if len(request.Model) > 256 {
		return errors.New("model must be at most 256 bytes")
	}
	return nil
}

func (request *sessionLeaseRequest) normalize() {
	request.OrganizationID = strings.TrimSpace(request.OrganizationID)
	request.WorkspaceID = strings.TrimSpace(request.WorkspaceID)
	request.ConversationID = strings.TrimSpace(request.ConversationID)
	request.InvocationID = strings.TrimSpace(request.InvocationID)
	request.AgentSessionID = strings.TrimSpace(request.AgentSessionID)
	request.Agent = strings.ToLower(strings.TrimSpace(request.Agent))
	request.Provider = strings.ToLower(strings.TrimSpace(request.Provider))
	request.Model = strings.TrimSpace(request.Model)
	request.ProxyBaseURL = strings.TrimSpace(request.ProxyBaseURL)
}

func sessionLeaseProvider(providerValue, modelValue string) (accounts.Provider, string, error) {
	providerName := strings.ToLower(strings.TrimSpace(providerValue))
	model := strings.TrimSpace(modelValue)
	if providerName == "" {
		modelProvider, modelID, hasProvider := strings.Cut(model, "/")
		if hasProvider {
			providerName = strings.ToLower(strings.TrimSpace(modelProvider))
			model = strings.TrimSpace(modelID)
		} else {
			lowerModel := strings.ToLower(model)
			switch {
			case strings.HasPrefix(lowerModel, "claude-"):
				providerName = "claude"
			case strings.HasPrefix(lowerModel, "kimi-"):
				providerName = "kimi"
			case strings.HasPrefix(lowerModel, "glm-"):
				providerName = "zai"
			default:
				providerName = "codex"
			}
		}
	}
	provider, err := parseSessionLeaseProvider(providerName)
	if err != nil {
		return "", "", err
	}
	if modelProvider, modelID, hasProvider := strings.Cut(model, "/"); hasProvider {
		if modelProviderValue, modelProviderErr := parseSessionLeaseProvider(strings.ToLower(strings.TrimSpace(modelProvider))); modelProviderErr == nil {
			if modelProviderValue != provider {
				return "", "", errors.New("provider and model provider do not match")
			}
			model = strings.TrimSpace(modelID)
		}
	}
	return provider, model, nil
}

func parseSessionLeaseProvider(providerName string) (accounts.Provider, error) {
	switch providerName {
	case "codex", "openai", "openai-codex":
		return accounts.ProviderCodex, nil
	case "claude", "anthropic":
		return accounts.ProviderClaude, nil
	case "kimi", "kimi-for-coding":
		return accounts.ProviderKimi, nil
	case "zai", "glm":
		return accounts.ProviderZAI, nil
	default:
		return "", fmt.Errorf("unsupported provider %q", providerName)
	}
}

func sessionLeaseProxyBaseURL(r *http.Request, override string) (string, error) {
	value := strings.TrimRight(strings.TrimSpace(override), "/")
	if value == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		value = scheme + "://" + r.Host
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("proxyBaseUrl must be an http or https base URL")
	}
	return strings.TrimRight(value, "/"), nil
}

func sessionLeaseScopeKey(request sessionLeaseRequest, provider accounts.Provider, model string) string {
	var key strings.Builder
	for _, component := range []string{
		request.OrganizationID,
		request.WorkspaceID,
		request.ConversationID,
		request.InvocationID,
		request.AgentSessionID,
		string(provider),
		model,
	} {
		fmt.Fprintf(&key, "%d:", len(component))
		key.WriteString(component)
	}
	return key.String()
}

// sessionLeaseRoutingKey separates account affinity for distinct invocations
// while keeping retries of one invocation idempotent. The agent's own session
// remains stable in the sandbox, so routing affinity does not need to outlive
// the invocation-scoped lease.
func sessionLeaseRoutingKey(request sessionLeaseRequest, provider accounts.Provider, model string) string {
	digest := sha256.Sum256([]byte(sessionLeaseScopeKey(request, provider, model)))
	return "lease-route-" + base64.RawURLEncoding.EncodeToString(digest[:18])
}

func sessionLeaseResponseFor(lease sessionLease) sessionLeaseResponse {
	baseURL := strings.TrimRight(lease.ProxyBaseURL, "/")
	piBaseURL := baseURL
	api := "openai-codex-responses"
	switch lease.Provider {
	case accounts.ProviderCodex:
		baseURL += "/backend-api/codex"
		// Pi's openai-codex-responses adapter appends /codex/responses.
		// Point it at the ChatGPT-compatible prefix instead of the generic
		// OpenAI-compatible /v1 prefix.
		piBaseURL += "/backend-api"
	case accounts.ProviderClaude:
		api = "anthropic-messages"
	case accounts.ProviderKimi:
		api = "anthropic-messages"
		baseURL += "/kimi"
		piBaseURL += "/kimi"
	case accounts.ProviderZAI:
		api = "openai-completions"
		baseURL += "/zai"
		piBaseURL += "/zai"
	}
	environment := map[string]string{
		"CLOUDMUX_SUBROUTER_LEASE_TOKEN": lease.Token,
	}
	if lease.Provider == accounts.ProviderCodex || lease.Provider == accounts.ProviderZAI {
		environment["OPENAI_API_KEY"] = lease.Token
		environment["OPENAI_BASE_URL"] = baseURL
	} else {
		environment["ANTHROPIC_API_KEY"] = lease.Token
		environment["ANTHROPIC_AUTH_TOKEN"] = lease.Token
		environment["ANTHROPIC_BASE_URL"] = baseURL
	}
	return sessionLeaseResponse{
		LeaseID:     lease.ID,
		SessionKey:  lease.SessionKey,
		ExpiresAt:   lease.ExpiresAt.Format(time.RFC3339Nano),
		Environment: environment,
		Assignment: sessionLeaseAssignment{
			AccountID: lease.AccountID,
			Provider:  string(lease.Provider),
			AuthMode:  string(lease.AuthMode),
			Model:     lease.Model,
			Reason:    "subrouter_scheduler",
		},
		Pi: sessionLeasePiConfig{
			Provider:                  "cloudmux-subrouter",
			API:                       api,
			BaseURL:                   piBaseURL,
			APIKeyEnvironmentVariable: "CLOUDMUX_SUBROUTER_LEASE_TOKEN",
			Model:                     lease.Model,
		},
	}
}

func writeSessionLeaseResponse(w http.ResponseWriter, lease sessionLease) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, sessionLeaseResponseFor(lease))
}
