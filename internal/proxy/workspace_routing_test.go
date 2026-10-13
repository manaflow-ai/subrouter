package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestAccountForAuthorizationPreservesWorkspaceIdentity(t *testing.T) {
	managed := []accounts.Account{
		{ID: "other", AuthMode: accounts.AuthModeOAuth, Token: "other-token"},
		{ID: "selected", AuthMode: accounts.AuthModeOAuth, Token: "selected-token"},
	}
	got, ok := accountForAuthorization(managed, "Bearer selected-token")
	if !ok || got.ID != "selected" {
		t.Fatalf("accountForAuthorization = (%+v, %v), want selected account", got, ok)
	}
	if _, ok := accountForAuthorization(managed, "Bearer unknown-token"); ok {
		t.Fatal("unknown authorization matched a managed account")
	}
	jwt := "eyJhbGciOiJub25lIn0.eyJjaGF0Z3B0X2FjY291bnRfaWQiOiJzZWxlY3RlZC1hY2N0In0."
	managed[1].AccountID = "selected-acct"
	got, ok = accountForAuthorization(managed, "Bearer "+jwt)
	if !ok || got.ID != "selected" {
		t.Fatalf("JWT workspace selection = (%+v, %v), want selected account", got, ok)
	}
}

func TestRewriteWorkspaceRoutingResponseMakesSentinelOriginExplicit(t *testing.T) {
	upstream, err := url.Parse("https://chatgpt.com/backend-api")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	response.Header().Set("Content-Type", "application/json")
	response.Body.WriteString(`{"accounts":[{"id":"acct-1","workspace_backend_origin":"NO_CONSTRAINT","account_routing_override":"NO_CONSTRAINT"}],"default_account_id":"acct-1"}`)
	result := response.Result()
	result.Body = io.NopCloser(strings.NewReader(response.Body.String()))
	if err := rewriteWorkspaceRoutingResponse(result, "/backend-api/wham/accounts/check", upstream); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.NewDecoder(result.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if got := payload.Accounts[0]["workspace_backend_origin"]; got != "https://chatgpt.com" {
		t.Fatalf("workspace_backend_origin = %v, want https://chatgpt.com", got)
	}
	if got := payload.Accounts[0]["account_routing_override"]; got != "NO_CONSTRAINT" {
		t.Fatalf("account_routing_override = %v, want NO_CONSTRAINT", got)
	}
}

func TestRewriteWorkspaceRoutingResponseLeavesExplicitOriginUnchanged(t *testing.T) {
	upstream, err := url.Parse("https://chatgpt.com/backend-api")
	if err != nil {
		t.Fatal(err)
	}
	result := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"accounts":[{"workspace_backend_origin":"https://workspace.chatgpt.com"}]}`)),
		Header:     make(http.Header),
	}
	if err := rewriteWorkspaceRoutingResponse(result, "/backend-api/wham/accounts/check", upstream); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.NewDecoder(result.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if got := payload.Accounts[0]["workspace_backend_origin"]; got != "https://workspace.chatgpt.com" {
		t.Fatalf("workspace_backend_origin = %v, want explicit origin preserved", got)
	}
}
