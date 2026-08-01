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
