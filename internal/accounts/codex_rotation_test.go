package accounts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// rotatingOAuthServer models the provider's actual refresh semantics: a refresh
// token is single-use, and presenting a consumed one fails with
// refresh_token_reused. That is the behaviour that killed 11 of 12 shared
// accounts in production, so the tests below exercise refresh against it rather
// than against a server that accepts any token forever.
type rotatingOAuthServer struct {
	mu sync.Mutex
	// live is the only refresh token the provider will currently accept.
	live string
	// consumed records every token already exchanged, so reuse is detectable.
	consumed map[string]bool
	// exchanges counts successful rotations.
	exchanges atomic.Int32
	// reuseAttempts counts presentations of an already-consumed token, which is
	// what permanently breaks an account chain.
	reuseAttempts atomic.Int32
	generation    int
}

func newRotatingOAuthServer(initial string) *rotatingOAuthServer {
	return &rotatingOAuthServer{live: initial, consumed: map[string]bool{}}
}

// expiredJWT builds an access token the refresh logic will consider expired, so
// a refresh is always attempted; freshJWT builds one that is still valid.
func testJWT(email string, expiry time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	claims := fmt.Sprintf(`{"email":%q,"exp":%d}`, email, expiry.Unix())
	return header + "." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".sig"
}

func (s *rotatingOAuthServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if payload.RefreshToken != s.live {
			if s.consumed[payload.RefreshToken] {
				s.reuseAttempts.Add(1)
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":"refresh_token_reused",` +
					`"message":"Your refresh token has already been used to generate a new access token."}}`))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"refresh_token_invalidated","message":"unknown token"}}`))
			return
		}
		s.consumed[payload.RefreshToken] = true
		s.generation++
		s.live = fmt.Sprintf("refresh-gen-%d", s.generation)
		s.exchanges.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  testJWT("chain@example.com", time.Now().Add(time.Hour)),
			"refresh_token": s.live,
			"id_token":      testJWT("chain@example.com", time.Now().Add(time.Hour)),
		})
	}))
	t.Cleanup(server.Close)
	previous := codexOAuthTokenURL
	codexOAuthTokenURL = server.URL
	t.Cleanup(func() { codexOAuthTokenURL = previous })
	return server
}

func seedChainAccount(t *testing.T, store CodexStore, email, refresh string) {
	t.Helper()
	if err := store.SaveStored(StoredCodexAccount{
		Email:    email,
		Provider: ProviderCodex,
		Auth: CodexAuthFile{Tokens: &CodexTokens{
			// Already expired, so every refresh call actually exchanges.
			AccessToken:  testJWT(email, time.Now().Add(-time.Hour)),
			RefreshToken: refresh,
			IDToken:      testJWT(email, time.Now().Add(time.Hour)),
		}},
	}); err != nil {
		t.Fatal(err)
	}
}

func storedRefreshToken(t *testing.T, store CodexStore, email string) string {
	t.Helper()
	account, ok, err := store.FindStored(email)
	if err != nil || !ok {
		t.Fatalf("account missing after refresh: ok=%v err=%v", ok, err)
	}
	if account.Auth.Tokens == nil {
		t.Fatal("account has no tokens after refresh")
	}
	return account.Auth.Tokens.RefreshToken
}

// Refreshing repeatedly must keep working: each exchange rotates the chain and
// the store must always hold the newest token, never a consumed one.
func TestRefreshRotatesRepeatedlyWithoutReuse(t *testing.T) {
	provider := newRotatingOAuthServer("refresh-gen-0")
	provider.start(t)
	store := CodexStore{Dir: t.TempDir()}
	seedChainAccount(t, store, "chain@example.com", "refresh-gen-0")

	previous := "refresh-gen-0"
	for round := 1; round <= 5; round++ {
		account, ok, err := store.FindStored("chain@example.com")
		if err != nil || !ok {
			t.Fatalf("round %d: account missing", round)
		}
		if _, _, err := store.RefreshStored(
			context.Background(), nil, account,
		); err != nil {
			t.Fatalf("round %d refresh failed: %v", round, err)
		}
		current := storedRefreshToken(t, store, "chain@example.com")
		if current == previous {
			t.Fatalf("round %d did not rotate the stored token (%s)", round, current)
		}
		previous = current
	}
	if got := provider.exchanges.Load(); got != 5 {
		t.Fatalf("provider performed %d exchanges, want 5", got)
	}
	if got := provider.reuseAttempts.Load(); got != 0 {
		t.Fatalf("presented a consumed token %d times, want 0", got)
	}
}

// Concurrent refreshes of one account must not present the same token twice.
// Without serialisation each caller reads the same chain and the losers get
// refresh_token_reused, which is terminal for the account.
func TestConcurrentRefreshNeverReusesAToken(t *testing.T) {
	provider := newRotatingOAuthServer("refresh-gen-0")
	provider.start(t)
	store := CodexStore{Dir: t.TempDir()}
	seedChainAccount(t, store, "chain@example.com", "refresh-gen-0")

	account, ok, err := store.FindStored("chain@example.com")
	if err != nil || !ok {
		t.Fatal("seed failed")
	}

	const callers = 10
	var wg sync.WaitGroup
	errs := make([]error, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, refreshErr := store.RefreshStored(
				context.Background(), nil, account,
			)
			errs[i] = refreshErr
		}(i)
	}
	close(start)
	wg.Wait()

	for i, refreshErr := range errs {
		if refreshErr != nil {
			t.Errorf("caller %d failed: %v", i, refreshErr)
		}
	}
	if got := provider.reuseAttempts.Load(); got != 0 {
		t.Fatalf("concurrent refresh presented a consumed token %d times; the account would be dead", got)
	}
	// A forced refresh is meant to exchange, so all ten rotate in turn. What
	// matters is that each one presents the token that was live when its turn
	// came, never a consumed one: exchanges equal callers and reuse stays zero.
	if got := provider.exchanges.Load(); got != callers {
		t.Errorf("%d forced callers produced %d exchanges, want %d", callers, got, callers)
	}
	// The chain must still be usable afterwards.
	final := storedRefreshToken(t, store, "chain@example.com")
	provider.mu.Lock()
	live := provider.live
	provider.mu.Unlock()
	if final != live {
		t.Fatalf("stored token %q is not the provider's live token %q", final, live)
	}
}

// Two store handles on one directory model the daemon and the CLI running at
// once on a single machine. They share the on-disk lock, so this must be safe;
// it is the cross-machine case that cannot be, which is why team mode stops
// local refreshing entirely.
func TestTwoStoreHandlesOnOneDirectoryStaySafe(t *testing.T) {
	provider := newRotatingOAuthServer("refresh-gen-0")
	provider.start(t)
	dir := t.TempDir()
	daemon := CodexStore{Dir: dir}
	cli := CodexStore{Dir: dir}
	seedChainAccount(t, daemon, "chain@example.com", "refresh-gen-0")

	account, _, err := daemon.FindStored("chain@example.com")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for _, store := range []CodexStore{daemon, cli, daemon, cli} {
		wg.Add(1)
		go func(s CodexStore) {
			defer wg.Done()
			if _, _, refreshErr := s.RefreshStored(
				context.Background(), nil, account,
			); refreshErr != nil {
				t.Errorf("refresh through a second handle failed: %v", refreshErr)
			}
		}(store)
	}
	wg.Wait()

	if got := provider.reuseAttempts.Load(); got != 0 {
		t.Fatalf("two handles on one directory reused a token %d times", got)
	}
	if storedRefreshToken(t, cli, "chain@example.com") == "refresh-gen-0" {
		t.Fatal("no rotation happened at all")
	}
}

// A consumed token must produce a terminal, recorded failure rather than a
// silent retry loop, so `sr status` can tell the user to re-add the account.
func TestReusedTokenIsRecordedAsTerminalFailure(t *testing.T) {
	provider := newRotatingOAuthServer("refresh-gen-live")
	provider.start(t)
	store := CodexStore{Dir: t.TempDir()}
	// Seed a token the provider has already consumed elsewhere, which is exactly
	// what a second refresher leaves behind.
	provider.mu.Lock()
	provider.consumed["refresh-stale"] = true
	provider.mu.Unlock()
	seedChainAccount(t, store, "stale@example.com", "refresh-stale")

	account, _, err := store.FindStored("stale@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, refreshErr := store.RefreshStored(
		context.Background(), nil, account,
	); refreshErr == nil {
		t.Fatal("refreshing with a consumed token succeeded")
	}
	if got := provider.reuseAttempts.Load(); got != 1 {
		t.Fatalf("reuse attempts = %d, want 1", got)
	}
	stored, ok, err := store.FindStored("stale@example.com")
	if err != nil || !ok {
		t.Fatal("account disappeared after a failed refresh")
	}
	if stored.Auth.RefreshFailure == nil {
		t.Fatal("terminal refresh failure was not recorded; sr status cannot report it")
	}
}

// The daemon refreshes only when the access token is expired, so a burst of
// scoring passes must collapse into one exchange rather than rotating the chain
// once per caller. The repo covers this with two callers and a stub transport;
// this runs ten against a provider that actually invalidates consumed tokens.
func TestExpiredRefreshHerdCollapsesToOneExchange(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	provider := newRotatingOAuthServer("refresh-gen-0")
	provider.start(t)
	store := CodexStore{Dir: t.TempDir()}
	seedChainAccount(t, store, "chain@example.com", "refresh-gen-0")

	account, ok, err := store.FindStored("chain@example.com")
	if err != nil || !ok {
		t.Fatal("seed failed")
	}

	const callers = 10
	var wg sync.WaitGroup
	start := make(chan struct{})
	failures := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, _, refreshErr := store.RefreshStoredIfExpired(
				context.Background(), nil, account,
			); refreshErr != nil {
				failures <- refreshErr
			}
		}()
	}
	close(start)
	wg.Wait()
	close(failures)
	for refreshErr := range failures {
		t.Errorf("caller failed: %v", refreshErr)
	}

	if got := provider.exchanges.Load(); got != 1 {
		t.Errorf("%d expired-path callers caused %d exchanges, want 1", callers, got)
	}
	if got := provider.reuseAttempts.Load(); got != 0 {
		t.Errorf("expired-path burst reused a token %d times", got)
	}
}
