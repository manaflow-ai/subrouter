package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

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
