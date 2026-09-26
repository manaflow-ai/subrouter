package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

func TestClaudeWebBalanceStoreRoundTrip(t *testing.T) {
	store := newClaudeWebBalanceStore(filepath.Join(t.TempDir(), "claude-web-balances.json"))
	if err := store.set("user@example.com", 365, "push"); err != nil {
		t.Fatal(err)
	}
	record, ok := store.record("user@example.com")
	if !ok || record.BalanceCents != 365 {
		t.Fatalf("record = %+v, %v", record, ok)
	}
	info, err := os.Stat(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("store file mode = %o", info.Mode().Perm())
	}

	// A stale entry is treated as absent.
	stale := claudeWebBalanceFile{Balances: map[string]claudeWebBalanceRecord{
		"old@example.com": {BalanceCents: 100, FetchedAt: time.Now().Add(-2 * claudeWebBalanceTTL)},
	}}
	data, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.record("old@example.com"); ok {
		t.Fatal("stale record was served")
	}
}

func TestClaudeWebBalanceEndpointAuthAndValidation(t *testing.T) {
	codexDir := t.TempDir()
	ref := &AccountRef{
		store:       accounts.CodexStore{Dir: codexDir},
		claudeStore: agentclaude.Store{Dir: t.TempDir()},
	}
	handler := Server{AccountRef: ref, AdminToken: "admin"}.Handler()

	post := func(auth, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/_subrouter/claude-web-balance", bytes.NewBufferString(body))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		return resp
	}

	if resp := post("", `{"email":"user@example.com","balance_cents":365}`); resp.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: status = %d", resp.Code)
	}
	if resp := post("Bearer wrong", `{"email":"user@example.com","balance_cents":365}`); resp.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: status = %d", resp.Code)
	}
	if resp := post("Bearer admin", `{"email":"not-an-email","balance_cents":365}`); resp.Code != http.StatusBadRequest {
		t.Fatalf("bad email: status = %d", resp.Code)
	}
	if resp := post("Bearer admin", `{"email":"user@example.com","balance_cents":-5}`); resp.Code != http.StatusBadRequest {
		t.Fatalf("negative balance: status = %d", resp.Code)
	}
	if resp := post("Bearer admin", `{"email":" User@Example.com ","balance_cents":365}`); resp.Code != http.StatusOK {
		t.Fatalf("valid push: status = %d body = %s", resp.Code, resp.Body.String())
	}
	// The push landed in the store file under the account store dir,
	// normalized to lower case.
	store := newClaudeWebBalanceStore(filepath.Join(codexDir, "claude-web-balances.json"))
	record, ok := store.record("user@example.com")
	if !ok || record.BalanceCents != 365 {
		t.Fatalf("stored record = %+v, %v", record, ok)
	}
}

func TestClaudeWebBalanceMergesIntoUsageStatus(t *testing.T) {
	claudeStore := agentclaude.Store{Dir: t.TempDir()}
	credential := agentclaude.CredentialInfo{
		AccessToken: "token", RefreshToken: "refresh",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}
	if err := claudeStore.ImportProfileCredential("work", credential); err != nil {
		t.Fatal(err)
	}
	dir := claudeStore.PreferredInstancePath(claudeStore.InstancePath("work"))
	config := `{"oauthAccount":{"emailAddress":"user@example.com"}}`
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}

	codexDir := t.TempDir()
	ref := &AccountRef{
		store:       accounts.CodexStore{Dir: codexDir},
		claudeStore: claudeStore,
		client: &http.Client{Transport: proxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewBufferString(`{"five_hour":{"utilization":0,"resets_at":"2099-01-01T00:00:00Z"}}`)),
			}, nil
		})},
	}
	handler := Server{AccountRef: ref, AdminToken: "admin"}.Handler()

	push := httptest.NewRequest(http.MethodPost, "/_subrouter/claude-web-balance", bytes.NewBufferString(`{"email":"user@example.com","balance_cents":365}`))
	push.Header.Set("Authorization", "Bearer admin")
	pushResp := httptest.NewRecorder()
	handler.ServeHTTP(pushResp, push)
	if pushResp.Code != http.StatusOK {
		t.Fatalf("push status = %d body = %s", pushResp.Code, pushResp.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/_subrouter/usage-status", nil)
	req.Header.Set("Authorization", "Bearer admin")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("usage-status = %d body = %s", resp.Code, resp.Body.String())
	}
	var statuses []AccountUsageStatus
	if err := json.Unmarshal(resp.Body.Bytes(), &statuses); err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 {
		t.Fatalf("statuses = %+v", statuses)
	}
	extra := statuses[0].ExtraUsage
	if extra == nil || extra.CreditsBalance == nil || *extra.CreditsBalance != 365 {
		t.Fatalf("extra usage = %+v", extra)
	}
	// The merge attaches only CreditsBalance; it must not invent enablement.
	if extra.IsEnabled || extra.MonthlyLimit != nil || extra.UsedCredits != nil {
		t.Fatalf("merge invented fields: %+v", extra)
	}
}
