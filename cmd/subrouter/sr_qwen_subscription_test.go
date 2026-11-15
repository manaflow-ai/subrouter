package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/manaflow-ai/subrouter/internal/agents/qwensubscription"
	"github.com/manaflow-ai/subrouter/internal/proxy"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/tenant"
)

func TestStandaloneQwenStatusRecoverySerializesCredentialMutations(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(map[bool]string{true: "remove", false: "relogin"}[remove], func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", filepath.Join(root, "home"))
			t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
			store := qwensubscription.Store{}
			credential := qwensubscription.CredentialInfo{AccessToken: "old-access", RefreshToken: "old-refresh", Subject: "member", MemberType: "pro", ExpiresAt: time.Now().Add(time.Hour)}
			if _, err := store.SaveManagedCredential("work", credential); err != nil {
				t.Fatal(err)
			}
			codexStore := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
			mutate := func() (bool, error) {
				if remove {
					_, changed, err := store.RemoveManagedAccount("work")
					return changed, err
				}
				credential.AccessToken, credential.RefreshToken = "replacement-access", "replacement-refresh"
				_, err := store.SaveManagedCredential("work", credential)
				return err == nil, err
			}
			var rotations atomic.Int32
			client := &http.Client{Transport: srRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				response := func(status int, body string) (*http.Response, error) {
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				}
				if r.URL.Path == "/api/oauth/token" {
					rotations.Add(1)
					if _, err := os.Stat(filepath.Join(codexStore.StoreDir(), ".account-generation")); err != nil {
						t.Error("status recovery started before publishing its mutation", err)
					}
					ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
					defer cancel()
					if err := proxy.PublishAccountDiskMutation(ctx, codexStore.StoreDir(), mutate); !errors.Is(err, context.DeadlineExceeded) {
						t.Errorf("credential mutation entered during status token exchange: %v", err)
					}
					return response(200, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`)
				}
				if r.URL.Path != "/api/oauth/userinfo" {
					t.Errorf("unexpected request path %s", r.URL.Path)
				}
				if r.Header.Get("Authorization") == "Bearer old-access" {
					return response(401, `{}`)
				}
				return response(200, `{"sub":"member","email":"member@example.test","member_type":"pro"}`)
			})}
			rows, err := (srRunner{store: codexStore, client: client}).fetchUsageRows(t.Context())
			if err != nil || rotations.Load() != 1 {
				t.Fatalf("status recovery failed: rotations=%d err=%v", rotations.Load(), err)
			}
			for _, row := range rows {
				if row.provider == accounts.ProviderQwenOAuth && row.err != nil {
					t.Fatal("membership repair failed", row.err)
				}
			}
			if err := proxy.PublishAccountDiskMutation(t.Context(), codexStore.StoreDir(), mutate); err != nil {
				t.Fatal(err)
			}
			listed, err := store.ListAccounts(t.Context())
			if err != nil || (remove && len(listed) != 0) || (!remove && (len(listed) != 1 || listed[0].Token != "replacement-access")) {
				t.Fatalf("completed mutation was undone: accounts=%d err=%v", len(listed), err)
			}
		})
	}
}

func TestStandaloneQwenStatusPublishesOnlyRecoveryIncludingPendingGrant(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{true: "verification failed after rotation", false: "fresh membership"}[rejected], func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", filepath.Join(root, "home"))
			t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
			store := qwensubscription.Store{}
			if _, err := store.SaveManagedCredential("work", qwensubscription.CredentialInfo{AccessToken: "old-access", RefreshToken: "old-refresh", Subject: "member", MemberType: "pro", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			var rotations atomic.Int32
			client := &http.Client{Transport: srRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				status, body := 200, `{"sub":"member","email":"member@example.test","member_type":"pro"}`
				if r.URL.Path == "/api/oauth/token" {
					rotations.Add(1)
					body = `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`
				} else if rejected {
					if r.Header.Get("Authorization") == "Bearer old-access" {
						status = 401
					} else {
						status = 503
					}
					body = `{}`
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			codexStore := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
			if _, err := (srRunner{store: codexStore, client: client}).fetchUsageRows(t.Context()); err != nil {
				t.Fatal(err)
			}
			_, publishedErr := os.Stat(filepath.Join(codexStore.StoreDir(), ".account-generation"))
			listed, err := store.ListAccounts(t.Context())
			if err != nil || len(listed) != 1 {
				t.Fatalf("inventory after status: accounts=%d err=%v", len(listed), err)
			}
			if rejected {
				if publishedErr != nil || rotations.Load() != 1 || listed[0].Token != "" {
					t.Fatalf("pending grant was not published and quarantined: marker=%v rotations=%d", publishedErr, rotations.Load())
				}
			} else if !os.IsNotExist(publishedErr) || rotations.Load() != 0 || listed[0].Token != "old-access" {
				t.Fatalf("fresh status mutated credentials: marker=%v rotations=%d", publishedErr, rotations.Load())
			}
		})
	}
}

func TestQwenSubscriptionCommandsDispatchInTeamAndHostedModes(t *testing.T) {
	key, err := tenant.DeriveKey([]byte("0123456789abcdef0123456789abcdef"), "project", "team")
	if err != nil {
		t.Fatal(err)
	}
	var removals atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/t/"+key)
		if path == r.URL.Path {
			t.Error("subscription command omitted tenant authorization")
		}
		switch path {
		case "/_subrouter/accounts":
			_, _ = io.WriteString(w, `[{"id":"qwen-subscription:work","label":"work","provider":"qwen-oauth","auth_mode":"oauth"}]`)
		case serverAccountImportPath:
			if r.Method == http.MethodGet {
				// Reject login before it can reach a live OAuth endpoint.
				_, _ = io.WriteString(w, `{"ok":true,"providers":["qwen"]}`)
				return
			}
			t.Error("unsupported subscription import reached mutation")
			removals.Add(1)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	for _, mode := range []broker.CredentialSource{broker.CredentialSourceTeam, broker.CredentialSourceHosted} {
		t.Run(string(mode), func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "cloud.json")
			t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)
			t.Setenv("SUBROUTER_SERVER", "")
			t.Setenv("SUBROUTER_CODEX_SERVER", "")
			if err := broker.SaveConfig(configPath, broker.Config{Version: 1, AccessToken: "access", RefreshToken: "refresh", TeamID: "team", CredentialSource: mode, HostedURL: server.URL, TenantKey: key}); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			runner := srRunner{program: "sr", store: accounts.CodexStore{Dir: t.TempDir()}, client: server.Client(), out: &out, errOut: &out}
			if err := runner.run(t.Context(), []string{"qwen", "subscription", "list"}); err != nil || !strings.Contains(out.String(), "qwen-subscription:work") {
				t.Fatalf("subscription list was not dispatched: %v: %s", err, out.String())
			}
			for _, command := range []string{"login", "remove"} {
				if err := runner.run(t.Context(), []string{"qwen", "subscription", command, "work"}); err == nil || !strings.Contains(err.Error(), "does not advertise qwen-oauth") {
					t.Fatalf("subscription %s bypassed tenant preflight: %v", command, err)
				}
			}
		})
	}
	if removals.Load() != 0 {
		t.Fatal("rejected preflight mutated the hosted account pool")
	}
}

func TestQwenSubscriptionAuthErrorSuggestsSubscriptionLogin(t *testing.T) {
	row := srUsageRow{provider: accounts.ProviderQwenOAuth, email: "qwen-subscription:work account", err: errors.New("userinfo HTTP 401")}
	if got := usageRowErrorHint(row); got != " (re-add with: sr qwen subscription login 'work account')" {
		t.Fatalf("wrong Qwen recovery command: %s", got)
	}
}

func TestQwenSubscriptionCommandsUseSelectedServerAndProtectedImport(t *testing.T) {
	var removals atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_subrouter/accounts":
			if r.Header.Get("Authorization") != "Bearer admin-secret" {
				t.Error("inventory did not use admin authorization")
			}
			_, _ = io.WriteString(w, `[{"id":"qwen-subscription:work","label":"work","provider":"qwen-oauth","auth_mode":"oauth"},{"id":"qwen:plan","label":"plan","provider":"qwen","auth_mode":"apikey"}]`)
		case "/_subrouter/account-import":
			if r.Header.Get("Authorization") != "Bearer import-secret" {
				t.Error("enrollment did not use scoped import authorization")
			}
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, `{"ok":true,"providers":["qwen-oauth"]}`)
				return
			}
			var payload serverAccountImportRequest
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.QwenSubscription == nil || !payload.QwenSubscription.Remove || payload.QwenSubscription.Label != "work" || payload.Provider != "qwen-oauth" {
				t.Error("incorrect subscription removal payload")
			}
			if payload.QwenSubscription.Credential.AccessToken != "" || payload.QwenSubscription.Credential.RefreshToken != "" {
				t.Error("removal carried a credential")
			}
			removals.Add(1)
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	var out bytes.Buffer
	r := srRunner{out: &out, errOut: &out, client: server.Client()}
	target := srServerConfig{Name: "team", URL: server.URL, AdminToken: "admin-secret", AccountImportToken: "import-secret"}
	if !isQwenManagementCommand([]string{"subscription", "login", "work"}) {
		t.Fatal("subscription command would launch the Token Plan client")
	}
	if err := r.qwenRemote(t.Context(), target, []string{"subscription", "list"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "qwen-subscription:work") || strings.Contains(out.String(), "qwen:plan") {
		t.Fatal("subscription inventory mixed plan-key accounts")
	}
	if err := r.qwenRemote(t.Context(), target, []string{"subscription", "remove", "work"}); err != nil {
		t.Fatal(err)
	}
	if removals.Load() != 1 {
		t.Fatal("removal was not sent to the selected server")
	}
}

func TestQwenSubscriptionLoginPreflightsBeforeStartingOAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"providers":["qwen","qwen-token"]}`)
	}))
	defer server.Close()
	var out bytes.Buffer
	r := srRunner{client: server.Client(), out: &out, errOut: &out}
	err := r.qwenRemote(t.Context(), srServerConfig{Name: "old", URL: server.URL, AccountImportToken: "import-secret"}, []string{"subscription", "login", "work"})
	if err == nil || !strings.Contains(err.Error(), "qwen-oauth") {
		t.Fatalf("unsupported server was not rejected before browser login: %v", err)
	}
	if strings.Contains(out.String(), "authorize") {
		t.Fatal("OAuth started before the remote capability check")
	}
}
