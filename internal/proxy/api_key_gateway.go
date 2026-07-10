package proxy

import (
	"crypto/subtle"
	"errors"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/session"
)

// APIKeyGatewayConfig exposes a provider API through a separate team
// credential while keeping the provider credential on the server.
type APIKeyGatewayConfig struct {
	Upstream     *url.URL
	APIKey       string
	GatewayToken string
	Transport    http.RoundTripper
}

func (c *APIKeyGatewayConfig) configured() bool {
	if c == nil || c.Upstream == nil {
		return false
	}
	apiKey := strings.TrimSpace(c.APIKey)
	gatewayToken := strings.TrimSpace(c.GatewayToken)
	return apiKey != "" && gatewayToken != "" && apiKey != gatewayToken
}

type apiKeyGatewayAuth int

const (
	apiKeyGatewayBearer apiKeyGatewayAuth = iota
	apiKeyGatewayXAPIKeyOrBearer
)

type apiKeyGatewaySpec struct {
	name                  string
	prefixes              []string
	auth                  apiKeyGatewayAuth
	stripHeaders          []string
	blockedPathPrefixes   []string
	blockedAPIKeyPrefixes []string
}

func (s Server) rejectMisroutedGatewayCredentials(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if baseURLProbeRequest(r) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if s.requestUsesConfiguredGatewayCredential(r) {
			http.Error(w, "gateway credential used outside its gateway route", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s Server) requestUsesConfiguredGatewayCredential(r *http.Request) bool {
	if r == nil {
		return false
	}
	var configured []string
	if s.Gemini != nil {
		configured = append(configured, strings.TrimSpace(s.Gemini.GatewayToken))
	}
	if s.AnthropicGateway != nil {
		configured = append(configured, strings.TrimSpace(s.AnthropicGateway.GatewayToken))
	}
	if s.OpenAIGateway != nil {
		configured = append(configured, strings.TrimSpace(s.OpenAIGateway.GatewayToken))
	}
	if s.Bedrock != nil {
		configured = append(configured, strings.TrimSpace(s.Bedrock.GatewayToken))
	}
	var presented []string
	presented = append(presented, r.Header.Values("X-Goog-Api-Key")...)
	presented = append(presented, r.Header.Values("X-Api-Key")...)
	for _, header := range r.Header.Values("Authorization") {
		if scheme, value, ok := strings.Cut(strings.TrimSpace(header), " "); ok && strings.EqualFold(scheme, "Bearer") {
			presented = append(presented, strings.TrimSpace(value))
		}
	}
	for _, value := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, protocol := range strings.Split(value, ",") {
			protocol = strings.TrimSpace(protocol)
			if strings.HasPrefix(protocol, openAIWebSocketCredentialPrefix) {
				presented = append(presented, strings.TrimPrefix(protocol, openAIWebSocketCredentialPrefix))
			}
		}
	}
	query := r.URL.Query()
	presented = append(presented, query["key"]...)
	presented = append(presented, query["api_key"]...)
	presented = append(presented, query["access_token"]...)
	presented = append(presented, query["oauth_token"]...)
	for _, candidate := range presented {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		for _, token := range configured {
			if len(candidate) == len(token) && token != "" && subtle.ConstantTimeCompare([]byte(candidate), []byte(token)) == 1 {
				return true
			}
		}
	}
	return false
}

func (s Server) apiKeyGatewayHandler(config *APIKeyGatewayConfig, spec apiKeyGatewaySpec) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if config == nil || !config.configured() {
			http.Error(w, spec.name+" gateway not configured", http.StatusServiceUnavailable)
			return
		}
		for _, prefix := range spec.blockedAPIKeyPrefixes {
			if strings.HasPrefix(strings.TrimSpace(config.APIKey), prefix) {
				http.Error(w, spec.name+" gateway requires a non-admin provider key", http.StatusServiceUnavailable)
				return
			}
		}
		if !authorizeAPIKeyGateway(r, config.GatewayToken, spec.auth) {
			http.Error(w, spec.name+" gateway token required", http.StatusUnauthorized)
			return
		}
		if gatewayMethodCanReflectCredentials(r.Method) {
			http.Error(w, "gateway method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if gatewayPathIsUnsafe(r.URL) {
			http.Error(w, "invalid gateway path", http.StatusBadRequest)
			return
		}
		if s.Lifecycle != nil && s.Lifecycle.Draining() {
			http.Error(w, "subrouter is draining", http.StatusServiceUnavailable)
			return
		}
		endProxyRequest := s.Lifecycle.BeginProxyRequest()
		defer endProxyRequest()

		upstream := cloneURL(config.Upstream)
		proxyRequest := r.Clone(r.Context())
		proxyRequest.URL = cloneURL(r.URL)
		proxyRequest.URL.Path = stripGatewayPathPrefix(proxyRequest.URL.Path, spec.prefixes)
		proxyRequest.URL.Path = stripDuplicateGatewayVersionPrefix(upstream.Path, proxyRequest.URL.Path)
		proxyRequest.URL.RawPath = ""
		joinedUpstreamPath := joinGatewayUpstreamPath(upstream.Path, proxyRequest.URL.Path)
		if gatewayPathIsUnsafe(&url.URL{Path: joinedUpstreamPath}) {
			http.Error(w, spec.name+" gateway upstream path is invalid", http.StatusServiceUnavailable)
			return
		}
		if gatewayPathUsesBlockedPrefix(proxyRequest.URL.Path, "/_subrouter") || gatewayPathUsesBlockedPrefix(joinedUpstreamPath, "/_subrouter") {
			http.Error(w, "subrouter management route not allowed", http.StatusForbidden)
			return
		}
		for _, prefix := range spec.blockedPathPrefixes {
			if gatewayPathUsesBlockedPrefix(proxyRequest.URL.Path, prefix) || gatewayPathUsesBlockedPrefix(joinedUpstreamPath, prefix) {
				http.Error(w, spec.name+" administrative route not allowed", http.StatusForbidden)
				return
			}
		}
		query := proxyRequest.URL.Query()
		query.Del("key")
		query.Del("api_key")
		query.Del("access_token")
		query.Del("oauth_token")
		proxyRequest.URL.RawQuery = query.Encode()
		proxyRequest.Header.Del("Authorization")
		proxyRequest.Header.Del("X-Api-Key")
		proxyRequest.Header.Del("X-Goog-Api-Key")
		proxyRequest.Header.Del("Cookie")
		stripOpenAIWebSocketCredential(proxyRequest.Header)
		if spec.auth == apiKeyGatewayXAPIKeyOrBearer {
			removeCommaHeaderValue(proxyRequest.Header, "Anthropic-Beta", claudeOAuthBetaHeader)
		}
		for _, header := range spec.stripHeaders {
			proxyRequest.Header.Del(header)
		}
		if spec.auth == apiKeyGatewayXAPIKeyOrBearer {
			proxyRequest.Header.Set("X-Api-Key", strings.TrimSpace(config.APIKey))
		} else {
			proxyRequest.Header.Set("Authorization", "Bearer "+strings.TrimSpace(config.APIKey))
		}
		session.StripSubrouterHeaders(proxyRequest.Header)
		stripOutboundForwardingHeaders(proxyRequest.Header)
		if s.Logger != nil {
			s.Logger.Info(spec.name+" api proxy request", "method", r.Method, "path", proxyRequest.URL.Path, "upstream", upstream.Host, "remote_addr", clientRemoteIP(r))
		}

		rp := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(upstream)
				stripOutboundForwardingHeaders(pr.Out.Header)
			},
			Transport: config.Transport,
		}
		rp.ModifyResponse = func(response *http.Response) error {
			response.Header.Del("Set-Cookie")
			return nil
		}
		if rp.Transport == nil {
			rp.Transport = s.transport()
		}
		if s.Logger != nil {
			rp.ErrorLog = log.New(proxyErrorWriter{
				logger:   s.Logger,
				agent:    spec.name + "-api",
				method:   r.Method,
				path:     proxyRequest.URL.Path,
				upstream: upstream.Host,
			}, "", 0)
			rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
				s.Logger.Error(spec.name+" api proxy request failed", "method", r.Method, "path", proxyRequest.URL.Path, "upstream", upstream.Host, "error", err)
				http.Error(w, spec.name+" upstream request failed", http.StatusBadGateway)
			}
		}
		rp.ServeHTTP(w, proxyRequest)
	})
}

