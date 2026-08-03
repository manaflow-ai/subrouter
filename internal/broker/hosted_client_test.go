package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHostedClientUsesTenantScopedDirectAccountAPI(t *testing.T) {
	key := "srt_0123456789abcdef0123456789abcdef"
	var uploaded bool
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if !strings.HasPrefix(r.URL.Path, "/t/"+key+"/_subrouter/accounts") {
			http.Error(w, "unexpected path", http.StatusBadRequest)
			return
		}
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id": "user@example.com", "provider": "codex",
				"auth_mode": "oauth", "email": "user@example.com",
				"label":  "Shared Codex",
				"health": map[string]any{"ok": false, "message": "refresh failed"},
			}})
		case http.MethodPost:
			uploaded = true
			_ = json.NewEncoder(w).Encode(map[string]any{"account": map[string]string{
				"id": "user@example.com", "kind": "codex", "label": "user@example.com",
			}})
		case http.MethodDelete:
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		}
	}))
	defer server.Close()
	client := NewClient(Config{
		Version: 1, BaseURL: DefaultBaseURL,
		AccessToken: "access", RefreshToken: "refresh",
		TeamID: "team", CredentialSource: CredentialSourceHosted,
		HostedURL: server.URL, TenantKey: key,
	})
	client.HTTPClient = server.Client()
	items, err := client.ListAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Kind != "codex" || items[0].Label != "Shared Codex" {
		t.Fatalf("items = %#v", items)
	}
	if items[0].Health == nil || items[0].Health.OK ||
		items[0].Health.Message != "refresh failed" {
		t.Fatalf("health = %#v", items[0].Health)
	}
	if _, err := client.UploadAccount(context.Background(), AccountUpload{
		"provider": "codex", "label": "user@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if !uploaded {
		t.Fatal("account was not uploaded")
	}
	if err := client.DeleteAccount(context.Background(), "user@example.com"); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if !strings.HasPrefix(path, "/t/"+key+"/_subrouter/accounts") {
			t.Fatalf("unexpected path = %s", path)
		}
	}
}

func TestHostedClientRepairPreservesTargetAccountID(t *testing.T) {
	key := "srt_0123456789abcdef0123456789abcdef"
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"account": map[string]string{
			"id": "account-a", "kind": "codex", "label": "user@example.com",
		}})
	}))
	defer server.Close()
	client := NewClient(Config{
		Version: 1, BaseURL: DefaultBaseURL,
		AccessToken: "access", RefreshToken: "refresh",
		TeamID: "team", CredentialSource: CredentialSourceHosted,
		HostedURL: server.URL, TenantKey: key,
	})
	client.HTTPClient = server.Client()
	input := AccountUpload{"provider": "codex", "label": "user@example.com"}
	if _, err := client.RepairAccount(context.Background(), "account-a", input); err != nil {
		t.Fatal(err)
	}
	if body["targetAccountID"] != "account-a" {
		t.Fatalf("repair target = %v, want account-a", body["targetAccountID"])
	}
	if _, exists := input["targetAccountID"]; exists {
		t.Fatal("RepairAccount mutated the caller's upload map")
	}
}

func TestHostedClientRejectsUnknownAPIKeyProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"id": "future", "provider": "future-provider", "auth_mode": "apikey",
		}})
	}))
	defer server.Close()
	client := NewClient(Config{
		Version: 1, BaseURL: DefaultBaseURL,
		AccessToken: "access", RefreshToken: "refresh", TeamID: "team",
		CredentialSource: CredentialSourceHosted, HostedURL: server.URL,
		TenantKey: "srt_0123456789abcdef0123456789abcdef",
	})
	client.HTTPClient = server.Client()
	if _, err := client.ListAccounts(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "unsupported hosted account provider") {
		t.Fatalf("error = %v, want unsupported provider", err)
	}
}
