package accounts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func workspaceAuth(email, workspace, generation string) CodexAuthFile {
	claims, _ := json.Marshal(map[string]any{
		"email": email, "exp": time.Now().Add(time.Hour).Unix(), "jti": generation,
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": workspace, "chatgpt_user_id": "fixture-user"},
	})
	token := "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	return CodexAuthFile{AuthMode: "chatgpt", Tokens: &CodexTokens{
		AccessToken: token, IDToken: token, RefreshToken: generation, AccountID: workspace,
	}}
}

func TestImportActiveKeepsSameEmailWorkspacesSeparate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir()}
	var imported []StoredCodexAccount
	for _, workspace := range []string{"personal-workspace", "team-workspace"} {
		if err := WriteActiveCodexAuth(workspaceAuth("shared@example.com", workspace, workspace)); err != nil {
			t.Fatal(err)
		}
		account, existed, err := store.ImportActive()
		if err != nil || existed {
			t.Fatalf("import %s: existed=%v err=%v", workspace, existed, err)
		}
		imported = append(imported, account)
	}
	all, err := store.List()
	if err != nil || len(all) != 2 {
		t.Fatalf("list: count=%d err=%v", len(all), err)
	}
	if all[0].ID == all[1].ID || all[0].AccountID == all[1].AccountID || all[0].Source == all[1].Source {
		t.Fatal("workspaces share a routing identity or credential file")
	}
	for _, account := range all {
		if account.Email != "shared@example.com" {
			t.Fatalf("display email = %q", account.Email)
		}
	}
	updated := workspaceAuth("shared@example.com", "team-workspace", "team-updated")
	if err := WriteActiveCodexAuth(updated); err != nil {
		t.Fatal(err)
	}
	team, existed, err := store.ImportActive()
	if err != nil || !existed || team.Email != imported[1].Email {
		t.Fatalf("reimport did not retain team identity: existed=%v err=%v", existed, err)
	}
	if active, err := store.DetectActiveAccount(); err != nil || active != team.Email {
		t.Fatalf("active=%q, want %q, err=%v", active, team.Email, err)
	}
	if _, removed, err := store.RemoveStored(team.Email); err != nil || !removed {
		t.Fatalf("remove team: removed=%v err=%v", removed, err)
	}
	personal, found, err := store.FindStored(imported[0].Email)
	if err != nil || !found || personal.Auth.Tokens.RefreshToken != "personal-workspace" {
		t.Fatalf("team operations changed personal account: found=%v err=%v", found, err)
	}
}

func TestActiveSyncDoesNotReplaceAnotherWorkspace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir()}
	personal := StoredCodexAccount{Email: "shared@example.com", Auth: workspaceAuth("shared@example.com", "personal", "personal")}
	if err := store.SaveStored(personal); err != nil {
		t.Fatal(err)
	}
	if err := WriteActiveCodexAuth(workspaceAuth("shared@example.com", "team", "team")); err != nil {
		t.Fatal(err)
	}
	if err := store.SyncActiveToStore(); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.FindStored(personal.Email)
	if err != nil || !found || got.Auth.Tokens.RefreshToken != "personal" {
		t.Fatalf("active team auth replaced stored personal auth: found=%v err=%v", found, err)
	}
	if err := syncActiveCodexAuthIfAccountActive(personal); err != nil {
		t.Fatal(err)
	}
	active, _, err := ReadActiveCodexAuth()
	if err != nil || active.Tokens.RefreshToken != "team" {
		t.Fatalf("personal refresh replaced active team auth: err=%v", err)
	}
}

func TestSaveStoredRejectsWorkspaceReplacement(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}
	account := StoredCodexAccount{Email: "shared@example.com", Auth: workspaceAuth("shared@example.com", "personal", "personal")}
	if err := store.SaveStored(account); err != nil {
		t.Fatal(err)
	}
	account.Auth = workspaceAuth("shared@example.com", "team", "team")
	if err := store.SaveStored(account); err == nil {
		t.Fatal("save replaced another workspace under the same identifier")
	}
}

func TestRefreshCodexAuthRejectsWorkspaceChange(t *testing.T) {
	original := workspaceAuth("shared@example.com", "personal", "personal")
	wrong := workspaceAuth("shared@example.com", "team", "team")
	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		body, _ := json.Marshal(wrong.Tokens)
		return jsonResponse(http.StatusOK, string(body)), nil
	})}
	if _, err := RefreshCodexAuth(context.Background(), client, original); err == nil {
		t.Fatal("refresh accepted tokens for another workspace")
	}
}