func (s Server) validateReloadedGatewayCredentials(loaded []accounts.Account) error {
	providerKeys := []string{s.ClaudeFableAPIKey}
	gatewayTokens := make([]string, 0, 4)
	if s.Gemini != nil {
		providerKeys = append(providerKeys, s.Gemini.APIKey)
		gatewayTokens = append(gatewayTokens, s.Gemini.GatewayToken)
	}
	if s.AnthropicGateway != nil {
		providerKeys = append(providerKeys, s.AnthropicGateway.APIKey)
		gatewayTokens = append(gatewayTokens, s.AnthropicGateway.GatewayToken)
	}
	if s.OpenAIGateway != nil {
		providerKeys = append(providerKeys, s.OpenAIGateway.APIKey)
		gatewayTokens = append(gatewayTokens, s.OpenAIGateway.GatewayToken)
	}
	if s.Bedrock != nil {
		gatewayTokens = append(gatewayTokens, s.Bedrock.GatewayToken)
	}
	for _, account := range loaded {
		if account.AuthMode == accounts.AuthModeAPIKey {
			providerKeys = append(providerKeys, account.Token)
		}
	}
	for _, providerKey := range providerKeys {
		providerKey = strings.TrimSpace(providerKey)
		if providerKey == "" {
			continue
		}
		if adminToken := strings.TrimSpace(s.AdminToken); adminToken != "" && providerKey == adminToken {
			return errors.New("reloaded provider key conflicts with the admin token")
		}
		for _, gatewayToken := range gatewayTokens {
			if providerKey == strings.TrimSpace(gatewayToken) {
				return errors.New("reloaded provider key conflicts with a gateway token")
			}
		}
	}
	return nil
}

