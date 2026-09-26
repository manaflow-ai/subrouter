package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestLifecycleDrainAndReadyEndpoints(t *testing.T) {
	lifecycle := NewLifecycle()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Sessions:     store,
		Lifecycle:    lifecycle,
		MaxBodyBytes: 1024,
	}.Handler()

	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/_subrouter/ready", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("ready status = %d", ready.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/_subrouter/drain", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Host = "127.0.0.1:31415"
	drain := httptest.NewRecorder()
	handler.ServeHTTP(drain, req)
	if drain.Code != http.StatusOK {
		t.Fatalf("drain status = %d, body = %s", drain.Code, drain.Body.String())
	}
	var payload struct {
		OK       bool `json:"ok"`
		Draining bool `json:"draining"`
	}
	if err := json.Unmarshal(drain.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.OK || !payload.Draining {
		t.Fatalf("drain payload = %+v", payload)
	}

	ready = httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/_subrouter/ready", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready status after drain = %d, body = %s", ready.Code, ready.Body.String())
	}
}

func TestReadyEndpointWaitsForStartupState(t *testing.T) {
	ready := false
	handler := Server{
		ReadyCheck: func() error {
			if !ready {
				return errors.New("startup score snapshot missing")
			}
			return nil
		},
	}.Handler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/_subrouter/ready", nil))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"startup_ready"`) {
		t.Fatalf("unready response = %d %s", response.Code, response.Body.String())
	}

	ready = true
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/_subrouter/ready", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("ready response = %d %s", response.Code, response.Body.String())
	}
}

func TestDrainRejectsNewProxySessionsButAllowsExistingSessions(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	activeSessions := NewActiveSessions()
	lifecycle := NewLifecycle()
	lifecycle.Drain()
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID:       "account@example.com",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "token",
		}},
		Sessions:       store,
		Scheduler:      selectacct.NewScheduler(nil),
		ActiveSessions: activeSessions,
		Lifecycle:      lifecycle,
		MaxBodyBytes:   1024,
	}.Handler()

	newReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	newReq.Header.Set("X-Subrouter-Session", "new-session")
	newResp := httptest.NewRecorder()
	handler.ServeHTTP(newResp, newReq)
	if newResp.Code != http.StatusServiceUnavailable {
		t.Fatalf("new drained request status = %d, body = %s", newResp.Code, newResp.Body.String())
	}

	if _, err := store.Put("codex", "sticky-session", "account@example.com", ""); err != nil {
		t.Fatal(err)
	}
	stickyReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	stickyReq.Header.Set("X-Subrouter-Session", "sticky-session")
	stickyResp := httptest.NewRecorder()
	handler.ServeHTTP(stickyResp, stickyReq)
	if stickyResp.Code != http.StatusNoContent {
		body, _ := io.ReadAll(stickyResp.Result().Body)
		t.Fatalf("sticky drained request status = %d, body = %s", stickyResp.Code, string(body))
	}

	endActive := activeSessions.Begin("codex", "active-session")
	defer endActive()
	activeReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	activeReq.Header.Set("X-Subrouter-Session", "active-session")
	activeResp := httptest.NewRecorder()
	handler.ServeHTTP(activeResp, activeReq)
	if activeResp.Code != http.StatusNoContent {
		body, _ := io.ReadAll(activeResp.Result().Body)
		t.Fatalf("active drained request status = %d, body = %s", activeResp.Code, string(body))
	}
}

