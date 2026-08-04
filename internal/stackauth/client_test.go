package stackauth

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNativeCLIFlowAndRefresh(t *testing.T) {
	var sawStackHeaders bool
	var refreshGrantType, refreshClientID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Stack-Project-Id") == "project" &&
			r.Header.Get("X-Hexclave-Project-Id") == "project" {
			sawStackHeaders = true
		}
		switch r.URL.Path {
		case "/auth/cli":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"polling_code": "poll",
				"login_code":   "login",
				"expires_at":   time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
			})
		case "/auth/cli/poll":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status":        "success",
				"refresh_token": "refresh",
			})
		case "/auth/oauth/token":
			if err := r.ParseForm(); err != nil {
				http.Error(w, "invalid form", http.StatusBadRequest)
				return
			}
			refreshGrantType = r.Form.Get("grant_type")
			refreshClientID = r.Form.Get("client_id")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"access_token":  "access",
				"refresh_token": "rotated",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := Client{
		APIURL:               server.URL,
		ProjectID:            "project",
		PublishableClientKey: "pck_test",
		HTTPClient:           server.Client(),
	}
	start, err := client.StartCLI(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if start.PollingCode != "poll" || start.LoginCode != "login" {
		t.Fatalf("start = %#v", start)
	}
	poll, err := client.PollCLI(context.Background(), start.PollingCode)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := client.Refresh(context.Background(), poll.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != "access" || tokens.RefreshToken != "rotated" {
		t.Fatalf("tokens = %#v", tokens)
	}
	if refreshGrantType != "refresh_token" || refreshClientID != "project" {
		t.Fatalf("refresh form = grant_type %q, client_id %q", refreshGrantType, refreshClientID)
	}
	if !sawStackHeaders {
		t.Fatal("native Stack/Hexclave client headers were not sent")
	}
}

func TestRetryableRecognizesTimeoutAndConnectionFailuresWithoutTemporary(t *testing.T) {
	for _, err := range []error{
		&net.DNSError{IsTimeout: true},
		syscall.ECONNRESET,
		syscall.ECONNREFUSED,
	} {
		if !Retryable(err) {
			t.Fatalf("%v was not retryable", err)
		}
	}
	if Retryable(syscall.EINVAL) {
		t.Fatal("unrelated syscall error was retryable")
	}
}

func TestFetchPublicConfigRejectsIncompleteResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":1}`))
	}))
	defer server.Close()
	_, err := FetchPublicConfig(context.Background(), server.Client(), server.URL)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchPublicConfigExplainsUnavailableCLIAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"cli_auth_unavailable"}`))
	}))
	defer server.Close()

	_, err := FetchPublicConfig(context.Background(), server.Client(), server.URL)
	if err == nil || !strings.Contains(err.Error(), "cmux.com login is temporarily unavailable") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "Stack") {
		t.Fatalf("provider leaked through public error: %v", err)
	}
}

func TestExchangeTenantUsesBrokeredURLAndNativeSessionPair(t *testing.T) {
	var sawRequest bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/subrouter/exchange" {
			http.NotFound(w, r)
			return
		}
		sawRequest = true
		if r.Header.Get("Authorization") != "Bearer access" ||
			r.Header.Get("X-Stack-Refresh-Token") != "refresh" ||
			r.Header.Get("X-Cmux-Team-Id") != "team-1" {
			http.Error(w, "missing native session", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tenantId": "team-1", "tenantName": "Acme",
			"tenantKey":    "srt_0123456789abcdef0123456789abcdef",
			"proxyUrl":     serverURL(r) + "/t/srt_0123456789abcdef0123456789abcdef",
			"capabilities": []string{"use"},
		})
	}))
	defer server.Close()

	exchange, err := ExchangeTenant(
		context.Background(), server.Client(),
		server.URL+"/api/subrouter/exchange",
		"access", "refresh", "team-1", "Acme",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !sawRequest || len(exchange.Capabilities) != 1 ||
		exchange.Capabilities[0] != "use" {
		t.Fatalf("exchange = %#v", exchange)
	}
}

func TestExchangeTenantDoesNotForwardRefreshTokenAcrossRedirects(t *testing.T) {
	var leakedRefreshToken string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leakedRefreshToken = r.Header.Get("X-Stack-Refresh-Token")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tenantId": "team-1", "tenantName": "Acme",
			"tenantKey":    "srt_0123456789abcdef0123456789abcdef",
			"proxyUrl":     serverURL(r) + "/t/srt_0123456789abcdef0123456789abcdef",
			"capabilities": []string{"use"},
		})
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	_, err := ExchangeTenant(
		context.Background(), redirect.Client(), redirect.URL,
		"access", "refresh", "team-1", "Acme",
	)
	if err == nil {
		t.Fatal("cross-origin redirect unexpectedly succeeded")
	}
	if leakedRefreshToken != "" {
		t.Fatal("refresh token was forwarded to the redirect target")
	}
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
}