func joinGatewayUpstreamPath(basePath, requestPath string) string {
	baseSlash := strings.HasSuffix(basePath, "/")
	requestSlash := strings.HasPrefix(requestPath, "/")
	switch {
	case baseSlash && requestSlash:
		return basePath + requestPath[1:]
	case !baseSlash && !requestSlash:
		return basePath + "/" + requestPath
	default:
		return basePath + requestPath
	}
}

func stripDuplicateGatewayVersionPrefix(basePath, requestPath string) string {
	baseVersion := pathLastSegment(basePath)
	requestVersion := pathFirstSegment(requestPath)
	if baseVersion == "" || baseVersion != requestVersion || !gatewayAPIVersionSegment(baseVersion) {
		return requestPath
	}
	stripped := strings.TrimPrefix(requestPath, "/"+requestVersion)
	if stripped == "" {
		return "/"
	}
	return stripped
}

func pathLastSegment(value string) string {
	value = strings.Trim(value, "/")
	if index := strings.LastIndexByte(value, '/'); index >= 0 {
		return value[index+1:]
	}
	return value
}

func pathFirstSegment(value string) string {
	value = strings.TrimPrefix(value, "/")
	if index := strings.IndexByte(value, '/'); index >= 0 {
		return value[:index]
	}
	return value
}

func gatewayAPIVersionSegment(segment string) bool {
	return segment == "v1" || segment == "v1beta" || segment == "v1alpha"
}

func gatewayPathUsesBlockedPrefix(requestPath, blockedPrefix string) bool {
	if requestPath == blockedPrefix || strings.HasPrefix(requestPath, blockedPrefix+"/") {
		return true
	}
	return strings.Contains(requestPath, blockedPrefix+"/") || strings.HasSuffix(requestPath, blockedPrefix)
}

