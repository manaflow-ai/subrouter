package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func switchTestServer(switchAccount func(context.Context, string, string) error) http.Handler {
	return Server{
		AdminToken:    "secret",
		MaxBodyBytes:  1024,
		SwitchAccount: switchAccount,
	}.Handler()
}

func postSwitch(handler http.Handler, remoteAddr, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/_subrouter/switch-account", strings.NewReader(body))
	req.RemoteAddr = remoteAddr
	if token != "" {
		req.Header.Set("X-Subrouter-Admin-Token", token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func TestSwitchAccountEndpointCallsCallbackAndReportsOK(t *testing.T) {
	var gotProvider, gotAccount string
	handler := switchTestServer(func(_ context.Context, provider, accountID string) error {
		gotProvider, gotAccount = provider, accountID
		return nil
	})
	recorder := postSwitch(handler, "100.64.0.2:12345", "secret", `{"provider":"codex","account_id":"a@example.com"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if gotProvider != "codex" || gotAccount != "a@example.com" {
		t.Fatalf("callback got (%q, %q)", gotProvider, gotAccount)
	}
	if !strings.Contains(recorder.Body.String(), `"ok": true`) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestSwitchAccountEndpointRequiresAdminTokenOffLoopback(t *testing.T) {
	handler := switchTestServer(func(_ context.Context, _, _ string) error { return nil })
	recorder := postSwitch(handler, "100.64.0.2:12345", "", `{"provider":"codex","account_id":"a@example.com"}`)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status without token = %d", recorder.Code)
	}
}

func TestSwitchAccountEndpointRejectsNonPost(t *testing.T) {
	handler := switchTestServer(func(_ context.Context, _, _ string) error { return nil })
	req := httptest.NewRequest(http.MethodGet, "/_subrouter/switch-account", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestSwitchAccountEndpointAnswers501WhenUnwired(t *testing.T) {
	handler := switchTestServer(nil)
	recorder := postSwitch(handler, "127.0.0.1:12345", "", `{"provider":"codex","account_id":"a@example.com"}`)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestSwitchAccountEndpointValidatesBody(t *testing.T) {
	called := false
	handler := switchTestServer(func(_ context.Context, _, _ string) error {
		called = true
		return nil
	})
	for name, body := range map[string]string{
		"not json":         `not-json`,
		"missing account":  `{"provider":"codex"}`,
		"missing provider": `{"account_id":"a@example.com"}`,
		"unknown provider": `{"provider":"gemini","account_id":"a@example.com"}`,
	} {
		recorder := postSwitch(handler, "127.0.0.1:12345", "", body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, body = %s", name, recorder.Code, recorder.Body.String())
		}
	}
	if called {
		t.Fatal("callback ran for an invalid request")
	}
}

func TestSwitchAccountEndpointMapsCallbackErrorTo422(t *testing.T) {
	handler := switchTestServer(func(_ context.Context, _, accountID string) error {
		return &accountNotFoundError{accountID}
	})
	recorder := postSwitch(handler, "127.0.0.1:12345", "", `{"provider":"claude","account_id":"nope"}`)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "nope") {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

type accountNotFoundError struct{ id string }

func (e *accountNotFoundError) Error() string { return "account " + e.id + " not found" }