func TestStrictQuiesceRejectsAllProxyIngressAndResumes(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-releaseRequest
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "sticky-session", "account@example.com", ""); err != nil {
		t.Fatal(err)
	}
	lifecycle := NewLifecycle()
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{ID: "account@example.com", AuthMode: accounts.AuthModeOAuth, Token: "token"}},
		Sessions: store, Scheduler: selectacct.NewScheduler(nil), Lifecycle: lifecycle, MaxBodyBytes: 1024,
	}.Handler()

	firstDone := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
		req.Header.Set("X-Subrouter-Session", "sticky-session")
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		firstDone <- resp.Code
	}()
	<-requestStarted

	quiesceReq := httptest.NewRequest(http.MethodPost, "/_subrouter/quiesce", nil)
	quiesceReq.RemoteAddr = "127.0.0.1:12345"
	quiesceReq.Host = "127.0.0.1:31415"
	quiesceResp := httptest.NewRecorder()
	handler.ServeHTTP(quiesceResp, quiesceReq)
	if quiesceResp.Code != http.StatusOK {
		t.Fatalf("quiesce status = %d", quiesceResp.Code)
	}
	var status struct {
		Quiesced               bool  `json:"quiesced"`
		AcceptingProxyRequests bool  `json:"accepting_proxy_requests"`
		ActiveProxyRequests    int64 `json:"active_proxy_requests"`
	}
	if err := json.Unmarshal(quiesceResp.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Quiesced || status.AcceptingProxyRequests || status.ActiveProxyRequests != 1 {
		t.Fatalf("quiesce status = %+v", status)
	}

	blocked := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	blocked.Header.Set("X-Subrouter-Session", "sticky-session")
	blockedResp := httptest.NewRecorder()
	handler.ServeHTTP(blockedResp, blocked)
	if blockedResp.Code != http.StatusServiceUnavailable {
		t.Fatalf("sticky request entered after quiesce: status=%d", blockedResp.Code)
	}
	close(releaseRequest)
	if code := <-firstDone; code != http.StatusNoContent {
		t.Fatalf("pre-quiesce request status = %d", code)
	}
	if lifecycle.ActiveProxyRequests() != 0 {
		t.Fatalf("active requests after release = %d", lifecycle.ActiveProxyRequests())
	}

	resumeReq := httptest.NewRequest(http.MethodPost, "/_subrouter/resume", nil)
	resumeReq.RemoteAddr = "127.0.0.1:12345"
	resumeReq.Host = "127.0.0.1:31415"
	resumeResp := httptest.NewRecorder()
	handler.ServeHTTP(resumeResp, resumeReq)
	if resumeResp.Code != http.StatusOK || lifecycle.Quiesced() || lifecycle.Draining() {
		t.Fatalf("resume failed: status=%d lifecycle=%+v", resumeResp.Code, lifecycle.Status())
	}
}

func TestAdminTokenRequiredForNonLoopbackAdminEndpoint(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Sessions:     store,
		AdminToken:   "secret",
		MaxBodyBytes: 1024,
	}.Handler()

	req := httptest.NewRequest(http.MethodGet, "/_subrouter/accounts", nil)
	req.RemoteAddr = "100.64.0.2:12345"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status without token = %d", recorder.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/_subrouter/accounts", nil)
	req.RemoteAddr = "100.64.0.2:12345"
	req.Header.Set("Authorization", "Bearer secret")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status with token = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestPromoteUsableClaudeStatusSkipsErroredActiveProfile(t *testing.T) {
	statuses := []AccountUsageStatus{
		{
			AccountStatus: AccountStatus{
				ID:        "austin@manaflow.ai",
				Provider:  accounts.ProviderClaude,
				AuthValid: false,
				Error:     "invalid_grant",
			},
			Active: true,
		},
		{
			AccountStatus: AccountStatus{
				ID:        "lawrence@manaflow.ai",
				Provider:  accounts.ProviderClaude,
				AuthValid: true,
			},
			Windows: []accounts.UsageWindow{
				{Name: "5h", UsedPercent: 30},
				{Name: "7d", UsedPercent: 20},
			},
		},
		{
			AccountStatus: AccountStatus{
				ID:        "lc+0@cmux.com",
				Provider:  accounts.ProviderClaude,
				AuthValid: true,
			},
			Windows: []accounts.UsageWindow{
				{Name: "5h", UsedPercent: 100},
				{Name: "7d", UsedPercent: 31},
			},
		},
	}

	promoteUsableClaudeStatus(statuses)

	if statuses[0].Active {
		t.Fatal("errored Claude profile stayed active")
	}
	if !statuses[1].Active {
		t.Fatal("usable Claude profile was not promoted to active")
	}
	if statuses[2].Active {
		t.Fatal("cooked Claude profile was promoted to active")
	}
}
