package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

func TestTeamClientUsesTenantScopedCredentialLeaseAPI(t *testing.T) {
	t.Parallel()

	const key = "srt_0123456789abcdef0123456789abcdef"
	var paths []string
	var leaseRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("tenant request sent cmux session authorization")
		}
		switch {
		case r.Method == http.MethodPost &&
			r.URL.Path == "/t/"+key+"/_subrouter/leases":
			leaseRequests++
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			if request["sessionId"] != "session-1" ||
				request["requiredAuthMode"] != "oauth" {
				t.Errorf("lease request = %#v", request)
			}
			if leaseRequests == 1 && request["sessionToken"] != nil {
				t.Errorf("first lease request sent a session token: %#v", request)
			}
			if leaseRequests == 2 && request["sessionToken"] != "session_server_issued" {
				t.Errorf("retry lease request did not reuse server session token: %#v", request)
			}
			now := time.Now().UTC()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"teamId": "team-1",
				"lease": map[string]any{
					"leaseId": "lease_test", "accountId": "user@example.com",
					"provider": "codex", "authMode": "oauth",
					"token": "access-only", "label": "user@example.com",
					"email": "user@example.com", "credentialGeneration": 7,
					"sessionToken": "session_server_issued",
					"issuedAt":     now.Format(time.RFC3339Nano),
					"expiresAt":    now.Add(5 * time.Minute).Format(time.RFC3339Nano),
				},
			})
		case r.Method == http.MethodPost &&
			r.URL.Path == "/t/"+key+"/_subrouter/leases/lease_test/events":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient(Config{
		Version: 1, BaseURL: DefaultBaseURL,
		AccessToken: "cmux-access", RefreshToken: "cmux-refresh",
		TeamID: "team-1", CredentialSource: CredentialSourceTeam,
		HostedURL: server.URL, TenantKey: key,
	})
	client.HTTPClient = server.Client()
	client.sessionTokens["expired"] = leaseSessionToken{
		value: "session_expired", expiresAt: time.Now().Add(-time.Second),
	}
	request := LeaseRequest{
		Provider: account.ProviderCodex, RequiredAuthMode: account.AuthModeOAuth,
		AgentType: "codex", SessionID: "session-1",
	}
	lease, err := client.Lease(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := client.sessionTokens["expired"]; ok {
		t.Fatal("expired session capability was not pruned")
	}
	if lease.Account.Token != "access-only" ||
		lease.Account.ID != "user@example.com" ||
		lease.CredentialGeneration != 7 {
		t.Fatalf("lease = %#v", lease)
	}
	if _, err := client.Lease(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if leaseRequests != 1 {
		t.Fatalf("lease requests = %d, want cached single request", leaseRequests)
	}
	if err := client.Report(context.Background(), lease.ID, LeaseReport{
		Outcome: LeaseUnauthorized, StatusCode: http.StatusUnauthorized,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Lease(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if leaseRequests != 2 {
		t.Fatalf("lease requests after terminal report = %d, want 2", leaseRequests)
	}
	for _, path := range paths {
		if !strings.HasPrefix(path, "/t/"+key+"/_subrouter/leases") {
			t.Fatalf("request escaped tenant lease API: %s", path)
		}
	}
}

func TestTeamModeRequiresHostedTenantConfiguration(t *testing.T) {
	t.Parallel()

	config := Config{
		BaseURL: DefaultBaseURL, AccessToken: "access", RefreshToken: "refresh",
		TeamID: "team-1", CredentialSource: CredentialSourceTeam,
	}
	if config.TeamModeReady() {
		t.Fatal("team mode was ready without a hosted URL and tenant key")
	}
	config.HostedURL = "https://sr.cmux.dev"
	config.TenantKey = "srt_0123456789abcdef0123456789abcdef"
	if !config.TeamModeReady() {
		t.Fatal("team mode was not ready with hosted tenant configuration")
	}
	if !config.HostedTenantReady() {
		t.Fatal("hosted tenant endpoint was not recognized")
	}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseSessionKeySurvivesAuthModeVariation(t *testing.T) {
	base := LeaseRequest{
		Provider: account.ProviderCodex, RequiredAuthMode: account.AuthModeOAuth,
		AgentType: "codex", SessionID: "session-1", UserEmail: "Alice@Example.COM ",
	}
	apiKey := base
	apiKey.RequiredAuthMode = account.AuthModeAPIKey
	apiKey.UserEmail = " alice@example.com"
	if leaseSessionKey(base) != leaseSessionKey(apiKey) {
		t.Fatal("auth mode or email presentation variation split one server-issued session capability")
	}
	otherUser := base
	otherUser.UserEmail = "bob@example.com"
	if leaseSessionKey(base) == leaseSessionKey(otherUser) {
		t.Fatal("distinct users shared one server-issued session capability")
	}
	anonymous := base
	anonymous.UserEmail = ""
	if leaseSessionKey(anonymous) == leaseSessionKey(base) {
		t.Fatal("anonymous and identified callers shared one server-issued session capability")
	}
}

func TestConcurrentFirstLeasesShareServerSessionCapability(t *testing.T) {
	t.Parallel()
	const key = "srt_0123456789abcdef0123456789abcdef"
	var requests atomic.Int32
	var mu sync.Mutex
	var requestTokens []string
	firstArrived := make(chan struct{})
	releaseFirst := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		number := requests.Add(1)
		mu.Lock()
		requestTokens = append(requestTokens, fmt.Sprint(request["sessionToken"]))
		mu.Unlock()
		if number == 1 {
			close(firstArrived)
			<-releaseFirst
		}
		now := time.Now().UTC()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"teamId": "team-1",
			"lease": map[string]any{
				"leaseId": fmt.Sprintf("lease-%d", number), "accountId": "user@example.com",
				"provider": "codex", "authMode": "oauth", "token": "access-only",
				"label": "user@example.com", "credentialGeneration": 1,
				"sessionToken": "session_server_issued",
				"issuedAt":     now.Format(time.RFC3339Nano),
				"expiresAt":    now.Add(5 * time.Minute).Format(time.RFC3339Nano),
			},
		})
	}))
	defer server.Close()
	client := NewClient(Config{
		Version: 1, BaseURL: DefaultBaseURL,
		AccessToken: "cmux-access", RefreshToken: "cmux-refresh",
		TeamID: "team-1", CredentialSource: CredentialSourceTeam,
		HostedURL: server.URL, TenantKey: key,
	})
	client.HTTPClient = server.Client()
	base := LeaseRequest{
		Provider: account.ProviderCodex, RequiredAuthMode: account.AuthModeOAuth,
		AgentType: "codex", SessionID: "session-1", UserEmail: "alice@example.com",
	}
	first := base
	first.Model = "gpt-5"
	second := base
	second.Model = "gpt-5-codex"
	results := make(chan error, 2)
	go func() { _, err := client.Lease(context.Background(), first); results <- err }()
	select {
	case <-firstArrived:
	case err := <-results:
		t.Fatalf("first lease failed before request: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("first lease did not reach server")
	}
	go func() { _, err := client.Lease(context.Background(), second); results <- err }()
	time.Sleep(50 * time.Millisecond)
	close(releaseFirst)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	tokens := append([]string(nil), requestTokens...)
	mu.Unlock()
	if len(tokens) != 2 || tokens[0] != "<nil>" || tokens[1] != "session_server_issued" {
		t.Fatalf("request session tokens = %#v", tokens)
	}
}

func TestConcurrentUsersWithSameSessionDoNotShareServerSessionCapability(t *testing.T) {
	t.Parallel()
	const key = "srt_0123456789abcdef0123456789abcdef"
	var requests atomic.Int32
	var mu sync.Mutex
	requestTokens := make(map[string]string)
	bothArrived := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		userEmail := fmt.Sprint(request["userEmail"])
		mu.Lock()
		requestTokens[userEmail] = fmt.Sprint(request["sessionToken"])
		mu.Unlock()
		if requests.Add(1) == 2 {
			close(bothArrived)
		}
		<-release
		now := time.Now().UTC()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"teamId": "team-1",
			"lease": map[string]any{
				"leaseId": "lease-" + userEmail, "accountId": userEmail,
				"provider": "codex", "authMode": "oauth", "token": "access-only",
				"label": userEmail, "email": userEmail, "credentialGeneration": 1,
				"sessionToken": "session-" + userEmail,
				"issuedAt":     now.Format(time.RFC3339Nano),
				"expiresAt":    now.Add(5 * time.Minute).Format(time.RFC3339Nano),
			},
		})
	}))
	defer server.Close()
	client := NewClient(Config{
		Version: 1, BaseURL: DefaultBaseURL,
		AccessToken: "cmux-access", RefreshToken: "cmux-refresh",
		TeamID: "team-1", CredentialSource: CredentialSourceTeam,
		HostedURL: server.URL, TenantKey: key,
	})
	client.HTTPClient = server.Client()
	base := LeaseRequest{
		Provider: account.ProviderCodex, RequiredAuthMode: account.AuthModeOAuth,
		AgentType: "codex", SessionID: "shared-session",
	}
	results := make(chan error, 2)
	for _, email := range []string{"alice@example.com", "bob@example.com"} {
		request := base
		request.UserEmail = email
		go func() { _, err := client.Lease(context.Background(), request); results <- err }()
	}
	select {
	case <-bothArrived:
		close(release)
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("distinct users were serialized behind one session capability flight")
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	tokens := make(map[string]string, len(requestTokens))
	for email, token := range requestTokens {
		tokens[email] = token
	}
	mu.Unlock()
	if len(tokens) != 2 || tokens["alice@example.com"] != "<nil>" || tokens["bob@example.com"] != "<nil>" {
		t.Fatalf("first request tokens = %#v, want independent empty capabilities", tokens)
	}
}
