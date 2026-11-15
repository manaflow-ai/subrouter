package qwen

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestConsoleAuthorizationAcceptsOfficialCallbackFormats(t *testing.T) {
	for _, format := range []string{"json", "nested-json", "form", "multipart", "query"} {
		t.Run(format, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var out bytes.Buffer
			credential, err := AuthorizeConsole(ctx, &out, func(authorizationURL string) {
				callback := consoleTestCallbackURL(t, authorizationURL)
				var body io.Reader
				contentType := "application/json"
				method := http.MethodPost
				switch format {
				case "json":
					body = strings.NewReader(`{"accessToken":"console-secret","consoleSite":"international","consoleRegion":"ap-southeast-1","apiKey":"discard-model-secret"}`)
				case "nested-json":
					body = strings.NewReader(`{"data":{"access_token":"console-secret"}}`)
				case "form":
					body = strings.NewReader("access_token=console-secret&console_switch_agent=123")
					contentType = "application/x-www-form-urlencoded"
				case "multipart":
					var buffer bytes.Buffer
					writer := multipart.NewWriter(&buffer)
					if err := writer.WriteField("access_token", "console-secret"); err != nil {
						t.Fatal(err)
					}
					if err := writer.Close(); err != nil {
						t.Fatal(err)
					}
					body, contentType = &buffer, writer.FormDataContentType()
				case "query":
					method = http.MethodGet
					callback += "&access_token=console-secret"
				}
				req, err := http.NewRequestWithContext(ctx, method, callback, body)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Origin", consoleOrigin)
				req.Header.Set("Content-Type", contentType)
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				bodyBytes, _ := io.ReadAll(res.Body)
				if res.StatusCode != http.StatusOK || strings.Contains(string(bodyBytes), "console-secret") || res.Header.Get("Cache-Control") != "no-store" {
					t.Fatalf("callback status=%d body=%q headers=%v", res.StatusCode, bodyBytes, res.Header)
				}
			})
			if err != nil || credential.AccessToken != "console-secret" || credential.ConsoleSite != "international" || credential.ConsoleRegion != "ap-southeast-1" {
				t.Fatalf("credential missing or wrong scope: err=%v", err)
			}
			if format == "form" && (credential.ConsoleSwitchAgent == nil || *credential.ConsoleSwitchAgent != 123) {
				t.Fatal("console account selector was lost")
			}
			if strings.Contains(out.String(), "console-secret") || strings.Contains(out.String(), "discard-model-secret") || !strings.Contains(out.String(), "beyond quota reads") {
				t.Fatal("authorization output leaked a credential or omitted the privilege disclosure")
			}
		})
	}
}

func TestConsoleAuthorizationRejectsUnsafeCallbacksWithoutConsumingLogin(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := AuthorizeConsole(ctx, io.Discard, func(authorizationURL string) {
		callback := consoleTestCallbackURL(t, authorizationURL)
		for _, test := range []struct {
			name, origin, body, host string
			badState                 bool
			want                     int
		}{
			{name: "wrong origin", origin: "https://example.com", body: `{"access_token":"do-not-log-secret"}`, want: http.StatusForbidden},
			{name: "wrong state", badState: true, body: `{"access_token":"do-not-log-secret"}`, want: http.StatusBadRequest},
			{name: "wrong host", host: "attacker.example", body: `{"access_token":"do-not-log-secret"}`, want: http.StatusForbidden},
			{name: "oversized body", body: strings.Repeat("x", consoleCallbackMaxBytes+1), want: http.StatusBadRequest},
			{name: "wrong region", body: `{"access_token":"do-not-log-secret","console_region":"attacker.example"}`, want: http.StatusBadRequest},
			{name: "control character", body: `{"access_token":"do-not-log-secret\nheader"}`, want: http.StatusBadRequest},
			{name: "missing token", body: `{}`, want: http.StatusBadRequest},
		} {
			target := callback
			if test.badState {
				target += "wrong"
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(test.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			if test.origin != "" {
				req.Header.Set("Origin", test.origin)
			}
			if test.host != "" {
				req.Host = test.host
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != test.want || strings.Contains(string(body), "do-not-log-secret") {
				t.Fatalf("%s: status=%d body=%q", test.name, res.StatusCode, body)
			}
		}
		preflight, _ := http.NewRequestWithContext(ctx, http.MethodOptions, callback, nil)
		preflight.Header.Set("Origin", consoleOrigin)
		preflight.Header.Set("Access-Control-Request-Private-Network", "true")
		res, err := http.DefaultClient.Do(preflight)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNoContent || res.Header.Get("Access-Control-Allow-Origin") != consoleOrigin || res.Header.Get("Access-Control-Allow-Private-Network") != "true" {
			t.Fatal("official console private-network preflight was rejected")
		}
		res, err = http.PostForm(callback, url.Values{"access_token": {"console-secret"}})
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("valid callback after rejected requests: %d", res.StatusCode)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestConsoleAuthorizationCancellationClosesListener(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var callback string
	_, err := AuthorizeConsole(ctx, io.Discard, func(target string) {
		callback = consoleTestCallbackURL(t, target)
		cancel()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	client := &http.Client{Timeout: time.Second}
	res, err := client.Get(callback)
	if err == nil {
		res.Body.Close()
		t.Fatal("cancelled listener remained reachable")
	}
}

func consoleTestCallbackURL(t *testing.T, authorizationURL string) string {
	t.Helper()
	parsed, err := url.Parse(authorizationURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "modelstudio.console.alibabacloud.com" || parsed.Path != "/console-login" {
		t.Fatalf("wrong official authorization URL: %v", err)
	}
	callback := "http://" + parsed.Query().Get("notice")
	parsedCallback, err := url.Parse(callback)
	if err != nil || parsedCallback.Hostname() != "127.0.0.1" || parsedCallback.Query().Get("state") == "" {
		t.Fatalf("invalid localhost callback: %v", err)
	}
	return callback
}
