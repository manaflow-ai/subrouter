package qwen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestFetchUsageAndSubscription(t *testing.T) {
	t.Setenv("SUBROUTER_STATE_DIR", t.TempDir())
	writeTestConsoleConfig(t, "qwen-token:work", map[string]any{
		"access_token":   "console-secret",
		"console_region": "ap-southeast-1",
		"console_site":   "international",
	})
	reset := time.Now().Add(2 * time.Hour).UnixMilli()
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "Bearer console-secret" {
			t.Fatal("console authorization was not sent")
		}
		api := req.URL.Query().Get("api")
		var body string
		switch api {
		case usageAPI:
			body = `{"data":{"DataV2":{"data":{"data":[{"per1WeekPercentage":0.25,"per1WeekResetTime":` + jsonNumber(reset) + `}]}}}}`
		case subscriptionAPI:
			body = `{"data":{"DataV2":{"data":{"data":[{"specCode":"pro","status":"VALID","instanceCode":"instance-123","startTime":1000,"endTime":2000}]}}}}`
		default:
			t.Fatalf("unexpected API %q", api)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}

	usage, err := FetchUsage(context.Background(), client, "qwen-token:work")
	if err != nil {
		t.Fatal(err)
	}
	if usage.FiveHour != nil {
		t.Fatal("an omitted 5h limit should remain nil/unknown")
	}
	if usage.Weekly == nil || usage.Weekly.UsedPercent != 25 || usage.Weekly.ResetAfterSeconds <= 0 {
		t.Fatalf("weekly usage = %+v", usage.Weekly)
	}
	subscription, err := FetchSubscription(context.Background(), client, "qwen-token:work")
	if err != nil {
		t.Fatal(err)
	}
	if subscription.Plan != "Pro" || subscription.Status != "valid" || subscription.InstanceCode != "instance-123" {
		t.Fatalf("subscription = %+v", subscription)
	}
}

func TestConsoleRequestDoesNotFollowRedirectWithBearerToken(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if requests > 1 {
			t.Fatal("Qwen console client followed a credential-bearing redirect")
		}
		return &http.Response{
			StatusCode: http.StatusTemporaryRedirect,
			Header:     http.Header{"Location": []string{"https://redirect.example/steal"}},
			Body:       io.NopCloser(strings.NewReader("redirect")),
			Request:    req,
		}, nil
	})}
	err := callConsole(context.Background(), client, consoleConfig{
		AccessToken: "console-secret", ConsoleRegion: defaultRegion, ConsoleSite: defaultSite,
	}, usageAPI, nil, new(any))
	if err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("redirect response error = %v", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want exactly the original request", requests)
	}
}

