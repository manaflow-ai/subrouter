package proxy

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/session"
)

const geminiUploadCapabilityTTL = 7 * 24 * time.Hour

const (
	geminiUploadCapabilityParam = "subrouter_upload_cap"
	geminiUploadExpiryParam     = "subrouter_upload_expires"
)

// GeminiConfig enables a transparent Gemini Developer API gateway. Clients
// present the gateway token as x-goog-api-key; the gateway replaces it
// with the provider key before forwarding the request.
type GeminiConfig struct {
	Upstream     *url.URL
	PublicURL    *url.URL
	APIKey       string
	GatewayToken string
	Transport    http.RoundTripper
}

func (c *GeminiConfig) configured() bool {
	if c == nil || c.Upstream == nil {
		return false
	}
	apiKey := strings.TrimSpace(c.APIKey)
	gatewayToken := strings.TrimSpace(c.GatewayToken)
	return apiKey != "" && gatewayToken != "" && apiKey != gatewayToken
}

func (s Server) geminiHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Gemini == nil || !s.Gemini.configured() {
			http.Error(w, "gemini gateway not configured", http.StatusServiceUnavailable)
			return
		}
		if !authorizeGeminiGateway(r, s.Gemini.GatewayToken) &&
			!authorizeGeminiUploadCapability(r, s.Gemini.GatewayToken, time.Now()) {
			http.Error(w, "gemini gateway token required", http.StatusUnauthorized)
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
		if r.Method == http.MethodHead && (r.URL.Path == "/gemini" || r.URL.Path == "/gemini/") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if s.Lifecycle != nil && s.Lifecycle.Draining() {
			http.Error(w, "subrouter is draining", http.StatusServiceUnavailable)
			return
		}
		endProxyRequest := s.Lifecycle.BeginProxyRequest()
		defer endProxyRequest()

		upstream := cloneURL(s.Gemini.Upstream)
		proxyRequest := r.Clone(r.Context())
		proxyRequest.URL = cloneURL(r.URL)
		proxyRequest.URL.Path = stripProviderPathPrefix(proxyRequest.URL.Path, "gemini")
		proxyRequest.URL.Path = stripDuplicateGatewayVersionPrefix(upstream.Path, proxyRequest.URL.Path)
		proxyRequest.URL.RawPath = ""
		joinedUpstreamPath := joinGatewayUpstreamPath(upstream.Path, proxyRequest.URL.Path)
		if gatewayPathIsUnsafe(&url.URL{Path: joinedUpstreamPath}) {
			http.Error(w, "gemini gateway upstream path is invalid", http.StatusServiceUnavailable)
			return
		}
		if gatewayPathUsesBlockedPrefix(proxyRequest.URL.Path, "/_subrouter") || gatewayPathUsesBlockedPrefix(joinedUpstreamPath, "/_subrouter") {
			http.Error(w, "subrouter management route not allowed", http.StatusForbidden)
			return
		}
		query := proxyRequest.URL.Query()
		query.Del("key")
		query.Del("api_key")
		query.Del("access_token")
		query.Del("oauth_token")
		query.Del("$userProject")
		query.Del(geminiUploadCapabilityParam)
		query.Del(geminiUploadExpiryParam)
		proxyRequest.URL.RawQuery = query.Encode()
		proxyRequest.Header.Del("Authorization")
		proxyRequest.Header.Del("X-Api-Key")
		proxyRequest.Header.Del("Cookie")
		proxyRequest.Header.Del("X-Goog-User-Project")
		stripOpenAIWebSocketCredential(proxyRequest.Header)
		proxyRequest.Header.Set("X-Goog-Api-Key", strings.TrimSpace(s.Gemini.APIKey))
		session.StripSubrouterHeaders(proxyRequest.Header)
		stripOutboundForwardingHeaders(proxyRequest.Header)
		if s.Logger != nil {
			s.Logger.Info("gemini proxy request", "method", r.Method, "path", proxyRequest.URL.Path, "upstream", upstream.Host, "remote_addr", clientRemoteIP(r))
		}

		rp := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(upstream)
				stripOutboundForwardingHeaders(pr.Out.Header)
			},
			Transport: s.Gemini.Transport,
		}
		rp.ModifyResponse = func(response *http.Response) error {
			response.Header.Del("Set-Cookie")
			return rewriteGeminiUploadURLs(response.Header, upstream, s.Gemini.PublicURL, r, s.Gemini.GatewayToken)
		}
		if rp.Transport == nil {
			rp.Transport = s.transport()
		}
		if s.Logger != nil {
			rp.ErrorLog = log.New(proxyErrorWriter{
				logger:   s.Logger,
				agent:    "gemini",
				method:   r.Method,
				path:     proxyRequest.URL.Path,
				upstream: upstream.Host,
			}, "", 0)
			rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
				s.Logger.Error("gemini proxy request failed", "method", r.Method, "path", proxyRequest.URL.Path, "upstream", upstream.Host, "error", err)
				http.Error(w, "gemini upstream request failed", http.StatusBadGateway)
			}
		}
		rp.ServeHTTP(w, proxyRequest)
	})
}

func rewriteGeminiUploadURLs(headers http.Header, upstream, publicURL *url.URL, request *http.Request, gatewayToken string) error {
	for _, header := range []string{"X-Goog-Upload-Url", "X-Goog-Upload-Control-Url"} {
		if err := rewriteGeminiUploadURLHeader(headers, header, upstream, publicURL, request, gatewayToken); err != nil {
			return err
		}
	}
	return nil
}

