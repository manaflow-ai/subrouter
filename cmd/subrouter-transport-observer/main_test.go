package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestObserverRecordsActualHTTPAndWebSocketHandshakesWithoutHeaderValues(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	var events bytes.Buffer
	handler := newObserverHandler(upstreamURL, &events)

	httpRequest := httptest.NewRequest(http.MethodPost, "http://observer/v1/responses", strings.NewReader("request body"))
	httpRequest.Header.Set("Authorization", "Bearer must-not-appear")
	handler.ServeHTTP(httptest.NewRecorder(), httpRequest)

	websocketRequest := httptest.NewRequest(http.MethodGet, "http://observer/v1/responses", nil)
	websocketRequest.Header.Set("Connection", "Upgrade")
	websocketRequest.Header.Set("Upgrade", "websocket")
	websocketRequest.Header.Set("Authorization", "Bearer must-not-appear")
	handler.ServeHTTP(httptest.NewRecorder(), websocketRequest)

	got := events.String()
	for _, want := range []string{
		`"transport":"http"`,
		`"transport":"websocket"`,
		`"method":"POST"`,
		`"method":"GET"`,
		`"path":"/v1/responses"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("observer events missing %q:\n%s", want, got)
		}
	}
	for _, secret := range []string{"must-not-appear", "request body", "Authorization"} {
		if strings.Contains(got, secret) {
			t.Fatalf("observer events leaked %q:\n%s", secret, got)
		}
	}
}

func TestObserverPreservesPublicUpstreamHostAndTenantPath(t *testing.T) {
	var gotHost string
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		gotHost = request.Host
		gotPath = request.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL + "/t/srt_0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}

	handler := newObserverHandler(upstreamURL, &bytes.Buffer{})
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:49152/v1/responses", nil)
	handler.ServeHTTP(httptest.NewRecorder(), request)

	if gotHost != upstreamURL.Host {
		t.Fatalf("upstream Host = %q, want %q", gotHost, upstreamURL.Host)
	}
	if gotPath != "/t/srt_0123456789abcdef0123456789abcdef/v1/responses" {
		t.Fatalf("upstream path = %q", gotPath)
	}
}

func TestValidateObserverUpstreamAcceptsPublicHTTPSOrigin(t *testing.T) {
	for _, rawURL := range []string{
		"http://127.0.0.1:31415",
		"https://staging.sr.cmux.com/t/srt_0123456789abcdef0123456789abcdef",
	} {
		upstream, err := url.Parse(rawURL)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateObserverUpstream(upstream); err != nil {
			t.Fatalf("validate %s: %v", rawURL, err)
		}
	}

	for _, rawURL := range []string{"file:///tmp/subrouter", "http:///missing-host"} {
		upstream, err := url.Parse(rawURL)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateObserverUpstream(upstream); err == nil {
			t.Fatalf("validate %s unexpectedly succeeded", rawURL)
		}
	}
}