func TestLegacyCodexWorkspaceRetainsItsKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir()}
	legacy := StoredCodexAccount{Email: "shared@example.com", Auth: workspaceAuth("shared@example.com", "personal", "original")}
	if err := store.SaveStored(legacy); err != nil {
		t.Fatal(err)
	}
	if err := WriteActiveCodexAuth(workspaceAuth("shared@example.com", "personal", "updated")); err != nil {
		t.Fatal(err)
	}
	got, existed, err := store.ImportActive()
	if err != nil || !existed || got.Email != legacy.Email {
		t.Fatalf("legacy key changed: existed=%v key=%q err=%v", existed, got.Email, err)
	}
	if err := WriteActiveCodexAuth(workspaceAuth("shared@example.com", "team", "team")); err != nil {
		t.Fatal(err)
	}
	team, existed, err := store.ImportActive()
	if err != nil || existed || team.Email == legacy.Email {
		t.Fatalf("team adopted legacy key: existed=%v key=%q err=%v", existed, team.Email, err)
	}
	if err := store.ReplaceStoredOAuthWithIsolated(context.Background(), team.Email, workspaceAuth("shared@example.com", "personal", "wrong")); err == nil {
		t.Fatal("team repair accepted personal credentials")
	}
	if err := store.ReplaceStoredOAuthWithIsolated(context.Background(), team.Email, workspaceAuth("shared@example.com", "team", "repaired")); err != nil {
		t.Fatal(err)
	}
}

func TestCodexWorkspaceClaimsDoNotUseOrganizationMembership(t *testing.T) {
	claims, _ := json.Marshal(map[string]any{
		"email":         "shared@example.com",
		"organizations": []map[string]string{{"id": "not-selected"}},
	})
	token := "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	if got := ExtractChatGPTAccountIDFromJWT(token); got != "" {
		t.Fatalf("organization membership selected workspace %q", got)
	}
}

func TestCodexWorkspaceRejectsConflictingTokens(t *testing.T) {
	auth := workspaceAuth("shared@example.com", "personal", "personal")
	auth.Tokens.AccountID = "team"
	if _, err := CodexOAuthIdentifier(auth); err == nil {
		t.Fatal("accepted explicit workspace that conflicts with token claims")
	}
}

func TestWorkspaceFilenamesDoNotCollideWithLegacyAliases(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}
	alias := StoredCodexAccount{Email: "shared@example.com_team", Auth: workspaceAuth("other@example.com", "other", "alias")}
	if err := store.SaveStored(alias); err != nil {
		t.Fatal(err)
	}
	workspace, _, err := store.ResolveCodexOAuthAccount(workspaceAuth("shared@example.com", "team", "team"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStored(workspace); err != nil {
		t.Fatal(err)
	}
	if alias.SourcePath(store) == workspace.SourcePath(store) {
		t.Fatal("workspace filename collides with a legacy alias")
	}
	if _, removed, err := store.RemoveStored(workspace.Email); err != nil || !removed {
		t.Fatalf("remove workspace: removed=%v err=%v", removed, err)
	}
	if _, found, err := store.FindStored(alias.Email); err != nil || !found {
		t.Fatalf("removing workspace removed legacy alias: found=%v err=%v", found, err)
	}
}

func TestLegacyHashAliasKeepsItsFileAndSupportsRemoval(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}
	account := StoredCodexAccount{Email: "hosted#blue", Auth: workspaceAuth("shared@example.com", "team", "original")}
	legacyPath := filepath.Join(store.Dir, "hosted_blue.json")
	body, _ := json.Marshal(account)
	if err := os.WriteFile(legacyPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.FindStored(account.Email)
	if err != nil || !found || got.SourcePath(store) != legacyPath {
		t.Fatalf("legacy lookup: found=%v err=%v", found, err)
	}
	got.Auth = workspaceAuth("shared@example.com", "team", "updated")
	if err := store.SaveStored(got); err != nil {
		t.Fatal(err)
	}
	all, err := store.ListStored()
	if err != nil || len(all) != 1 {
		t.Fatalf("save duplicated legacy file: count=%d err=%v", len(all), err)
	}
	if _, removed, err := store.RemoveStored(account.Email); err != nil || !removed {
		t.Fatalf("remove legacy alias: removed=%v err=%v", removed, err)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy file remains: %v", err)
	}
}