func TestConsoleRequestClassifiesExpiredLoginResponse(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"code":"BailianGateway.Login.NotLogined","message":"login expired"}`,
			)),
			Request: req,
		}, nil
	})}
	err := callConsole(context.Background(), client, consoleConfig{
		AccessToken: "expired-console-token", ConsoleRegion: defaultRegion, ConsoleSite: defaultSite,
	}, usageAPI, nil, new(any))
	if !errors.Is(err, ErrConsoleLoginRequired) {
		t.Fatalf("console error = %v, want ErrConsoleLoginRequired", err)
	}
}

func TestUsageAndSubscriptionParsersClassifyEmbeddedExpiredLogin(t *testing.T) {
	for name, parse := range map[string]func(any) error{
		"usage":        func(value any) error { _, err := findUsagePayload(value); return err },
		"subscription": func(value any) error { _, err := findSubscriptionDetails(value); return err },
	} {
		t.Run(name, func(t *testing.T) {
			err := parse(map[string]any{"data": map[string]any{
				"success": false, "errorCode": "BailianGateway.Login.NotLogined",
			}})
			if !errors.Is(err, ErrConsoleLoginRequired) {
				t.Fatalf("error = %v, want ErrConsoleLoginRequired", err)
			}
		})
	}
}

func TestConsoleRequestDoesNotClassifyLoginMarkerOutsideExactCode(t *testing.T) {
	for _, body := range []string{
		`{"code":"Wrapper.BailianGateway.Login.NotLogined","message":"different error"}`,
		`{"code":"Other.Error","message":"BailianGateway.Login.NotLogined"}`,
		`{"data":{"message":"BailianGateway.Login.NotLogined"}}`,
	} {
		client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(body)), Request: req,
			}, nil
		})}
		var output any
		err := callConsole(context.Background(), client, consoleConfig{
			AccessToken: "console-token", ConsoleRegion: defaultRegion, ConsoleSite: defaultSite,
		}, usageAPI, nil, &output)
		if errors.Is(err, ErrConsoleLoginRequired) {
			t.Fatalf("response %s was misclassified as an expired login", body)
		}
	}
}

func TestStatusErrorDeduplicatesAndExplainsExpiredLogin(t *testing.T) {
	expired := fmt.Errorf("usage: %w", ErrConsoleLoginRequired)
	err := StatusError("qwen-token:work", expired, expired)
	if err == nil {
		t.Fatal("expired login errors were discarded")
	}
	message := err.Error()
	if strings.Count(message, "login needed") != 1 ||
		!strings.Contains(message, "sr qwen login 'qwen-token:work'") {
		t.Fatalf("status error = %q", message)
	}
}

func TestStatusErrorShellQuotesAccountID(t *testing.T) {
	err := StatusError("qwen-token:team's $(unsafe); value", ErrConsoleLoginRequired)
	want := `sr qwen login 'qwen-token:team'"'"'s $(unsafe); value'`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("status error = %q, want safely quoted command %q", err, want)
	}
}

func TestConsoleAccountRejectsTerminalControl(t *testing.T) {
	root := t.TempDir()
	if err := SetConsoleAccountIn(root, "qwen-token:work", "unsafe\x1b[31m"); err == nil {
		t.Fatal("terminal control sequence was accepted as a console account label")
	}
	if got := ConsoleAccountIn(root, "qwen-token:work"); got != "" {
		t.Fatalf("unsafe label was persisted as %q", got)
	}
}

func TestConsoleLoginConfigIsIsolatedAndStripsTemporaryKey(t *testing.T) {
	t.Setenv("SUBROUTER_STATE_DIR", t.TempDir())
	accountID := "qwen-token:work"
	if err := PrepareConsoleLogin(accountID, "model-secret", "https://example.test/v1"); err != nil {
		t.Fatal(err)
	}
	config := readTestConsoleConfig(t, accountID)
	if config["api_key"] != "model-secret" {
		t.Fatal("temporary model key was not prepared")
	}
	config["access_token"] = "console-secret"
	writeTestConsoleConfig(t, accountID, config)
	if err := FinishConsoleLogin(accountID); err != nil {
		t.Fatal(err)
	}
	config = readTestConsoleConfig(t, accountID)
	if _, ok := config["api_key"]; ok {
		t.Fatal("model key remained in the console profile")
	}
	if config["access_token"] != "console-secret" {
		t.Fatal("console token was not preserved")
	}
	if err := SetConsoleAccount(accountID, "person@example.com"); err != nil {
		t.Fatal(err)
	}
	if got := ConsoleAccount(accountID); got != "person@example.com" {
		t.Fatalf("console account = %q", got)
	}
	if err := SetConsoleAccount(accountID, ""); err != nil {
		t.Fatal(err)
	}
	if got := ConsoleAccount(accountID); got != "" {
		t.Fatalf("empty label should clear stale identity, got %q", got)
	}
	other := ConsoleConfigPath("qwen-token:other")
	if other == ConsoleConfigPath(accountID) || filepath.Dir(other) == filepath.Dir(ConsoleConfigPath(accountID)) {
		t.Fatal("different accounts must use different console profiles")
	}
}

func TestStripTemporaryLoginKeyDoesNotCreateMissingProfile(t *testing.T) {
	root := t.TempDir()
	accountID := "qwen-token:missing"
	if err := StripTemporaryLoginKeyIn(root, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ConsoleConfigDirIn(root, accountID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing profile was created: %v", err)
	}
}

func TestPrepareConsoleLoginCannotReuseStaleAccessToken(t *testing.T) {
	t.Setenv("SUBROUTER_STATE_DIR", t.TempDir())
	accountID := "qwen-token:work"
	writeTestConsoleConfig(t, accountID, map[string]any{"access_token": "stale-console-token"})
	if err := PrepareConsoleLogin(accountID, "model-secret", "https://example.test/v1"); err != nil {
		t.Fatal(err)
	}
	config := readTestConsoleConfig(t, accountID)
	if _, ok := config["access_token"]; ok {
		t.Fatal("prepare retained a stale console access token")
	}
	if err := FinishConsoleLogin(accountID); err == nil {
		t.Fatal("login without a newly issued console token was accepted")
	}
	config = readTestConsoleConfig(t, accountID)
	if config["access_token"] != "stale-console-token" {
		t.Fatal("failed reauthorization did not restore the previous working token")
	}
	if _, ok := config[previousAccessTokenKey]; ok {
		t.Fatal("failed reauthorization retained internal staging state")
	}
}

func TestConsoleCredentialRootsAreIsolatedAndRemovalIsScoped(t *testing.T) {
	rootA := filepath.Join(t.TempDir(), "tenant-a")
	rootB := filepath.Join(t.TempDir(), "tenant-b")
	accountID := "qwen-token:shared-label"
	if err := SaveConsoleCredentialIn(rootA, accountID, ConsoleCredential{AccessToken: "tenant-a-token", Account: "a@example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := SaveConsoleCredentialIn(rootB, accountID, ConsoleCredential{AccessToken: "tenant-b-token", Account: "b@example.com"}); err != nil {
		t.Fatal(err)
	}
	credentialA, err := ExportConsoleCredentialIn(rootA, accountID)
	if err != nil {
		t.Fatal(err)
	}
	credentialB, err := ExportConsoleCredentialIn(rootB, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if credentialA.AccessToken != "tenant-a-token" || credentialA.Account != "a@example.com" {
		t.Fatalf("tenant A credential = %+v", credentialA)
	}
	if credentialB.AccessToken != "tenant-b-token" || credentialB.Account != "b@example.com" {
		t.Fatalf("tenant B credential = %+v", credentialB)
	}
	if err := RemoveConsoleCredentialIn(rootA, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := ExportConsoleCredentialIn(rootA, accountID); err == nil {
		t.Fatal("removed tenant A credential remains readable")
	}
	credentialB, err = ExportConsoleCredentialIn(rootB, accountID)
	if err != nil || credentialB.AccessToken != "tenant-b-token" {
		t.Fatalf("tenant B credential was affected: credential=%+v err=%v", credentialB, err)
	}
}

func TestConsoleScopeRootsDoNotExposeOrCollideOnScope(t *testing.T) {
	scopeA := "remote\x00server-a\x00https://one.example"
	scopeB := "remote\x00server-b\x00https://two.example"
	rootA := ConsoleRootForScope(scopeA)
	rootB := ConsoleRootForScope(scopeB)
	if rootA == rootB {
		t.Fatal("different remote scopes shared a console root")
	}
	if strings.Contains(rootA, "one.example") || strings.Contains(rootB, "two.example") {
		t.Fatal("remote scope identity leaked into the console path")
	}
}

func writeTestConsoleConfig(t *testing.T, accountID string, config map[string]any) {
	t.Helper()
	if err := os.MkdirAll(ConsoleConfigDir(accountID), 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ConsoleConfigPath(accountID), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readTestConsoleConfig(t *testing.T, accountID string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(ConsoleConfigPath(accountID))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(body, &config); err != nil {
		t.Fatal(err)
	}
	return config
}

func jsonNumber(value int64) string {
	return strconv.FormatInt(value, 10)
}
