package claude

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A client that disconnects after the token endpoint has already rotated the
// single-use refresh token must not cost us the new pair: the refresh and its
// persistence complete even though the caller's context is gone.
func TestRefreshCredentialPersistsRotatedPairWhenCallerCancelsMidRefresh(t *testing.T) {
	originalURL := oauthTokenURL
	defer func() { oauthTokenURL = originalURL }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The upstream has redeemed the old refresh token by now. The caller
		// disconnects before the response arrives.
		cancel()
		// Hold the response until the client has either abandoned the
		// connection (caller cancellation reached the transport) or clearly
		// kept waiting for it.
		select {
		case <-r.Context().Done():
		case <-time.After(300 * time.Millisecond):
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`)
	}))
	defer server.Close()
	oauthTokenURL = server.URL

	store := Store{Dir: t.TempDir()}
	if err := store.ImportProfileCredential("cancel@example.com", CredentialInfo{
		AccessToken:  "spent-access",
		RefreshToken: "spent-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	profile, ok := store.FindProfile("cancel@example.com")
	if !ok {
		t.Fatal("profile not found")
	}

	_, _, refreshErr := store.RefreshCredentialIfExpired(ctx, server.Client(), profile)

	stored, err := store.ReadCredential(context.Background(), store.ClaudeConfigDir(profile.Name))
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.RefreshToken != "rotated-refresh" || stored.AccessToken != "rotated-access" {
		t.Fatalf("stored credential after canceled refresh = %+v (refresh err: %v); want the rotated pair persisted", stored, refreshErr)
	}
}

// A same-token metadata rewrite (plan/tier/scopes) that lands during the OAuth
// round trip must not cause the freshly rotated pair to be discarded.
func TestRefreshCredentialKeepsRotatedPairAcrossMetadataOnlyRewrite(t *testing.T) {
	originalURL := oauthTokenURL
	defer func() { oauthTokenURL = originalURL }()

	store := Store{Dir: t.TempDir()}
	original := CredentialInfo{
		AccessToken:      "spent-access",
		RefreshToken:     "spent-refresh",
		SubscriptionType: "pro",
		ExpiresAt:        time.Now().Add(-time.Hour).UnixMilli(),
	}
	if err := store.ImportProfileCredential("meta@example.com", original); err != nil {
		t.Fatal(err)
	}
	profile, ok := store.FindProfile("meta@example.com")
	if !ok {
		t.Fatal("profile not found")
	}
	configDir := store.ClaudeConfigDir(profile.Name)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rewritten := original
		rewritten.SubscriptionType = "max"
		rewritten.RateLimitTier = "default_claude_max_20x"
		rewritten.Scopes = []string{"user:inference", "user:profile"}
		if err := store.WriteCredential(context.Background(), configDir, rewritten); err != nil {
			t.Errorf("metadata rewrite: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`)
	}))
	defer server.Close()
	oauthTokenURL = server.URL

	if _, _, err := store.RefreshCredentialIfExpired(context.Background(), server.Client(), profile); err != nil {
		t.Fatal(err)
	}
	stored, err := store.ReadCredential(context.Background(), configDir)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.AccessToken != "rotated-access" || stored.RefreshToken != "rotated-refresh" {
		t.Fatalf("stored credential = %+v; want the rotated pair, not the spent one", stored)
	}
	if stored.SubscriptionType != "max" || stored.RateLimitTier != "default_claude_max_20x" || len(stored.Scopes) != 2 {
		t.Fatalf("stored metadata = %+v; want the concurrent metadata rewrite preserved", stored)
	}
	if stored.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("stored ExpiresAt = %d; want the refreshed expiry", stored.ExpiresAt)
	}
}
