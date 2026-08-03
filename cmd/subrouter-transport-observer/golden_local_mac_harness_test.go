package main

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGoldenLocalMacHarnessOrchestratesAllModesWithoutContentEvidence(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a deterministic fake released client")
	}
	root := t.TempDir()
	fakeClient := filepath.Join(root, "released-subrouter")
	build := exec.Command("go", "build", "-o", fakeClient, "./testdata/golden_fake_client")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake released client: %v\n%s", err, output)
	}
	fakeClientData, err := os.ReadFile(fakeClient)
	if err != nil {
		t.Fatal(err)
	}
	fakeClientHash := sha256.Sum256(fakeClientData)
	assetName := "subrouter_9.9.9_darwin_arm64"
	release := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/latest":
			_, _ = w.Write([]byte(`{"tag_name":"v9.9.9"}`))
		case "/download/v9.9.9/" + assetName:
			_, _ = w.Write(fakeClientData)
		case "/download/v9.9.9/SHA256SUMS":
			_, _ = fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(fakeClientHash[:]), assetName)
		default:
			http.NotFound(w, request)
		}
	}))
	defer release.Close()

	hosted := httptest.NewServer(goldenFakeHostedHandler())
	defer hosted.Close()
	cloudPath := filepath.Join(root, "cloud.json")
	tenantKey := "srt_0123456789abcdef0123456789abcdef"
	cloud := map[string]any{
		"version": 1, "baseUrl": hosted.URL,
		"accessToken": "ACCESS_TOKEN_SECRET", "refreshToken": "REFRESH_TOKEN_SECRET",
		"localProxyToken": "LOCAL_PROXY_SECRET", "teamId": "team-golden", "teamName": "Golden",
		"credentialSource": "team", "hostedUrl": hosted.URL, "tenantKey": tenantKey,
	}
	data, _ := json.Marshal(cloud)
	if err := os.WriteFile(cloudPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(root, "codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"access_token":"CODEX_AUTH_SECRET"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	fakeBin := filepath.Join(root, "bin")
	if err := os.MkdirAll(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	lsofPath := filepath.Join(fakeBin, "lsof")
	lsofScript := `#!/bin/sh
pid=0
previous=
for value in "$@"; do
  if [ "$previous" = "-p" ]; then pid="$value"; fi
  previous="$value"
done
printf 'p%s\nf9\nn127.0.0.1:41000->203.0.113.10:443\nTST=ESTABLISHED\n' "$pid"
`
	if err := os.WriteFile(lsofPath, []byte(lsofScript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeBin, "pgrep"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	actionLog := filepath.Join(root, "actions.log")
	actionPath := filepath.Join(root, "action.sh")
	actionScript := `#!/bin/sh
test "$DEPLOY_ENV_SECRET" = "DEPLOY_ENV_VALUE_SECRET" || exit 9
printf '%s\n' "$1" >> "$ACTION_LOG"
sleep "$2"
`
	if err := os.WriteFile(actionPath, []byte(actionScript), 0o700); err != nil {
		t.Fatal(err)
	}
	enableGoldenTestMode(t, release.URL+"/latest", release.URL+"/download")
	t.Setenv("DEPLOY_ENV_SECRET", "DEPLOY_ENV_VALUE_SECRET")
	t.Setenv("ACTION_LOG", actionLog)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	artifacts := filepath.Join(root, "artifacts")
	err = runGolden([]string{
		"--cloud-config", cloudPath,
		"--codex-home", codexHome,
		"--codex-bin", fakeClient,
		"--artifact-dir", artifacts,
		"--stream-lines", "8",
		"--timeout", "15s",
		"--activate", actionPath, "activation", "0.9",
		"--rollback", actionPath, "rollback", "0.5",
		"--old-generation-check", actionPath, "cleanup", "0.01",
	})
	if err != nil {
		result, _ := os.ReadFile(filepath.Join(artifacts, "result.json"))
		t.Fatalf("golden harness: %v\n%s", err, result)
	}
	actions, err := os.ReadFile(actionLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(actions) != "activation\nrollback\ncleanup\n" {
		t.Fatalf("actions = %q", actions)
	}
	resultData, err := os.ReadFile(filepath.Join(artifacts, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result goldenSummary
	if err := json.Unmarshal(resultData, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Passed || !result.PrivateWorkspaceRemoved || !result.FreshLocalLeaseObserved ||
		!result.ReleaseChecksumVerified || result.ReleasedVersion != "9.9.9" || len(result.Sessions) != 6 {
		t.Fatalf("incomplete result: %#v", result)
	}
	allEvidence := readGoldenArtifacts(t, artifacts)
	for _, forbidden := range []string{
		"ACCESS_TOKEN_SECRET", "REFRESH_TOKEN_SECRET", "LOCAL_PROXY_SECRET", "CODEX_AUTH_SECRET",
		"DEPLOY_ENV_VALUE_SECRET", "REQUEST_BODY_SECRET", "REQUEST_HEADER_SECRET",
		"LEASE_REQUEST_BODY_SECRET", "LEASE_HEADER_SECRET", tenantKey,
		"healthy-response-secret", "response-body-not-recorded",
		"Do not use tools", "exact nonce from the first turn",
	} {
		if strings.Contains(allEvidence, forbidden) {
			t.Fatalf("content-blind evidence leaked %q", forbidden)
		}
	}
}

func goldenFakeHostedHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/_subrouter/health" || request.URL.Path == "/_subrouter/ready" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("healthy-response-secret"))
			return
		}
		if strings.HasSuffix(request.URL.Path, "/_subrouter/leases") {
			_, _ = io.Copy(io.Discard, request.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"lease":"response-body-not-recorded"}`))
			return
		}
		if !strings.HasSuffix(request.URL.Path, "/responses") {
			http.NotFound(w, request)
			return
		}
		goldenFakeStream(w, request)
	})
}

func goldenFakeStream(w http.ResponseWriter, request *http.Request) {
	duration := 8 * time.Second
	if request.Header.Get("X-Golden-Short") == "1" {
		duration = 120 * time.Millisecond
	}
	if strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
		connection, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer connection.Close()
		digest := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, _ = fmt.Fprintf(buffered, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(digest[:]))
		_ = buffered.Flush()
		deadline := time.Now().Add(duration)
		for time.Now().Before(deadline) {
			_, _ = connection.Write([]byte{0x81, 0x01, 'x'})
			time.Sleep(20 * time.Millisecond)
		}
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	flusher := w.(http.Flusher)
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		_, _ = w.Write([]byte("data:x\n\n"))
		flusher.Flush()
		time.Sleep(20 * time.Millisecond)
	}
}

func readGoldenArtifacts(t *testing.T, root string) string {
	t.Helper()
	var output strings.Builder
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			output.Write(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return output.String()
}

func TestObserverTemplatesTenantAndLeasePathsAndRecordsOnlyByteMetadata(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		_, _ = w.Write([]byte("RESPONSE_BODY_SECRET"))
	}))
	defer upstream.Close()
	parsed, _ := url.Parse(upstream.URL)
	var events strings.Builder
	handler := newObserverHandler(parsed, &events)
	request := httptest.NewRequest(http.MethodPost, "http://observer/t/srt_TENANT_SECRET/_subrouter/leases/LEASE_SECRET/events", strings.NewReader("REQUEST_BODY_SECRET"))
	request.Header.Set("Authorization", "Bearer HEADER_SECRET")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	evidence := events.String()
	for _, required := range []string{`"path":"/_subrouter/leases/:id/events"`, `"kind":"request_chunk"`, `"kind":"response_chunk"`, `"connection_id":"connection-`} {
		if !strings.Contains(evidence, required) {
			t.Fatalf("evidence missing %q:\n%s", required, evidence)
		}
	}
	for _, forbidden := range []string{"srt_TENANT_SECRET", "LEASE_SECRET", "REQUEST_BODY_SECRET", "RESPONSE_BODY_SECRET", "HEADER_SECRET", "Authorization"} {
		if strings.Contains(evidence, forbidden) {
			t.Fatalf("evidence leaked %q:\n%s", forbidden, evidence)
		}
	}
}

func TestGoldenSessionValidationRejectsEveryContinuityFailureClass(t *testing.T) {
	newSession := func() *goldenSession {
		return &goldenSession{
			threadID: "thread", threadIDCount: 1, marker: "marker", markerCount: 1,
			nonce: "nonce", nonceCount: 1, issues: map[string]int{}, exitCode: 0,
		}
	}
	tests := []struct {
		name string
		edit func(*goldenSession)
		want string
	}{
		{name: "nonzero", edit: func(s *goldenSession) { s.exitCode = 7 }, want: "codex_nonzero_exit"},
		{name: "duplicate marker", edit: func(s *goldenSession) { s.markerCount = 2 }, want: "duplicate_completion_marker"},
		{name: "missing marker", edit: func(s *goldenSession) { s.markerCount = 0 }, want: "completion_marker_missing"},
		{name: "missing nonce", edit: func(s *goldenSession) { s.nonceCount = 0 }, want: "nonce_context_missing"},
		{name: "reconnect", edit: func(s *goldenSession) { s.issues["reconnect"] = 1 }, want: "codex_transport_issue_reconnect"},
		{name: "retry", edit: func(s *goldenSession) { s.issues["retry"] = 1 }, want: "codex_transport_issue_retry"},
		{name: "fallback", edit: func(s *goldenSession) { s.issues["fallback"] = 1 }, want: "codex_transport_issue_fallback"},
		{name: "error", edit: func(s *goldenSession) { s.issues["error"] = 1 }, want: "codex_transport_issue_error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := newSession()
			test.edit(session)
			if got := fixedGoldenFailure(validateGoldenSessions([]*goldenSession{session}, false)); got != test.want {
				t.Fatalf("failure = %q, want %q", got, test.want)
			}
		})
	}
}

func TestGoldenReleasedClientRejectsChecksumMismatch(t *testing.T) {
	release := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/latest":
			_, _ = w.Write([]byte(`{"tag_name":"v1.2.3"}`))
		case "/download/v1.2.3/subrouter_1.2.3_darwin_arm64":
			_, _ = w.Write([]byte("fake-binary"))
		case "/download/v1.2.3/SHA256SUMS":
			_, _ = w.Write([]byte(strings.Repeat("0", 64) + "  subrouter_1.2.3_darwin_arm64\n"))
		default:
			http.NotFound(w, request)
		}
	}))
	defer release.Close()
	enableGoldenTestMode(t, release.URL+"/latest", release.URL+"/download")
	_, err := acquireReleasedClient(context.Background(), goldenOptions{releasedVersion: "latest"}, t.TempDir(), true)
	if got := fixedGoldenFailure(err); got != "release_checksum_mismatch" {
		t.Fatalf("failure = %q", got)
	}
}

func enableGoldenTestMode(t *testing.T, releaseAPI, releaseDownloadRoot string) {
	t.Helper()
	previous := goldenTestHooks
	goldenTestHooks.enabled = true
	goldenTestHooks.releaseAPI = releaseAPI
	goldenTestHooks.releaseDownloadRoot = releaseDownloadRoot
	t.Cleanup(func() { goldenTestHooks = previous })
}

func TestGoldenObserverValidationRejectsRetryAndTransportFallback(t *testing.T) {
	makeSession := func(transports ...string) *goldenSession {
		stats := newObserverStats()
		for index, transport := range transports {
			stats.observe(transportEvent{
				Kind: "request_started", Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
				Transport: transport, Method: http.MethodPost, Path: "/v1/responses",
				RequestID: fmt.Sprintf("request-%d", index), ConnectionID: fmt.Sprintf("connection-%d", index),
			})
			stats.observe(transportEvent{
				Kind: "response_chunk", Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
				Transport: transport, Method: http.MethodPost, Path: "/v1/responses",
				RequestID: fmt.Sprintf("request-%d", index), ConnectionID: fmt.Sprintf("connection-%d", index), Bytes: 1,
			})
		}
		return &goldenSession{transport: "websocket", observer: &runningGoldenObserver{stats: stats}}
	}
	if got := fixedGoldenFailure(validateObserverTurns([]*goldenSession{makeSession("websocket", "websocket")}, 1)); got != "response_request_count_invalid" {
		t.Fatalf("retry failure = %q", got)
	}
	if got := fixedGoldenFailure(validateObserverTurns([]*goldenSession{makeSession("http")}, 1)); got != "transport_fallback_detected" {
		t.Fatalf("fallback failure = %q", got)
	}
}
