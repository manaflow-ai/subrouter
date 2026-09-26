package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/manaflow-ai/subrouter/session"
)

const testCutoverMarker = "SUBROUTER_EXISTING_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestCutoverCanaryRequestEvidenceRequiresExactCurrentUserInput(t *testing.T) {
	prompt := "Reply with exactly " + testCutoverMarker
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "top-level input", body: `{"model":"test","input":"` + prompt + `"}`, want: true},
		{name: "metadata", body: `{"metadata":{"note":"` + prompt + `"},"input":"different"}`},
		{name: "object key", body: `{"` + prompt + `":true,"input":"different"}`},
		{name: "prior context", body: `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` + prompt + `"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ack"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"different"}]}]}`},
		{name: "challenged prior user with assistant current", body: `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` + prompt + `"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ack"}]}]}`},
		{name: "multiple current-user leaves", body: `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` + prompt + `"},{"type":"input_text","text":"extra"}]}]}`},
		{name: "exact current user after prior context", body: `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"old"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ack"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"` + prompt + `"}]}]}`, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := replayableCutoverRequest(t, []byte(test.body), "")
			markerHash, armed := cutoverCanaryRequestEvidence(request, "codex", "selected", true, armedCutoverRegistry(prompt, "selected"), time.Now().UTC())
			if !armed {
				t.Fatal("matching challenge was not consumed")
			}
			if got := markerHash != ""; got != test.want {
				t.Fatalf("marker evidence=%t hash=%q, want %t", got, markerHash, test.want)
			}
		})
	}
}

func TestCutoverCanaryRequestEvidenceStreamsCompressedInputWithinBounds(t *testing.T) {
	t.Parallel()
	prompt := "Reply with exactly " + testCutoverMarker
	body := []byte(`{"model":"test","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` + prompt + `"}]}]}`)
	request := replayableCutoverRequest(t, zstdCutoverBytes(t, body), "zstd")
	got, armed := cutoverCanaryRequestEvidence(request, "codex", "selected", true, armedCutoverRegistry(prompt, "selected"), time.Now().UTC())
	if !armed || got != sha256Hex([]byte(testCutoverMarker)) {
		t.Fatalf("compressed evidence armed=%t hash=%q", armed, got)
	}

	oversized := []byte(`{"input":"` + strings.Repeat("x", (16<<20)+1) + prompt + `"}`)
	request = replayableCutoverRequest(t, zstdCutoverBytes(t, oversized), "zstd")
	got, armed = cutoverCanaryRequestEvidence(request, "codex", "selected", true, armedCutoverRegistry(prompt, "selected"), time.Now().UTC())
	if !armed || got != "" {
		t.Fatalf("oversized compressed evidence armed=%t hash=%q", armed, got)
	}
}

