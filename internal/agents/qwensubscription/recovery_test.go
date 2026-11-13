package qwensubscription

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestQwenSubscriptionPendingRotationSurvivesUserinfoFailureAndRestart(t *testing.T) {
	var rotations atomic.Int32
	var unavailable atomic.Bool
	unavailable.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			rotations.Add(1)
			_, _ = io.WriteString(w, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`)
			return
		}
		if unavailable.Load() {
			http.Error(w, "unavailable", 503)
			return
		}
		_, _ = io.WriteString(w, `{"sub":"qwen-user","email":"verified@example.test","member_type":"plus"}`)
	}))
	defer server.Close()
	oldConfig, oldInfo := oauthConfig, userInfoURL
	t.Cleanup(func() { oauthConfig, userInfoURL = oldConfig, oldInfo })
	oauthConfig.TokenURL, userInfoURL = server.URL+"/token", server.URL+"/userinfo"
	store := Store{Dir: t.TempDir()}
	credential := testCredential()
	acct, err := store.SaveManagedCredential("work", credential)
	if err != nil {
		t.Fatal(err)
	}
	credential.ExpiresAt = time.Now().Add(-time.Minute)
	if _, err := store.write("work", credential); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RefreshAccountIfNeeded(t.Context(), server.Client(), acct); err == nil {
		t.Fatal("userinfo failure was hidden")
	}
	saved, ok, err := store.read("work")
	if err != nil || !ok || saved.RefreshToken != "rotated-refresh" || !saved.VerificationPending {
		t.Fatal("rotation was lost before userinfo completed")
	}
	listed, err := store.ListAccounts(t.Context())
	if err != nil || len(listed) != 1 || listed[0].Token != "" {
		t.Fatal("pending bearer escaped quarantine")
	}
	// A new process must verify the durable replacement without redeeming it again.
	restarted := Store{Dir: store.Dir}
	unavailable.Store(false)
	got, err := restarted.RefreshAccount(t.Context(), server.Client(), acct)
	if err != nil || got.Token != "rotated-access" || rotations.Load() != 1 {
		t.Fatal("restart did not recover the pending credential", err)
	}
	// A prolonged outage may outlive the pending access token. Preserve another
	// rotation and recover rather than attempting userinfo with an expired token.
	saved.VerificationPending = true
	saved.ExpiresAt = time.Now().Add(-time.Minute)
	if _, err := store.write("work", saved); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.RefreshAccount(t.Context(), server.Client(), acct); err != nil || rotations.Load() != 2 {
		t.Fatal("expired pending grant did not recover", err)
	}
}

type cancelUserinfoTransport struct {
	base   http.RoundTripper
	cancel context.CancelFunc
}

func (tr cancelUserinfoTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/userinfo" {
		tr.cancel()
		return nil, context.Canceled
	}
	return tr.base.RoundTrip(r)
}

func TestQwenSubscriptionCancellationAfterExchangeKeepsReplacement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`)
	}))
	defer server.Close()
	oldConfig, oldInfo := oauthConfig, userInfoURL
	t.Cleanup(func() { oauthConfig, userInfoURL = oldConfig, oldInfo })
	oauthConfig.TokenURL, userInfoURL = server.URL+"/token", server.URL+"/userinfo"
	store := Store{Dir: t.TempDir()}
	credential := testCredential()
	acct, err := store.SaveManagedCredential("work", credential)
	if err != nil {
		t.Fatal(err)
	}
	credential.ExpiresAt = time.Now().Add(-time.Minute)
	if _, err := store.write("work", credential); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := &http.Client{Transport: cancelUserinfoTransport{base: server.Client().Transport, cancel: cancel}}
	if _, _, err := store.RefreshAccountIfNeeded(ctx, client, acct); err == nil {
		t.Fatal("cancellation was hidden")
	}
	saved, ok, err := store.read("work")
	if err != nil || !ok || saved.RefreshToken != "new-refresh" || !saved.VerificationPending {
		t.Fatal("cancelled verification lost the new refresh token")
	}
}

func TestQwenSubscriptionImportChecksMembershipBeforeRedemption(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	old := oauthConfig
	t.Cleanup(func() { oauthConfig = old })
	oauthConfig.TokenURL = server.URL
	store := Store{Dir: t.TempDir()}
	credential := testCredential()
	if _, err := store.SaveManagedCredential("existing", credential); err != nil {
		t.Fatal(err)
	}
	for _, refresh := range []string{credential.RefreshToken, "independent-device-grant"} {
		candidate := credential
		candidate.RefreshToken = refresh
		if _, err := store.ImportVerifiedCredential(t.Context(), server.Client(), "alias", candidate); err == nil {
			t.Fatal("duplicate membership was accepted")
		}
	}
	if requests.Load() != 0 {
		t.Fatal("a stored membership's grant was redeemed before duplicate checking")
	}
	saved, ok, err := store.read("existing")
	if err != nil || !ok || saved.RefreshToken != credential.RefreshToken {
		t.Fatal("duplicate import changed the existing grant")
	}
}

func TestQwenSubscriptionConcurrentRejectedCredentialRepairsOnce(t *testing.T) {
	var rotations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			rotations.Add(1)
			_, _ = io.WriteString(w, `{"access_token":"repaired-access","refresh_token":"repaired-refresh","expires_in":3600}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"sub": "qwen-user", "email": "verified@example.test", "member_type": "plus"})
	}))
	defer server.Close()
	oldConfig, oldInfo := oauthConfig, userInfoURL
	t.Cleanup(func() { oauthConfig, userInfoURL = oldConfig, oldInfo })
	oauthConfig.TokenURL, userInfoURL = server.URL+"/token", server.URL+"/userinfo"
	store := Store{Dir: t.TempDir()}
	acct, err := store.SaveManagedCredential("work", testCredential())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			got, err := store.RefreshRejectedAccount(t.Context(), server.Client(), acct)
			if err != nil || got.Token != "repaired-access" {
				t.Error("rejected credential was not repaired", err)
			}
		})
	}
	wg.Wait()
	if rotations.Load() != 1 {
		t.Fatalf("same rejected credential rotated %d times", rotations.Load())
	}
	if _, ok, err := store.RemoveManagedAccount("work"); err != nil || !ok {
		t.Fatal(err)
	}
	if _, err := store.RefreshRejectedAccount(t.Context(), server.Client(), acct); err == nil {
		t.Fatal("removed grant was resurrected")
	}
}
