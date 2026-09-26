package main

import (
	"testing"
	"time"

	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

func TestClaudeAccountUploadCarriesSetupTokenWithoutRefreshToken(t *testing.T) {
	credential := agentclaude.SetupTokenCredential(testSetupToken, time.Now())
	upload, ok := claudeAccountUpload("work", &credential)
	if !ok {
		t.Fatal("a setup token with a future expiry must be uploadable to hosted cmux")
	}
	oauth, _ := upload.body["claudeAiOauth"].(map[string]any)
	if oauth == nil {
		t.Fatalf("upload body = %+v", upload.body)
	}
	if oauth["accessToken"] != testSetupToken || oauth["refreshToken"] != "" {
		t.Fatalf("claudeAiOauth = %+v", oauth)
	}
	if oauth["expiresAt"] != credential.ExpiresAt {
		t.Fatalf("expiresAt = %v, want %d", oauth["expiresAt"], credential.ExpiresAt)
	}
	scopes, _ := oauth["scopes"].([]string)
	if len(scopes) != 1 || scopes[0] != agentclaude.SetupTokenScope {
		t.Fatalf("scopes = %v", oauth["scopes"])
	}

	expired := agentclaude.SetupTokenCredential(testSetupToken, time.Now().Add(-agentclaude.SetupTokenLifetime-time.Hour))
	if _, ok := claudeAccountUpload("stale", &expired); ok {
		t.Fatal("an expired setup token must not be uploaded")
	}
	if _, ok := claudeAccountUpload("bare", &agentclaude.CredentialInfo{AccessToken: "access"}); ok {
		t.Fatal("a refresh-less credential without an expiry must not be uploaded")
	}
	if _, ok := claudeAccountUpload("classic", &agentclaude.CredentialInfo{AccessToken: "access", RefreshToken: "refresh"}); !ok {
		t.Fatal("the classic OAuth pair must stay uploadable")
	}
}

func TestClaudeCredentialIdentityDistinguishesSetupTokens(t *testing.T) {
	first := agentclaude.SetupTokenCredential("sk-ant-oat01-first-"+testSetupToken[20:], time.Now())
	second := agentclaude.SetupTokenCredential("sk-ant-oat01-second-"+testSetupToken[20:], time.Now())
	if claudeCredentialIdentity(&first) == claudeCredentialIdentity(&second) {
		t.Fatal("two setup tokens must not collapse into one dedup key")
	}
	oauth := agentclaude.CredentialInfo{AccessToken: first.AccessToken, RefreshToken: "refresh"}
	if claudeCredentialIdentity(&oauth) == claudeCredentialIdentity(&first) {
		t.Fatal("a refreshable credential is identified by its refresh token, not its access token")
	}
	if claudeCredentialIdentity(nil) != "" {
		t.Fatal("nil credential must have an empty identity")
	}
}
