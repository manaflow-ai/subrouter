package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

type bedrockRoundTripFunc func(*http.Request) (*http.Response, error)

func (f bedrockRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func staticBedrockCreds() aws.CredentialsProvider {
	return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "secret", Source: "test"}, nil
	})
}

func namedBedrockCreds(accessKey string) aws.CredentialsProvider {
	return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: "secret", Source: accessKey}, nil
	})
}

func TestBedrockHandlerSignsAndForwards(t *testing.T) {
	var captured *http.Request
	var capturedBody string
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		captured = req
		b, _ := io.ReadAll(req.Body)
		capturedBody = string(b)
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})
	s := Server{Bedrock: &BedrockConfig{Regions: []string{"us-east-1"}, Credentials: staticBedrockCreds(), Transport: rt}}
	h := s.bedrockHandler()

	req := httptest.NewRequest(http.MethodPost, "/bedrock/model/us.anthropic.claude-fable-5/invoke", strings.NewReader(`{"anthropic_version":"bedrock-2023-05-31"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-token-should-be-dropped")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != `{"ok":true}` {
		t.Fatalf("body = %q, want forwarded upstream body", rec.Body.String())
	}
	if captured == nil {
		t.Fatal("no upstream request captured")
	}
	if got := captured.URL.String(); got != "https://bedrock-runtime.us-east-1.amazonaws.com/model/us.anthropic.claude-fable-5/invoke" {
		t.Fatalf("upstream URL = %q", got)
	}
	if captured.Host != "bedrock-runtime.us-east-1.amazonaws.com" {
		t.Fatalf("upstream Host = %q", captured.Host)
	}
	auth := captured.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		t.Fatalf("Authorization = %q, want SigV4 signature", auth)
	}
	if !strings.Contains(auth, "/us-east-1/bedrock/aws4_request") {
		t.Fatalf("Authorization scope missing bedrock/us-east-1: %q", auth)
	}
	if captured.Header.Get("X-Amz-Date") == "" {
		t.Fatal("missing X-Amz-Date on signed request")
	}
	if capturedBody != `{"anthropic_version":"bedrock-2023-05-31"}` {
		t.Fatalf("forwarded body = %q", capturedBody)
	}
}

func TestBedrockHandlerRequiresGatewayToken(t *testing.T) {
	forwarded := false
	rt := bedrockRoundTripFunc(func(*http.Request) (*http.Response, error) {
		forwarded = true
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	s := Server{Bedrock: &BedrockConfig{Regions: []string{"us-east-1"}, Credentials: staticBedrockCreds(), Transport: rt, GatewayToken: "secret-token"}}
	h := s.bedrockHandler()

	// Missing token -> 401, never forwarded.
	req := httptest.NewRequest(http.MethodPost, "/bedrock/model/x/invoke", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", rec.Code)
	}
	if forwarded {
		t.Fatal("request forwarded despite missing gateway token")
	}

	// Correct bearer token -> forwarded.
	req = httptest.NewRequest(http.MethodPost, "/bedrock/model/x/invoke", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer secret-token")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !forwarded {
		t.Fatalf("valid token status = %d forwarded = %v, want 200 + forwarded", rec.Code, forwarded)
	}
}

func TestBedrockHandlerRetriesNextSourceOnThrottle(t *testing.T) {
	var accessKeys []string
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		auth := req.Header.Get("Authorization")
		switch {
		case strings.Contains(auth, "Credential=AKIAONE/"):
			accessKeys = append(accessKeys, "AKIAONE")
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Body:       io.NopCloser(strings.NewReader(`{"error":"throttled"}`)),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
			}, nil
		case strings.Contains(auth, "Credential=AKIATWO/"):
			accessKeys = append(accessKeys, "AKIATWO")
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
			}, nil
		default:
			t.Fatalf("unexpected Authorization header: %q", auth)
			return nil, nil
		}
	})
	s := Server{Bedrock: &BedrockConfig{
		Regions: []string{"us-east-1"},
		Sources: []BedrockCredentialSource{
			{Name: "aw0", Credentials: namedBedrockCreds("AKIAONE")},
			{Name: "aw1", Credentials: namedBedrockCreds("AKIATWO")},
		},
		Transport: rt,
	}}

	req := httptest.NewRequest(http.MethodPost, "/bedrock/model/us.anthropic.claude-fable-5/invoke", strings.NewReader(`{"anthropic_version":"bedrock-2023-05-31"}`))
	rec := httptest.NewRecorder()
	s.bedrockHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != `{"ok":true}` {
		t.Fatalf("body = %q, want successful second source response", rec.Body.String())
	}
	if got := strings.Join(accessKeys, ","); got != "AKIAONE,AKIATWO" {
		t.Fatalf("access key order = %s, want AKIAONE,AKIATWO", got)
	}
}

func TestBedrockHandlerRetriesNextRegionOnThrottle(t *testing.T) {
	var attempts []*http.Request
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts = append(attempts, req)
		switch req.Host {
		case "bedrock-runtime.us-east-1.amazonaws.com":
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Body:       io.NopCloser(strings.NewReader(`{"error":"throttled"}`)),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
			}, nil
		case "bedrock-runtime.us-west-2.amazonaws.com":
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
			}, nil
		default:
			t.Fatalf("unexpected host: %s", req.Host)
			return nil, nil
		}
	})
	s := Server{Bedrock: &BedrockConfig{
		Regions:   []string{"us-east-1", "us-west-2"},
		Sources:   []BedrockCredentialSource{{Name: "aw0", Credentials: staticBedrockCreds()}},
		Transport: rt,
	}}

	req := httptest.NewRequest(http.MethodPost, "/bedrock/model/us.anthropic.claude-fable-5/invoke", strings.NewReader(`{"anthropic_version":"bedrock-2023-05-31"}`))
	rec := httptest.NewRecorder()
	s.bedrockHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(attempts))
	}
	second := attempts[1]
	if second.Host != "bedrock-runtime.us-west-2.amazonaws.com" {
		t.Fatalf("second Host = %q, want us-west-2 Bedrock runtime", second.Host)
	}
	if got := second.URL.String(); got != "https://bedrock-runtime.us-west-2.amazonaws.com/model/us.anthropic.claude-fable-5/invoke" {
		t.Fatalf("second URL = %q", got)
	}
	if auth := second.Header.Get("Authorization"); !strings.Contains(auth, "/us-west-2/bedrock/aws4_request") {
		t.Fatalf("Authorization scope missing bedrock/us-west-2: %q", auth)
	}
}

func TestBedrockHandlerRoundRobinStartsAcrossRegionSourcePairs(t *testing.T) {
	var starts []string
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		auth := req.Header.Get("Authorization")
		accessKey := ""
		switch {
		case strings.Contains(auth, "Credential=AKIAONE/"):
			accessKey = "AKIAONE"
		case strings.Contains(auth, "Credential=AKIATWO/"):
			accessKey = "AKIATWO"
		default:
			t.Fatalf("unexpected Authorization header: %q", auth)
		}
		starts = append(starts, req.Host+"/"+accessKey)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})
	s := Server{Bedrock: &BedrockConfig{
		Regions: []string{"us-east-1", "us-west-2"},
		Sources: []BedrockCredentialSource{
			{Name: "aw0", Credentials: namedBedrockCreds("AKIAONE")},
			{Name: "aw1", Credentials: namedBedrockCreds("AKIATWO")},
		},
		Transport: rt,
	}}

	for i := 0; i < 4; i++ {
		req := httptest.NewRequest(http.MethodPost, "/bedrock/model/us.anthropic.claude-fable-5/invoke", strings.NewReader(`{"anthropic_version":"bedrock-2023-05-31"}`))
		rec := httptest.NewRecorder()
		s.bedrockHandler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200 (body %q)", i, rec.Code, rec.Body.String())
		}
	}

	want := strings.Join([]string{
		"bedrock-runtime.us-east-1.amazonaws.com/AKIAONE",
		"bedrock-runtime.us-east-1.amazonaws.com/AKIATWO",
		"bedrock-runtime.us-west-2.amazonaws.com/AKIAONE",
		"bedrock-runtime.us-west-2.amazonaws.com/AKIATWO",
	}, ",")
	if got := strings.Join(starts, ","); got != want {
		t.Fatalf("start order = %s, want %s", got, want)
	}
}

func TestBedrockConfigConfiguredRequiresRegionsAndSources(t *testing.T) {
	if !(&BedrockConfig{Regions: []string{"us-east-1"}, Credentials: staticBedrockCreds()}).configured() {
		t.Fatal("expected config with region and credentials to be configured")
	}
	if (&BedrockConfig{Credentials: staticBedrockCreds()}).configured() {
		t.Fatal("config without regions should not be configured")
	}
	if (&BedrockConfig{Regions: []string{"us-east-1"}}).configured() {
		t.Fatal("config without sources should not be configured")
	}
	if got := (&BedrockConfig{Regions: []string{"us-west-2"}, Credentials: staticBedrockCreds()}).primaryRegion(); got != "us-west-2" {
		t.Fatalf("primaryRegion = %q, want us-west-2", got)
	}
}

func TestBedrockHandlerLogsClientAttributionOnThrottle(t *testing.T) {
	var logBuf bytes.Buffer
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader(`{"error":"throttled"}`)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})
	s := Server{
		Logger: slog.New(slog.NewTextHandler(&logBuf, nil)),
		Bedrock: &BedrockConfig{
			Regions:   []string{"us-east-1"},
			Sources:   []BedrockCredentialSource{{Name: "aw0", Credentials: namedBedrockCreds("AKIAONE")}},
			Transport: rt,
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/bedrock/model/us.anthropic.claude-fable-5/invoke", strings.NewReader(`{"anthropic_version":"bedrock-2023-05-31"}`))
	req.Header.Set("User-Agent", "sr claude-aws")
	rec := httptest.NewRecorder()
	s.bedrockHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body %q)", rec.Code, rec.Body.String())
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "bedrock throttled") || !strings.Contains(logs, "remote_addr=") || !strings.Contains(logs, "user_agent=\"sr claude-aws\"") {
		t.Fatalf("expected throttle log with remote_addr and user_agent; logs=\n%s", logs)
	}
}

func TestBedrockHandlerDisabledWithoutConfig(t *testing.T) {
	s := Server{}
	rec := httptest.NewRecorder()
	s.bedrockHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/bedrock/model/x/invoke", strings.NewReader("{}")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when bedrock not configured", rec.Code)
	}
}

func TestBedrockHandlerRetriesNextSourceOn5xx(t *testing.T) {
	var accessKeys []string
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		auth := req.Header.Get("Authorization")
		switch {
		case strings.Contains(auth, "Credential=AKIAONE/"):
			accessKeys = append(accessKeys, "AKIAONE")
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       io.NopCloser(strings.NewReader(`{"message":"Bedrock is unable to process your request."}`)),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
			}, nil
		case strings.Contains(auth, "Credential=AKIATWO/"):
			accessKeys = append(accessKeys, "AKIATWO")
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
			}, nil
		default:
			t.Fatalf("unexpected Authorization header: %q", auth)
			return nil, nil
		}
	})
	s := Server{Bedrock: &BedrockConfig{
		Regions: []string{"us-east-1"},
		Sources: []BedrockCredentialSource{
			{Name: "aw0", Credentials: namedBedrockCreds("AKIAONE")},
			{Name: "aw1", Credentials: namedBedrockCreds("AKIATWO")},
		},
		Transport: rt,
	}}

	req := httptest.NewRequest(http.MethodPost, "/bedrock/model/us.anthropic.claude-fable-5/invoke", strings.NewReader(`{"anthropic_version":"bedrock-2023-05-31"}`))
	rec := httptest.NewRecorder()
	s.bedrockHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := strings.Join(accessKeys, ","); got != "AKIAONE,AKIATWO" {
		t.Fatalf("access key order = %s, want AKIAONE,AKIATWO", got)
	}
}
