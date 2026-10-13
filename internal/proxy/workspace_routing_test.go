package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

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
