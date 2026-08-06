package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestObserverFailsClosedWhenGoldenRequestSignalWriteFails(t *testing.T) {
	previousHooks := goldenTestHooks
	goldenTestHooks.enabled = true
	signalCalled := make(chan string, 1)
	goldenTestHooks.outboundRequestWritten = func(token string) error {
		signalCalled <- token
		return errors.New("signal write failed")
	}
	t.Cleanup(func() { goldenTestHooks = previousHooks })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("upstream response must not escape"))
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	handler := newObserverHandler(upstreamURL, &bytes.Buffer{})
	token := "0123456789abcdef0123456789abcdef"
	request := httptest.NewRequest(http.MethodPost, "http://observer/v1/responses", strings.NewReader("request body"))
	request.Header.Set(goldenRequestTokenHeader, token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
	if strings.Contains(recorder.Body.String(), "upstream response must not escape") {
		t.Fatal("observer forwarded a response after the request signal write failed")
	}
	select {
	case got := <-signalCalled:
		if got != token {
			t.Fatalf("signal token = %q, want %q", got, token)
		}
	default:
		t.Fatal("outbound request completion callback was not invoked")
	}
}

func TestObserverStripsResponseAttemptTokenWithRequestSignalHook(t *testing.T) {
	const responseAttemptHeader = "X-Subrouter-Golden-Response-Attempt"
	previousHooks := goldenTestHooks
	goldenTestHooks.enabled = true
	goldenTestHooks.outboundRequestWritten = func(string) error { return nil }
	t.Cleanup(func() { goldenTestHooks = previousHooks })

	attemptTokenReachedUpstream := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		attemptTokenReachedUpstream = request.Header.Get(responseAttemptHeader) != ""
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	handler := newObserverHandler(upstreamURL, &bytes.Buffer{})
	request := httptest.NewRequest(http.MethodPost, "http://observer/v1/responses", strings.NewReader("request body"))
	request.Header.Set(goldenRequestTokenHeader, "0123456789abcdef0123456789abcdef")
	request.Header.Set(responseAttemptHeader, "fedcba9876543210fedcba9876543210")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
	if attemptTokenReachedUpstream {
		t.Fatal("golden response attempt token escaped the observer")
	}
}

func TestObserverRejectsInvalidGoldenRequestTokenBeforeUpstream(t *testing.T) {
	previousHooks := goldenTestHooks
	goldenTestHooks.enabled = true
	signalCalled := make(chan struct{}, 1)
	goldenTestHooks.outboundRequestWritten = func(string) error {
		signalCalled <- struct{}{}
		return nil
	}
	t.Cleanup(func() { goldenTestHooks = previousHooks })

	upstreamReached := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamReached <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	handler := newObserverHandler(upstreamURL, &bytes.Buffer{})
	request := httptest.NewRequest(http.MethodPost, "http://observer/v1/responses", strings.NewReader("request body"))
	request.Header.Set(goldenRequestTokenHeader, "../not-opaque")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
	select {
	case <-signalCalled:
		t.Fatal("invalid token reached the completion callback")
	default:
	}
	select {
	case <-upstreamReached:
		t.Fatal("invalid token reached the upstream")
	default:
	}
}

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

func TestObserverRecordsResponseStatusWithoutBodyContent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	var events bytes.Buffer
	handler := newObserverHandler(upstreamURL, &events)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://observer/v1/responses", nil))

	statusRecorded := false
	decoder := json.NewDecoder(&events)
	for decoder.More() {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event["kind"] == "response_status" && event["status_code"] == float64(http.StatusServiceUnavailable) {
			statusRecorded = true
		}
	}
	if !statusRecorded {
		t.Fatalf("response status missing from observer evidence:\n%s", events.String())
	}
	if strings.Contains(events.String(), "body") {
		t.Fatal("observer evidence recorded response body content")
	}
}

func TestObserverRecordsFinalResponseStatusAfterInformationalStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusEarlyHints)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("response-body-must-not-appear"))
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	var events bytes.Buffer
	handler := newObserverHandler(upstreamURL, &events)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://observer/v1/responses", nil))

	var statuses []int
	decoder := json.NewDecoder(&events)
	for decoder.More() {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event["kind"] == "response_status" {
			statuses = append(statuses, int(event["status_code"].(float64)))
		}
	}
	if len(statuses) != 1 || statuses[0] != http.StatusServiceUnavailable {
		t.Fatalf("recorded response statuses = %v, want [%d]\n%s", statuses, http.StatusServiceUnavailable, events.String())
	}
	if strings.Contains(events.String(), "response-body-must-not-appear") {
		t.Fatal("observer evidence recorded response body content")
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