func gatewayMethodCanReflectCredentials(method string) bool {
	return strings.EqualFold(method, "TRACE") || strings.EqualFold(method, "TRACK")
}

func gatewayPathIsUnsafe(requestURL *url.URL) bool {
	if requestURL == nil {
		return true
	}
	fullyDecodedPath, err := url.PathUnescape(requestURL.Path)
	if err != nil || fullyDecodedPath != requestURL.Path {
		return true
	}
	for _, candidate := range []string{requestURL.Path, requestURL.RawPath} {
		if candidate == "" {
			continue
		}
		for range 8 {
			if !strings.HasPrefix(candidate, "/") || strings.Contains(candidate, "//") || strings.Contains(candidate, `\`) {
				return true
			}
			lowerCandidate := strings.ToLower(candidate)
			if strings.Contains(lowerCandidate, "%2f") || strings.Contains(lowerCandidate, "%5c") {
				return true
			}
			for _, segment := range strings.Split(candidate, "/") {
				if segment == "." || segment == ".." {
					return true
				}
			}
			decoded, err := url.PathUnescape(candidate)
			if err != nil {
				return true
			}
			if decoded == candidate {
				candidate = ""
				break
			}
			candidate = decoded
		}
		if candidate != "" {
			return true
		}
	}
	return false
}

func authorizeAPIKeyGateway(r *http.Request, configuredToken string, auth apiKeyGatewayAuth) bool {
	token := strings.TrimSpace(configuredToken)
	if token == "" {
		return false
	}
	var got string
	if auth == apiKeyGatewayXAPIKeyOrBearer {
		got = strings.TrimSpace(r.Header.Get("X-Api-Key"))
	}
	if got == "" {
		header := strings.TrimSpace(r.Header.Get("Authorization"))
		if scheme, value, ok := strings.Cut(header, " "); ok && strings.EqualFold(scheme, "Bearer") {
			got = strings.TrimSpace(value)
		}
	}
	if got == "" && auth == apiKeyGatewayBearer {
		got = openAIWebSocketCredential(r.Header)
	}
	if len(got) != len(token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

const openAIWebSocketCredentialPrefix = "openai-insecure-api-key."

var openAIWebSocketTenantPrefixes = []string{"openai-organization.", "openai-project."}

func openAIWebSocketCredential(headers http.Header) string {
	for _, value := range headers.Values("Sec-WebSocket-Protocol") {
		for _, protocol := range strings.Split(value, ",") {
			protocol = strings.TrimSpace(protocol)
			if strings.HasPrefix(protocol, openAIWebSocketCredentialPrefix) {
				return strings.TrimPrefix(protocol, openAIWebSocketCredentialPrefix)
			}
		}
	}
	return ""
}

func stripOpenAIWebSocketCredential(headers http.Header) {
	var kept []string
	for _, value := range headers.Values("Sec-WebSocket-Protocol") {
		for _, protocol := range strings.Split(value, ",") {
			protocol = strings.TrimSpace(protocol)
			lowerProtocol := strings.ToLower(protocol)
			isTenantSelector := false
			for _, prefix := range openAIWebSocketTenantPrefixes {
				if strings.HasPrefix(lowerProtocol, prefix) {
					isTenantSelector = true
					break
				}
			}
			if protocol != "" && !strings.HasPrefix(lowerProtocol, openAIWebSocketCredentialPrefix) && !isTenantSelector {
				kept = append(kept, protocol)
			}
		}
	}
	if len(kept) == 0 {
		headers.Del("Sec-WebSocket-Protocol")
		return
	}
	headers.Set("Sec-WebSocket-Protocol", strings.Join(kept, ", "))
}

func stripGatewayPathPrefix(path string, prefixes []string) string {
	for _, prefix := range prefixes {
		stripped := stripProviderPathPrefix(path, prefix)
		if stripped != path {
			return stripped
		}
	}
	return path
}