func rewriteGeminiUploadURLHeader(headers http.Header, header string, upstream, publicURL *url.URL, request *http.Request, gatewayToken string) error {
	raw := strings.TrimSpace(headers.Get(header))
	if raw == "" {
		return nil
	}
	if upstream == nil || request == nil || (request.Host == "" && publicURL == nil) {
		return errors.New("cannot sanitize Gemini upload URL")
	}
	uploadURL, err := url.Parse(raw)
	if err != nil {
		return errors.New("cannot sanitize Gemini upload URL")
	}
	if !uploadURL.IsAbs() {
		resolutionBase := cloneURL(upstream)
		if !strings.HasSuffix(resolutionBase.Path, "/") {
			resolutionBase.Path += "/"
		}
		resolutionBase.RawPath = ""
		uploadURL = resolutionBase.ResolveReference(uploadURL)
	}
	if !sameURLOrigin(uploadURL, upstream) {
		return errors.New("cannot sanitize Gemini upload URL")
	}
	scheme, host := geminiPublicOrigin(request, publicURL)
	if scheme == "" || host == "" {
		return errors.New("cannot sanitize Gemini upload URL")
	}
	uploadURL.Scheme = scheme
	uploadURL.Host = host
	uploadURL.User = nil
	uploadURL.Fragment = ""
	uploadURL.RawFragment = ""
	uploadPath := uploadURL.Path
	basePath := strings.TrimSuffix(upstream.Path, "/")
	if basePath != "" {
		if uploadPath != basePath && !strings.HasPrefix(uploadPath, basePath+"/") {
			return errors.New("cannot sanitize Gemini upload URL")
		}
		uploadPath = strings.TrimPrefix(uploadPath, basePath)
	}
	if !strings.HasPrefix(uploadPath, "/") {
		uploadPath = "/" + uploadPath
	}
	uploadURL.Path = "/gemini" + uploadPath
	uploadURL.RawPath = ""
	addGeminiUploadCapability(uploadURL, gatewayToken, time.Now().Add(geminiUploadCapabilityTTL))
	headers.Set(header, uploadURL.String())
	return nil
}

func sameURLOrigin(left, right *url.URL) bool {
	if left == nil || right == nil || !strings.EqualFold(left.Scheme, right.Scheme) ||
		!strings.EqualFold(left.Hostname(), right.Hostname()) {
		return false
	}
	return effectiveURLPort(left) == effectiveURLPort(right)
}

func effectiveURLPort(value *url.URL) string {
	if port := value.Port(); port != "" {
		return port
	}
	switch strings.ToLower(value.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func geminiPublicOrigin(request *http.Request, publicURL *url.URL) (string, string) {
	if publicURL != nil {
		return publicURL.Scheme, publicURL.Host
	}
	scheme := "http"
	if request.TLS != nil {
		scheme = "https"
	} else {
		forwardedProto, _, _ := strings.Cut(request.Header.Get("X-Forwarded-Proto"), ",")
		if strings.EqualFold(strings.TrimSpace(forwardedProto), "https") {
			scheme = "https"
		}
	}
	return scheme, request.Host
}

func addGeminiUploadCapability(uploadURL *url.URL, gatewayToken string, expires time.Time) {
	query := uploadURL.Query()
	query.Del("key")
	query.Del("api_key")
	query.Del("access_token")
	query.Del("oauth_token")
	query.Del(geminiUploadCapabilityParam)
	query.Set(geminiUploadExpiryParam, strconv.FormatInt(expires.Unix(), 10))
	uploadURL.RawQuery = query.Encode()
	query.Set(geminiUploadCapabilityParam, geminiUploadCapability(uploadURL.Path, query, gatewayToken))
	uploadURL.RawQuery = query.Encode()
}

func authorizeGeminiUploadCapability(r *http.Request, gatewayToken string, now time.Time) bool {
	if r == nil {
		return false
	}
	capabilityPath := r.URL.Path
	if strings.HasPrefix(capabilityPath, "/upload/") {
		capabilityPath = "/gemini" + capabilityPath
	} else if !strings.HasPrefix(capabilityPath, "/gemini/upload/") {
		return false
	}
	query := r.URL.Query()
	got := query.Get(geminiUploadCapabilityParam)
	expiresRaw := query.Get(geminiUploadExpiryParam)
	if got == "" || expiresRaw == "" {
		return false
	}
	expires, err := strconv.ParseInt(expiresRaw, 10, 64)
	if err != nil || now.Unix() > expires {
		return false
	}
	want := geminiUploadCapability(capabilityPath, query, gatewayToken)
	return len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func geminiUploadCapability(path string, query url.Values, gatewayToken string) string {
	unsigned := make(url.Values, len(query))
	for key, values := range query {
		unsigned[key] = append([]string(nil), values...)
	}
	unsigned.Del(geminiUploadCapabilityParam)
	mac := hmac.New(sha256.New, []byte(strings.TrimSpace(gatewayToken)))
	_, _ = mac.Write([]byte(path))
	_, _ = mac.Write([]byte{'\n'})
	_, _ = mac.Write([]byte(unsigned.Encode()))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func authorizeGeminiGateway(r *http.Request, configuredToken string) bool {
	token := strings.TrimSpace(configuredToken)
	if token == "" {
		return false
	}
	got := strings.TrimSpace(r.Header.Get("X-Goog-Api-Key"))
	if got == "" {
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if scheme, value, ok := strings.Cut(auth, " "); ok && strings.EqualFold(scheme, "Bearer") {
			got = strings.TrimSpace(value)
		}
	}
	if got == "" {
		got = strings.TrimSpace(r.URL.Query().Get("key"))
	}
	if len(got) != len(token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}
