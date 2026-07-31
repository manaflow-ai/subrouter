package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
			now := time.Now().UTC()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"teamId": "team-1",
				"lease": map[string]any{
					"leaseId": "lease_test", "accountId": "user@example.com",
					"provider": "codex", "authMode": "oauth",
					"token": "access-only", "label": "user@example.com",
					"email": "user@example.com", "credentialGeneration": 7,
					"issuedAt":  now.Format(time.RFC3339Nano),
					"expiresAt": now.Add(5 * time.Minute).Format(time.RFC3339Nano),
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
	request := LeaseRequest{
		Provider: account.ProviderCodex, RequiredAuthMode: account.AuthModeOAuth,
		AgentType: "codex", SessionID: "session-1",
	}
	lease, err := client.Lease(context.Background(), request)
	if err != nil {
		t.Fatal(err)
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
		Outcome: LeaseSuccess, StatusCode: http.StatusOK,
	}); err != nil {
		t.Fatal(err)
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
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
}