func TestCutoverCanaryOrdinaryTrafficDoesNotInspectBody(t *testing.T) {
	prompt := "Reply with exactly " + testCutoverMarker
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"ordinary"}`))
	request.Header.Set("Content-Type", "application/json")
	var getBodyCalls atomic.Int32
	request.GetBody = func() (io.ReadCloser, error) {
		getBodyCalls.Add(1)
		return io.NopCloser(strings.NewReader(`{"input":"ordinary"}`)), nil
	}
	registry := armedCutoverRegistry(prompt, "selected")
	if hash, armed := cutoverCanaryRequestEvidence(request, "codex", "ordinary", true, registry, time.Now().UTC()); hash != "" || armed {
		t.Fatalf("ordinary traffic produced evidence armed=%t hash=%q", armed, hash)
	}
	if hash, armed := cutoverCanaryRequestEvidence(request, "codex", "selected", false, registry, time.Now().UTC()); hash != "" || armed {
		t.Fatalf("inactive traffic produced evidence armed=%t hash=%q", armed, hash)
	}
	if got := getBodyCalls.Load(); got != 0 {
		t.Fatalf("ordinary traffic body inspected %d times", got)
	}
}

func TestCutoverChallengeAdminEndpointArmsExactIdleSessionOnce(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "selected", "account", ""); err != nil {
		t.Fatal(err)
	}
	active := NewActiveSessions()
	registry := newCutoverChallengeRegistry()
	server := Server{Sessions: store, ActiveSessions: active, cutoverChallenges: registry, AdminToken: "secret"}
	handler := server.Handler()
	registration := testCutoverRegistration("selected")

	unauthorized := cutoverChallengeAdminRequest(t, http.MethodPost, registration, "")
	unauthorized.RemoteAddr = "192.0.2.1:1234"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, unauthorized)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", response.Code)
	}

	request := cutoverChallengeAdminRequest(t, http.MethodPost, registration, "secret")
	request.RemoteAddr = "192.0.2.1:1234"
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("arm status=%d body=%s", response.Code, response.Body.String())
	}
	if _, ok := registry.take("codex", "other", time.Now().UTC()); ok {
		t.Fatal("challenge matched an unselected session")
	}
	if _, ok := registry.take("codex", "selected", time.Now().UTC()); !ok {
		t.Fatal("challenge did not match selected session")
	}
	if _, ok := registry.take("codex", "selected", time.Now().UTC()); ok {
		t.Fatal("challenge was not one-shot")
	}

	end := active.Begin("codex", "selected")
	defer end()
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, cutoverChallengeAdminRequest(t, http.MethodPost, registration, "secret"))
	if response.Code != http.StatusConflict {
		t.Fatalf("active-session arm status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestCutoverCanaryLogEvidenceOmitsRawAccountIdentity(t *testing.T) {
	const (
		accountIdentity = "complete-account@example.invalid"
		markerHash      = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	)
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	logger.Info("proxy request", privacySafeCutoverRequestLogAttrs("codex", "selected-session", userEmailHash(accountIdentity), http.MethodPost, "/v1/responses", "provider.invalid", "127.0.0.1", "codex-test", markerHash)...)
	line := output.String()
	for _, want := range []string{`"msg":"proxy request"`, `"agent":"codex"`, `"session":"selected-session"`, `"cutover_marker_hash":"` + markerHash + `"`, `"time":`} {
		if !strings.Contains(line, want) {
			t.Fatalf("privacy-safe marker evidence missing %s: %s", want, line)
		}
	}
	if strings.Contains(line, accountIdentity) || strings.Contains(line, `"account"`) || strings.Contains(line, "@example.invalid") {
		t.Fatalf("marker evidence leaked raw account identity: %s", line)
	}
}

func TestSessionInventoryReportsExplicitActivity(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "selected", "account", ""); err != nil {
		t.Fatal(err)
	}
	active := NewActiveSessions()
	end := active.Begin("codex", "selected")
	server := Server{Sessions: store, ActiveSessions: active}
	request := httptest.NewRequest(http.MethodGet, "/_subrouter/sessions", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var activeRows []sessionStatusForTest
	if err := json.Unmarshal(response.Body.Bytes(), &activeRows); err != nil {
		t.Fatal(err)
	}
	if len(activeRows) != 1 || activeRows[0].Active == nil || !*activeRows[0].Active {
		t.Fatalf("active inventory=%s", response.Body.String())
	}

	end()
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/_subrouter/sessions", nil))
	var idleRows []sessionStatusForTest
	if err := json.Unmarshal(response.Body.Bytes(), &idleRows); err != nil {
		t.Fatal(err)
	}
	if len(idleRows) != 1 || idleRows[0].Active == nil || *idleRows[0].Active {
		t.Fatalf("idle inventory=%s", response.Body.String())
	}
}

func armedCutoverRegistry(prompt, sessionID string) *cutoverChallengeRegistry {
	now := time.Now().UTC()
	registration := testCutoverRegistration(sessionID)
	registration.InputSHA256 = sha256Hex([]byte(prompt))
	registry := newCutoverChallengeRegistry()
	_ = registry.arm(armedCutoverChallenge{cutoverChallengeRegistration: registration, notBefore: now.Add(-time.Second), expiresAt: now.Add(time.Minute)}, now)
	return registry
}

func testCutoverRegistration(sessionID string) cutoverChallengeRegistration {
	now := time.Now().UTC()
	return cutoverChallengeRegistration{
		Schema:       cutoverChallengeSchema,
		AgentType:    "codex",
		SessionID:    sessionID,
		InputSHA256:  sha256Hex([]byte("Reply with exactly " + testCutoverMarker)),
		MarkerSHA256: sha256Hex([]byte(testCutoverMarker)),
		NotBefore:    now.Add(-time.Second).Format(time.RFC3339Nano),
		ExpiresAt:    now.Add(time.Minute).Format(time.RFC3339Nano),
	}
}

func cutoverChallengeAdminRequest(t *testing.T, method string, registration cutoverChallengeRegistration, token string) *http.Request {
	t.Helper()
	body, err := json.Marshal(registration)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, "/_subrouter/cutover-challenge", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("X-Subrouter-Admin-Token", token)
	}
	return request
}

func replayableCutoverRequest(t *testing.T, body []byte, encoding string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if encoding != "" {
		request.Header.Set("Content-Encoding", encoding)
	}
	if replayable, err := makeRequestBodyReplayable(request, replayablePostMaxBodyBytes); err != nil || !replayable {
		t.Fatalf("make request replayable=%t err=%v", replayable, err)
	}
	return request
}

func zstdCutoverBytes(t *testing.T, body []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	encoder, err := zstd.NewWriter(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encoder.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

type sessionStatusForTest struct {
	session.Assignment
	Active *bool `json:"active"`
}
