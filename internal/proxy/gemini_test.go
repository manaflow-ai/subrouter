package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestGeminiGatewayReplacesClientCredentialAndPreservesAPIPaths(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got := r.Header.Get("X-Goog-Api-Key"); got != "provider-secret" {
			t.Fatalf("X-Goog-Api-Key = %q, want provider credential", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization leaked upstream: %q", got)
		}
		if r.URL.Query().Get("key") != "" {
			t.Fatalf("query key leaked upstream")
		}
		if r.URL.Query().Get("api_key") != "" || r.URL.Query().Get("access_token") != "" || r.URL.Query().Get("oauth_token") != "" {
			t.Fatalf("alternate query credential leaked upstream: %q", r.URL.RawQuery)
		}
		if got := r.Header.Get("X-Api-Key"); got != "" {
			t.Fatalf("cross-gateway API key leaked upstream: %q", got)
		}
		if got := r.Header.Get("Sec-WebSocket-Protocol"); strings.Contains(got, openAIWebSocketCredentialPrefix) {
			t.Fatalf("OpenAI WebSocket credential leaked upstream: %q", got)
		}
		if got := r.Header.Get("Cookie"); got != "" {
			t.Fatalf("gateway cookie leaked upstream: %q", got)
		}
		if r.URL.Query().Get("$userProject") != "" {
			t.Fatalf("Google billing project leaked upstream")
		}
		if got := r.Header.Get("X-Goog-User-Project"); got != "" {
			t.Fatalf("X-Goog-User-Project leaked upstream: %q", got)
		}
		if got := r.Header.Get("X-Subrouter-User-Email"); got != "" {
			t.Fatalf("X-Subrouter-User-Email leaked upstream: %q", got)
		}
		if got := r.Header.Get("X-Subrouter-Session"); got != "" {
			t.Fatalf("X-Subrouter-Session leaked upstream: %q", got)
		}
		if got := r.Header.Get("X-Subrouter-Admin-Token"); got != "" {
			t.Fatalf("X-Subrouter-Admin-Token leaked upstream: %q", got)
		}
		w.Header().Set("X-Goog-Upload-Url", "http://"+r.Host+"/upload/v1beta/files?upload_id=abc")
		w.Header().Set("X-Goog-Upload-Control-Url", "http://"+r.Host+"/upload/v1beta/files?key=provider-secret&upload_id=abc")
		w.Header().Set("Set-Cookie", "provider-session=secret")
		_, _ = io.WriteString(w, r.URL.Path)
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{Gemini: &GeminiConfig{
		Upstream:     upstreamURL,
		APIKey:       "provider-secret",
		GatewayToken: "team-token",
	}}.Handler()

	for _, path := range []string{
		"/gemini/upload/v1beta/files",
		"/gemini/v1beta/interactions",
		"/gemini/v1beta/files/example",
		"/upload/v1beta/files",
		"/v1beta/interactions",
	} {
		req := httptest.NewRequest(http.MethodPost, path+"?key=client-secret&api_key=other&access_token=oauth&oauth_token=legacy&%24userProject=client-project", strings.NewReader("{}"))
		req.Header.Set("X-Goog-Api-Key", "team-token")
		req.Header.Set("Authorization", "Bearer client-secret")
		req.Header.Set("X-Subrouter-User-Email", "alice@example.com")
		req.Header.Set("X-Subrouter-Session", "session-secret")
		req.Header.Set("X-Subrouter-Admin-Token", "admin-secret")
		req.Header.Set("X-Goog-User-Project", "client-project")
		req.Header.Set("X-Api-Key", "anthropic-team")
		req.Header.Set("Sec-WebSocket-Protocol", "realtime, openai-insecure-api-key.openai-team")
		req.Header.Set("Cookie", "gateway-session=secret")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d body = %s", path, rec.Code, rec.Body.String())
		}
		if got := strings.TrimSpace(rec.Body.String()); got != strings.TrimPrefix(path, "/gemini") {
			t.Fatalf("%s upstream path = %q", path, got)
		}
		requireGeminiUploadCapabilityURL(t, rec.Header().Get("X-Goog-Upload-Url"), "http", "example.com", "/gemini/upload/v1beta/files", "team-token")
		requireGeminiUploadCapabilityURL(t, rec.Header().Get("X-Goog-Upload-Control-Url"), "http", "example.com", "/gemini/upload/v1beta/files", "team-token")
		if got := rec.Header().Get("Set-Cookie"); got != "" {
			t.Fatalf("provider cookie leaked to client: %q", got)
		}
	}
	if got := requests.Load(); got != 5 {
		t.Fatalf("upstream requests = %d", got)
	}
}

