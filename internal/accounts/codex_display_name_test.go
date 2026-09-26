package accounts

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func planJWT(email, plan, workspace string) string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	auth := map[string]any{"chatgpt_user_id": "user-1", "chatgpt_account_id": workspace}
	if plan != "" {
		auth["chatgpt_plan_type"] = plan
	}
	payload, _ := json.Marshal(map[string]any{
		"exp":                         time.Now().Add(time.Hour).Unix(),
		"email":                       email,
		"https://api.openai.com/auth": auth,
	})
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func ownerAccount(email, plan, workspace string) StoredCodexAccount {
	token := planJWT(email, plan, workspace)
	auth := CodexAuthFile{Tokens: &CodexTokens{AccessToken: token, IDToken: token, RefreshToken: "r", AccountID: workspace}}
	key, err := CodexOAuthIdentifier(auth)
	if err != nil {
		panic(err)
	}
	return StoredCodexAccount{Email: key, Auth: auth}
}

// Two records for one login email read as the login email, never as the opaque
// owner hash; the plan is reported separately and never folded into the name.
func TestDisplayNameSeparatesWorkspaceAndPersonalPlanUnderOneEmail(t *testing.T) {
	team := ownerAccount("lawrence@example.com", "team", "ef354321-0000-4000-8000-000000000001")
	personal := ownerAccount("lawrence@example.com", "pro", "76a0ff53-0000-4000-8000-000000000002")
	if team.Email == personal.Email {
		t.Fatal("two workspaces must not share a stored key")
	}
	if got := team.DisplayName(); got != "lawrence@example.com" {
		t.Fatalf("team display = %q", got)
	}
	if got := personal.DisplayName(); got != "lawrence@example.com" {
		t.Fatalf("personal display = %q", got)
	}
	if got, want := team.PlanType(), "team"; got != want {
		t.Fatalf("team plan = %q, want %q", got, want)
	}
	if got, want := personal.PlanType(), "pro"; got != want {
		t.Fatalf("personal plan = %q, want %q", got, want)
	}
	if account, ok := team.Account("test"); !ok || account.Label != "lawrence@example.com" {
		t.Fatalf("account label = %q ok=%v", account.Label, ok)
	}
}

func TestDisplayNameFallsBackToWorkspacePrefixWithoutPlan(t *testing.T) {
	account := ownerAccount("lawrence@example.com", "", "ef354321-0000-4000-8000-000000000001")
	if got := account.DisplayName(); got != "lawrence@example.com [workspace ef354321]" {
		t.Fatalf("display = %q", got)
	}
}

func TestDisplayNamePrefersLabelAndKeepsPlanOutOfLegacyKeys(t *testing.T) {
	labeled := ownerAccount("lawrence@example.com", "team", "ef354321-0000-4000-8000-000000000001")
	labeled.Label = "work laptop"
	if got := labeled.DisplayName(); got != "work laptop" {
		t.Fatalf("labeled display = %q", got)
	}
	legacy := StoredCodexAccount{Email: "lawrence@example.com", Auth: CodexAuthFile{Tokens: &CodexTokens{IDToken: planJWT("lawrence@example.com", "pro", "76a0ff53-0000-4000-8000-000000000002")}}}
	if got := legacy.DisplayName(); got != "lawrence@example.com" {
		t.Fatalf("legacy display = %q", got)
	}
	if got := legacy.PlanType(); got != "pro" {
		t.Fatalf("legacy plan = %q", got)
	}
	noPlan := StoredCodexAccount{Email: "lawrence@example.com", Auth: CodexAuthFile{Tokens: &CodexTokens{IDToken: planJWT("lawrence@example.com", "", "76a0ff53-0000-4000-8000-000000000002")}}}
	if got := noPlan.DisplayName(); got != "lawrence@example.com" {
		t.Fatalf("legacy display without plan = %q", got)
	}
	apiKey := StoredCodexAccount{Email: "apikey:ops", Auth: CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-test"}}
	if got := apiKey.DisplayName(); got != "apikey:ops" {
		t.Fatalf("api key display = %q", got)
	}
}
