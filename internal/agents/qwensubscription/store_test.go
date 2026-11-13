package qwensubscription

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testCredential() CredentialInfo {
	return CredentialInfo{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(time.Hour), Subject: "qwen-user", Email: "verified@example.test", MemberType: "plus"}
}

func TestQwenSubscriptionDeviceLoginUsesPaidClientAndVerifiedIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "subrouter" {
			t.Error("Qwen requests need an explicit client identity")
		}
		switch r.URL.Path {
		case "/device":
			_ = r.ParseForm()
			if r.Form.Get("client_id") != "qwen-code-cli" || r.Form.Get("scope") != "model:invoke profile:read" {
				t.Error("wrong paid subscription client")
			}
			_, _ = io.WriteString(w, `{"device_code":"private-device","user_code":"CODE","verification_uri":"https://chat.qwen.ai/device","verification_uri_complete":"https://chat.qwen.ai/device?user_code=CODE","interval":1,"expires_in":30}`)
		case "/token":
			_ = r.ParseForm()
			if r.Form.Get("device_code") != "private-device" || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
				t.Error("incorrect device exchange")
			}
			_, _ = io.WriteString(w, `{"access_token":"secret-access","refresh_token":"secret-refresh","expires_in":3600,"token_type":"Bearer","scope":"model:invoke profile:read"}`)
		case "/userinfo":
			if r.Header.Get("Authorization") != "Bearer secret-access" {
				t.Error("userinfo did not use the issued access token")
			}
			_, _ = io.WriteString(w, `{"sub":"verified-sub","email":"owner@example.test","member_type":"pro"}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	oldConfig, oldInfo := oauthConfig, userInfoURL
	t.Cleanup(func() { oauthConfig, userInfoURL = oldConfig, oldInfo })
	oauthConfig.DeviceAuthURL, oauthConfig.TokenURL, userInfoURL = server.URL+"/device", server.URL+"/token", server.URL+"/userinfo"
	var out bytes.Buffer
	store := Store{Dir: t.TempDir()}
	c, err := store.AuthorizeManaged(t.Context(), server.Client(), "work", &out)
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "verified-sub" || c.Email != "owner@example.test" || c.MemberType != "pro" {
		t.Fatal("identity was not verified")
	}
	if !strings.Contains(out.String(), "https://chat.qwen.ai/authorize?") || !strings.Contains(out.String(), "user_code=CODE") || !strings.Contains(out.String(), "client=cli") || strings.Contains(out.String(), "/device?") {
		t.Fatal("login did not open Qwen's subscription authorization page")
	}
	for _, secret := range []string{"private-device", "secret-access", "secret-refresh"} {
		if strings.Contains(out.String(), secret) {
			t.Fatal("login printed a secret")
		}
	}
	if listed, _ := store.ListAccounts(t.Context()); len(listed) != 0 {
		t.Fatal("authorization persisted before publication")
	}
	acct, err := store.SaveManagedCredential("work", c)
	if err != nil {
		t.Fatal(err)
	}
	if acct.ID != "qwen-subscription:work" || acct.CredentialVersion == "" {
		t.Fatal("missing managed account identity")
	}
	path, _ := store.path("work")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("credential file is not private")
	}
}

func TestQwenSubscriptionConcurrentRefreshPersistsOneRotation(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	c := testCredential()
	acct, err := store.SaveManagedCredential("work", c)
	if err != nil {
		t.Fatal(err)
	}
	c.ExpiresAt = time.Now().Add(-time.Minute)
	if _, err := store.write("work", c); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/userinfo" {
			if r.Header.Get("Authorization") != "Bearer rotated-access" {
				t.Error("userinfo did not use rotated access token")
			}
			_, _ = io.WriteString(w, `{"sub":"qwen-user","email":"verified@example.test","member_type":"plus"}`)
			return
		}
		calls.Add(1)
		_ = r.ParseForm()
		if r.Form.Get("refresh_token") != "old-refresh" || r.Form.Get("client_id") != "qwen-code-cli" {
			t.Error("wrong refresh grant")
		}
		_, _ = io.WriteString(w, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`)
	}))
	defer server.Close()
	old, oldInfo := oauthConfig, userInfoURL
	t.Cleanup(func() { oauthConfig, userInfoURL = old, oldInfo })
	oauthConfig.TokenURL, userInfoURL = server.URL, server.URL+"/userinfo"
	var transactions atomic.Int32
	store.RefreshTransaction = func(ctx context.Context, f func() error) error { transactions.Add(1); return f() }
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			got, err := store.RefreshAccount(t.Context(), server.Client(), acct)
			if err != nil || got.Token != "rotated-access" {
				t.Error("refresh did not return the rotated credential", err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 || transactions.Load() != 8 {
		t.Fatal("refresh was not serialized")
	}
	saved, ok, err := store.read("work")
	if err != nil || !ok || saved.RefreshToken != "rotated-refresh" {
		t.Fatal("rotated grant was not durable")
	}
	if saved.Subject != c.Subject || saved.Email != c.Email {
		t.Fatal("refresh changed identity")
	}
}

func TestQwenSubscriptionRemovalCannotResurrectStaleAccount(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	acct, err := store.SaveManagedCredential("work", testCredential())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.RemoveManagedAccount("work"); err != nil || !ok {
		t.Fatal("remove failed", err)
	}
	if _, err := store.RefreshAccount(t.Context(), http.DefaultClient, acct); err == nil {
		t.Fatal("removed account refreshed")
	}
	ids, err := store.AccountInventoryIDs(t.Context())
	if err != nil || len(ids) != 0 {
		t.Fatal("removed profile reappeared", err)
	}
}

func TestQwenSubscriptionRejectsDuplicateGrantAndUnsafeLabels(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	c := testCredential()
	if _, err := store.SaveManagedCredential("one", c); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveManagedCredential("two", c); err == nil {
		t.Fatal("same rotating grant accepted twice")
	}
	for _, label := range []string{"", "bad\nlabel", "qwen-subscription:reserved", strings.Repeat("a", 161)} {
		if _, err := ManagedAccountID(label); err == nil {
			t.Fatal("unsafe label accepted")
		}
	}
	// Malformed credentials remain visible to inventory and can be removed.
	path, _ := store.path("one")
	if err := os.WriteFile(path, []byte(`{"access_token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListAccounts(t.Context()); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("corruption was hidden or leaked a token")
	}
	if ids, err := store.AccountInventoryIDs(t.Context()); err != nil || len(ids) != 1 {
		t.Fatal("corrupt account disappeared from inventory")
	}
	if _, ok, err := store.RemoveManagedAccount("one"); err != nil || !ok {
		t.Fatal("could not remove corrupt credential")
	}
}

func TestQwenSubscriptionUserinfoRejectsRedirectsAndIdentityChange(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { destinationCalls.Add(1) }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"sub": "different-user", "email": "other@example.test", "member_type": "pro"})
	}))
	defer server.Close()
	old := userInfoURL
	t.Cleanup(func() { userInfoURL = old })
	userInfoURL = server.URL + "/redirect"
	if _, err := AttestCredential(t.Context(), server.Client(), testCredential()); err == nil {
		t.Fatal("redirect accepted")
	}
	if destinationCalls.Load() != 0 {
		t.Fatal("userinfo token followed redirect")
	}
	userInfoURL = server.URL
	if _, err := AttestCredential(t.Context(), server.Client(), testCredential()); err == nil {
		t.Fatal("identity replacement accepted")
	}
}

func TestQwenSubscriptionImportRejectsMismatchedRefreshIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = io.WriteString(w, `{"access_token":"other-owner-access","refresh_token":"rotated-refresh","expires_in":3600}`)
			return
		}
		subject := "qwen-user"
		if r.Header.Get("Authorization") == "Bearer other-owner-access" {
			subject = "different-owner"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"sub": subject, "email": "owner@example.test", "member_type": "pro"})
	}))
	defer server.Close()
	oldConfig, oldInfo := oauthConfig, userInfoURL
	t.Cleanup(func() { oauthConfig, userInfoURL = oldConfig, oldInfo })
	oauthConfig.TokenURL, userInfoURL = server.URL+"/token", server.URL+"/userinfo"
	if _, err := (Store{Dir: t.TempDir()}).ImportVerifiedCredential(t.Context(), server.Client(), "work", testCredential()); err == nil {
		t.Fatal("access token from one owner and refresh token from another were accepted")
	}
}