func TestGeminiGatewayRejectsWrongGatewayToken(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{Gemini: &GeminiConfig{
		Upstream:     upstreamURL,
		APIKey:       "provider-secret",
		GatewayToken: "team-token",
	}}.Handler()

	req := httptest.NewRequest(http.MethodPost, "/gemini/v1beta/interactions", strings.NewReader("{}"))
	req.Header.Set("X-Goog-Api-Key", "wrong-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("upstream requests = %d", got)
	}
}

func TestGeminiGatewayReportsMissingProviderCredential(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPost, "/gemini/v1beta/interactions", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	Server{}.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestGeminiGatewayReportsMissingGatewayToken(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{Gemini: &GeminiConfig{
		Upstream: upstreamURL,
		APIKey:   "provider-secret",
	}}.Handler()

	req := httptest.NewRequest(http.MethodPost, "/gemini/v1beta/interactions", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("upstream requests = %d", got)
	}
}

func TestGeminiGatewayRejectsReusedProviderKey(t *testing.T) {
	t.Parallel()

	upstreamURL, err := url.Parse("https://generativelanguage.googleapis.com")
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{Gemini: &GeminiConfig{
		Upstream: upstreamURL, APIKey: "same-secret", GatewayToken: "same-secret",
	}}.Handler()
	req := httptest.NewRequest(http.MethodPost, "/gemini/v1beta/interactions", strings.NewReader("{}"))
	req.Header.Set("X-Goog-Api-Key", "same-secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestGeminiGatewayRejectsRequestsWhileDraining(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := NewLifecycle()
	lifecycle.Drain()
	handler := Server{
		Lifecycle: lifecycle,
		Gemini: &GeminiConfig{
			Upstream:     upstreamURL,
			APIKey:       "provider-secret",
			GatewayToken: "team-token",
		},
	}.Handler()

	req := httptest.NewRequest(http.MethodPost, "/gemini/v1beta/interactions", strings.NewReader("{}"))
	req.Header.Set("X-Goog-Api-Key", "team-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("upstream requests = %d", got)
	}
	if got := lifecycle.ActiveProxyRequests(); got != 0 {
		t.Fatalf("active proxy requests = %d", got)
	}
}

func TestGeminiGatewayTracksActiveProxyRequests(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := NewLifecycle()
	handler := Server{
		Lifecycle: lifecycle,
		Gemini: &GeminiConfig{
			Upstream:     upstreamURL,
			APIKey:       "provider-secret",
			GatewayToken: "team-token",
		},
	}.Handler()

	req := httptest.NewRequest(http.MethodPost, "/gemini/v1beta/interactions", strings.NewReader("{}"))
	req.Header.Set("X-Goog-Api-Key", "team-token")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(rec, req)
	}()

	<-started
	if got := lifecycle.ActiveProxyRequests(); got != 1 {
		t.Fatalf("active proxy requests = %d, want 1", got)
	}
	close(release)
	<-done
	if got := lifecycle.ActiveProxyRequests(); got != 0 {
		t.Fatalf("active proxy requests after response = %d", got)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestRewriteGeminiUploadURLUsesForwardedHTTPS(t *testing.T) {
	t.Parallel()

	upstream, err := url.Parse("https://generativelanguage.googleapis.com")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/gemini/upload/v1beta/files", nil)
	request.Host = "gateway.example.com"
	request.Header.Set("X-Forwarded-Proto", "https, http")
	headers := http.Header{
		"X-Goog-Upload-Url": []string{"https://provider-user:provider-pass@generativelanguage.googleapis.com/upload/v1beta/files?upload_id=abc&api_key=provider-key&access_token=provider-oauth&oauth_token=provider-legacy#provider-fragment"},
	}
	if err := rewriteGeminiUploadURLs(headers, upstream, nil, request, "team-token"); err != nil {
		t.Fatal(err)
	}
	requireGeminiUploadCapabilityURL(t, headers.Get("X-Goog-Upload-Url"), "https", "gateway.example.com", "/gemini/upload/v1beta/files", "team-token")
}

func TestRewriteGeminiUploadURLUsesConfiguredPublicOrigin(t *testing.T) {
	t.Parallel()

	upstream, err := url.Parse("https://generativelanguage.googleapis.com")
	if err != nil {
		t.Fatal(err)
	}
	publicURL, err := url.Parse("https://gemini.team.example")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/gemini/upload/v1beta/files", nil)
	request.Host = "127.0.0.1:31415"
	request.Header.Set("X-Forwarded-Host", "attacker.example")
	headers := http.Header{
		"X-Goog-Upload-Url": []string{"https://generativelanguage.googleapis.com/upload/v1beta/files?upload_id=abc"},
	}
	if err := rewriteGeminiUploadURLs(headers, upstream, publicURL, request, "team-token"); err != nil {
		t.Fatal(err)
	}
	requireGeminiUploadCapabilityURL(t, headers.Get("X-Goog-Upload-Url"), "https", "gemini.team.example", "/gemini/upload/v1beta/files", "team-token")
}

func TestRewriteGeminiUploadURLNormalizesDefaultUpstreamPort(t *testing.T) {
	t.Parallel()

	upstream, err := url.Parse("https://generativelanguage.googleapis.com:443")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/gemini/upload/v1beta/files", nil)
	headers := http.Header{
		"X-Goog-Upload-Url": []string{"https://generativelanguage.googleapis.com/upload/v1beta/files?upload_id=abc"},
	}
	if err := rewriteGeminiUploadURLs(headers, upstream, nil, request, "team-token"); err != nil {
		t.Fatal(err)
	}
	requireGeminiUploadCapabilityURL(t, headers.Get("X-Goog-Upload-Url"), "http", "example.com", "/gemini/upload/v1beta/files", "team-token")
}

func TestRewriteGeminiUploadURLContainsCustomUpstreamBasePath(t *testing.T) {
	t.Parallel()

	upstream, err := url.Parse("https://proxy.example.com/google")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/gemini/upload/v1beta/files", nil)
	request.Host = "gateway.example.com"
	headers := http.Header{
		"X-Goog-Upload-Url": []string{"https://proxy.example.com/google/upload/v1beta/files?upload_id=abc"},
	}
	if err := rewriteGeminiUploadURLs(headers, upstream, nil, request, "team-token"); err != nil {
		t.Fatal(err)
	}
	requireGeminiUploadCapabilityURL(t, headers.Get("X-Goog-Upload-Url"), "http", "gateway.example.com", "/gemini/upload/v1beta/files", "team-token")
}

func TestRewriteGeminiRelativeUploadURLUsesCustomUpstreamBasePath(t *testing.T) {
	t.Parallel()

	upstream, err := url.Parse("https://proxy.example.com/google")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/gemini/upload/v1beta/files", nil)
	headers := http.Header{
		"X-Goog-Upload-Url": []string{"upload/v1beta/files?upload_id=abc"},
	}
	if err := rewriteGeminiUploadURLs(headers, upstream, nil, request, "team-token"); err != nil {
		t.Fatal(err)
	}
	requireGeminiUploadCapabilityURL(t, headers.Get("X-Goog-Upload-Url"), "http", "example.com", "/gemini/upload/v1beta/files", "team-token")
}

func TestRewriteGeminiUploadURLRejectsPathOutsideCustomUpstreamBase(t *testing.T) {
	t.Parallel()

	upstream, err := url.Parse("https://proxy.example.com/google")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/gemini/upload/v1beta/files", nil)
	headers := http.Header{
		"X-Goog-Upload-Url": []string{"https://proxy.example.com/upload/v1beta/files?upload_id=abc"},
	}
	if err := rewriteGeminiUploadURLs(headers, upstream, nil, request, "team-token"); err == nil {
		t.Fatal("upload URL outside custom upstream base path was accepted")
	}
}

func TestRewriteGeminiUploadURLsRejectsForeignControlURL(t *testing.T) {
	t.Parallel()

	upstream, err := url.Parse("https://generativelanguage.googleapis.com")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/gemini/upload/v1beta/files", nil)
	headers := http.Header{
		"X-Goog-Upload-Control-Url": []string{"https://attacker.example/upload?key=provider-secret"},
	}
	if err := rewriteGeminiUploadURLs(headers, upstream, nil, request, "team-token"); err == nil {
		t.Fatal("foreign upload control URL was accepted")
	}
}

func TestGeminiGatewayAuthenticatesResumableUploadContinuation(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if !strings.HasPrefix(r.URL.Path, "/google/upload/v1beta/files") {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		if got := r.Header.Get("X-Goog-Api-Key"); got != "provider-secret" {
			t.Errorf("X-Goog-Api-Key = %q", got)
		}
		if r.URL.Query().Get("key") != "" {
			t.Errorf("gateway token leaked upstream")
		}
		if r.URL.Query().Get(geminiUploadCapabilityParam) != "" || r.URL.Query().Get(geminiUploadExpiryParam) != "" {
			t.Errorf("upload capability leaked upstream")
		}
		if r.URL.Query().Get("upload_id") == "" {
			w.Header().Set("X-Goog-Upload-Url", "http://"+r.Host+r.URL.Path+"?upload_id=abc")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL + "/google")
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{Gemini: &GeminiConfig{
		Upstream: upstreamURL, APIKey: "provider-secret", GatewayToken: "team-token",
	}}.Handler()

	start := httptest.NewRequest(http.MethodPost, "/gemini/upload/v1beta/files", strings.NewReader("metadata"))
	start.Header.Set("X-Goog-Api-Key", "team-token")
	startRec := httptest.NewRecorder()
	handler.ServeHTTP(startRec, start)
	if startRec.Code != http.StatusOK {
		t.Fatalf("start status = %d body = %s", startRec.Code, startRec.Body.String())
	}
	continuationURL := startRec.Header().Get("X-Goog-Upload-Url")
	if strings.Contains(continuationURL, "team-token") {
		t.Fatalf("continuation URL exposes reusable gateway token: %q", continuationURL)
	}
	parsedContinuation := requireGeminiUploadCapabilityURL(t, continuationURL, "http", "example.com", "/gemini/upload/v1beta/files", "team-token")

	continuation := httptest.NewRequest(http.MethodPost, continuationURL, strings.NewReader("file bytes"))
	continuationRec := httptest.NewRecorder()
	handler.ServeHTTP(continuationRec, continuation)
	if continuationRec.Code != http.StatusNoContent {
		t.Fatalf("continuation status = %d body = %s", continuationRec.Code, continuationRec.Body.String())
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("upstream requests = %d, want 2", got)
	}
	rootAlias := *parsedContinuation
	rootAlias.Path = strings.TrimPrefix(rootAlias.Path, "/gemini")
	rootAlias.RawPath = ""
	rootContinuation := httptest.NewRequest(http.MethodPost, rootAlias.String(), strings.NewReader("root alias bytes"))
	rootContinuationRec := httptest.NewRecorder()
	handler.ServeHTTP(rootContinuationRec, rootContinuation)
	if rootContinuationRec.Code != http.StatusNoContent {
		t.Fatalf("root alias continuation status = %d body = %s", rootContinuationRec.Code, rootContinuationRec.Body.String())
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("upstream requests after root alias = %d, want 3", got)
	}
	tamperedQuery := parsedContinuation.Query()
	tamperedQuery.Set("upload_id", "other")
	parsedContinuation.RawQuery = tamperedQuery.Encode()
	tampered := httptest.NewRequest(http.MethodPost, parsedContinuation.String(), strings.NewReader("tampered"))
	tamperedRec := httptest.NewRecorder()
	handler.ServeHTTP(tamperedRec, tampered)
	if tamperedRec.Code != http.StatusUnauthorized {
		t.Fatalf("tampered continuation status = %d body = %s", tamperedRec.Code, tamperedRec.Body.String())
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("tampered continuation reached upstream: requests = %d", got)
	}
}

func requireGeminiUploadCapabilityURL(t *testing.T, raw, scheme, host, path, gatewayToken string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != scheme || parsed.Host != host || parsed.Path != path {
		t.Fatalf("upload URL = %q", raw)
	}
	if parsed.User != nil || parsed.Fragment != "" {
		t.Fatalf("upload URL retained embedded credentials: %q", raw)
	}
	query := parsed.Query()
	if query.Get("upload_id") != "abc" || query.Get("key") != "" ||
		query.Get("api_key") != "" || query.Get("access_token") != "" || query.Get("oauth_token") != "" ||
		query.Get(geminiUploadCapabilityParam) == "" || query.Get(geminiUploadExpiryParam) == "" {
		t.Fatalf("upload URL query = %q", parsed.RawQuery)
	}
	request := httptest.NewRequest(http.MethodPost, parsed.String(), nil)
	if !authorizeGeminiUploadCapability(request, gatewayToken, time.Now()) {
		t.Fatalf("upload capability did not verify")
	}
	return parsed
}

func TestGeminiUploadCapabilityExpires(t *testing.T) {
	t.Parallel()

	uploadURL, err := url.Parse("https://gateway.example.com/gemini/upload/v1beta/files?upload_id=abc")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	addGeminiUploadCapability(uploadURL, "team-token", now.Add(-time.Second))
	request := httptest.NewRequest(http.MethodPost, uploadURL.String(), nil)
	if authorizeGeminiUploadCapability(request, "team-token", now) {
		t.Fatal("expired upload capability authorized")
	}
}

func TestGeminiGatewayProxiesLiveWebSocket(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("key") != "" {
			t.Errorf("query key leaked upstream")
		}
		if got := r.Header.Get("X-Goog-Api-Key"); got != "provider-secret" {
			t.Errorf("X-Goog-Api-Key = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization leaked upstream: %q", got)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read message: %v", err)
			return
		}
		if err := conn.WriteMessage(messageType, append([]byte("echo:"), message...)); err != nil {
			t.Errorf("write message: %v", err)
		}
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{Gemini: &GeminiConfig{
		Upstream: upstreamURL, APIKey: "provider-secret", GatewayToken: "team-token",
	}}.Handler()
	gateway := httptest.NewServer(handler)
	defer gateway.Close()
	wsURL := "ws" + strings.TrimPrefix(gateway.URL, "http") +
		"/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent?key=team-token"
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if response != nil && response.Body != nil {
			defer response.Body.Close()
		}
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	_, message, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(message); got != "echo:hello" {
		t.Fatalf("message = %q", got)
	}
}
