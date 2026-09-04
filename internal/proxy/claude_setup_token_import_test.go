package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

const testSetupToken = "sk-ant-oat01-TESTTOKENTESTTOKENTESTTOKENTESTTOKENTESTTOKENTESTTOKENTESTTOKENTESTTOKENTESTTOKENTESTTOK-FAKEFAKEAA"

func TestValidateClaudeAccountImportAcceptsSetupTokenWithFutureExpiry(t *testing.T) {
	name, credential, err := validateClaudeAccountImport(claudeAccountImport{
		Name:       "work",
		Credential: agentclaude.SetupTokenCredential(testSetupToken, time.Now()),
	})
	if err != nil {
		t.Fatalf("setup token import rejected: %v", err)
	}
	if name != "work" || credential.AccessToken != testSetupToken || credential.RefreshToken != "" {
		t.Fatalf("validated import = (%q, %+v)", name, credential)
	}
	if len(credential.Scopes) != 1 || credential.Scopes[0] != agentclaude.SetupTokenScope {
		t.Fatalf("scopes were not carried through: %v", credential.Scopes)
	}

	// The classic refreshable pair keeps working exactly as before.
	if _, _, err := validateClaudeAccountImport(claudeAccountImport{
		Name:       "classic",
		Credential: agentclaude.CredentialInfo{AccessToken: "access", RefreshToken: "refresh"},
	}); err != nil {
		t.Fatalf("OAuth pair import rejected: %v", err)
	}

	for _, tc := range []struct {
		label      string
		credential agentclaude.CredentialInfo
	}{
		{"no refresh token and no expiry", agentclaude.CredentialInfo{AccessToken: "access"}},
		{"expired setup token", agentclaude.SetupTokenCredential(testSetupToken, time.Now().Add(-agentclaude.SetupTokenLifetime-time.Hour))},
		{"no access token", agentclaude.CredentialInfo{RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}},
	} {
		_, _, err := validateClaudeAccountImport(claudeAccountImport{Name: "bad", Credential: tc.credential})
		if err == nil || !strings.Contains(err.Error(), "incomplete") {
			t.Errorf("%s: err = %v, want incomplete payload rejection", tc.label, err)
		}
	}
}

func TestSetupTokenExpiryErrorIsTerminalCredentialError(t *testing.T) {
	// The store phrases an expired setup token with "no usable credential" so
	// the account leaves routing until it is re-added instead of retrying.
	err := agentclaude.CredentialInfo{}.Validate()
	if err == nil {
		t.Fatal("empty credential must not validate")
	}
	expired := "Claude profile \"work\" has no usable credential: setup token expired 2027-09-02; re-add with 'sr claude add work'"
	if !isTerminalCredentialError(errorString(expired)) {
		t.Fatalf("expired setup token error was not classified terminal: %s", expired)
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }

func TestTenantAccountUploadAcceptsSetupTokenWithoutRefreshToken(t *testing.T) {
	registry, handler, _ := newMultiTenantFixture(t)
	_, key, err := registry.Create("team")
	if err != nil {
		t.Fatal(err)
	}
	path := "/t/" + key + "/_subrouter/accounts"
	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return response
	}
	future := time.Now().Add(agentclaude.SetupTokenLifetime).UnixMilli()
	accepted := post(fmt.Sprintf(`{"provider":"claude","label":"work","claudeAiOauth":{"accessToken":%q,"expiresAt":%d,"scopes":["user:inference"]}}`, testSetupToken, future))
	if accepted.Code != http.StatusOK {
		t.Fatalf("setup token upload status = %d, body = %s", accepted.Code, accepted.Body.String())
	}
	if strings.Contains(accepted.Body.String(), testSetupToken) {
		t.Fatal("upload response echoed the token")
	}
	past := time.Now().Add(-time.Hour).UnixMilli()
	for name, body := range map[string]string{
		"expired setup token":  fmt.Sprintf(`{"provider":"claude","label":"stale","claudeAiOauth":{"accessToken":%q,"expiresAt":%d}}`, testSetupToken, past),
		"no refresh no expiry": fmt.Sprintf(`{"provider":"claude","label":"bare","claudeAiOauth":{"accessToken":%q}}`, testSetupToken),
	} {
		response := post(body)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, body = %s", name, response.Code, response.Body.String())
		}
	}
}
