package accounts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
)

func ownerAuth(email, user, workspace, generation string) CodexAuthFile {
	auth := workspaceAuth(email, workspace, generation)
	claims, _ := DecodeJWTClaims(auth.Tokens.IDToken)
	claims["https://api.openai.com/auth"].(map[string]any)["chatgpt_user_id"] = user
	body, _ := json.Marshal(claims)
	token := "header." + base64.RawURLEncoding.EncodeToString(body) + ".signature"
	auth.Tokens.IDToken, auth.Tokens.AccessToken = token, token
	return auth
}

func TestCodexOwnerImportPreservesRecordAcrossEmailChange(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir()}
	if err := WriteActiveCodexAuth(ownerAuth("before@example.com", "user-1", "team", "first")); err != nil {
		t.Fatal(err)
	}
	first, _, err := store.ImportActive()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteActiveCodexAuth(ownerAuth("after@example.com", "user-1", "team", "second")); err != nil {
		t.Fatal(err)
	}
	second, existed, err := store.ImportActive()
	if err != nil || !existed || second.Email != first.Email {
		t.Fatalf("email change replaced identity: existed=%v err=%v", existed, err)
	}
	all, err := store.List()
	if err != nil || len(all) != 1 || all[0].Email != "after@example.com" {
		t.Fatalf("email change duplicated account: count=%d err=%v", len(all), err)
	}
}

func TestCodexOwnerKeepsDifferentUsersWithOneEmailAndWorkspaceSeparate(t *testing.T) {
	store := CodexStore{Dir: t.TempDir()}
	for _, user := range []string{"user-1", "user-2"} {
		a, exists, err := store.ResolveCodexOAuthAccount(ownerAuth("shared@example.com", user, "team", user))
		if err != nil || exists {
			t.Fatalf("different users collapsed: exists=%v err=%v", exists, err)
		}
		if err := store.SaveStored(a); err != nil {
			t.Fatal(err)
		}
	}
	all, err := store.List()
	if err != nil || len(all) != 2 || all[0].ID == all[1].ID {
		t.Fatalf("owner count=%d err=%v", len(all), err)
	}
}

func TestCodexOwnerRefreshRejectsDifferentUserInSameWorkspace(t *testing.T) {
	old := ownerAuth("same@example.com", "user-1", "team", "first")
	next := ownerAuth("same@example.com", "user-2", "team", "second")
	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		body, _ := json.Marshal(next.Tokens)
		return jsonResponse(http.StatusOK, string(body)), nil
	})}
	if _, err := RefreshCodexAuth(context.Background(), client, old); err == nil {
		t.Fatal("refresh accepted another user")
	}
}

func TestCodexOwnerRepairAllowsEmailChangeAndRejectsUserChange(t *testing.T) {
	old := ownerAuth("before@example.com", "user-1", "team", "first")
	if !CanReplaceCodexOAuthIdentity(old, ownerAuth("after@example.com", "user-1", "team", "second")) {
		t.Fatal("repair rejected same owner after email change")
	}
	if CanReplaceCodexOAuthIdentity(old, ownerAuth("before@example.com", "user-2", "team", "third")) {
		t.Fatal("repair accepted different user")
	}
}
