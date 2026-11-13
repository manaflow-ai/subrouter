package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/agents/qwensubscription"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestQwenSubscriptionRepairRechecksEnrollmentBeforePublishing(t *testing.T) {
	for _, remove := range []bool{true, false} {
		t.Run(map[bool]string{true: "removed", false: "replaced"}[remove], func(t *testing.T) {
			store := qwensubscription.Store{Dir: t.TempDir()}
			credential := qwensubscription.CredentialInfo{AccessToken: "subscription-access", RefreshToken: "subscription-refresh", ExpiresAt: time.Now().Add(time.Hour), Subject: "verified-qwen-owner", MemberType: "pro"}
			if _, err := store.SaveManagedCredential("work", credential); err != nil {
				t.Fatal(err)
			}
			ref, err := OpenAccountRefWithSources(t.Context(), accounts.CodexStore{Dir: t.TempDir()}, agentclaude.Store{Dir: t.TempDir()}, &http.Client{Transport: qwenUserinfoTransport{}}, []OAuthAccountSource{store})
			if err != nil {
				t.Fatal(err)
			}
			configured, _ := ref.qwenSubscriptionStore()
			transaction := configured.RefreshTransaction
			configured.RefreshTransaction = func(ctx context.Context, refresh func() error) error {
				if err := transaction(ctx, refresh); err != nil {
					return err
				}
				// Complete removal or re-login after refresh releases its disk lock,
				// then reload so the stale repair cannot rely on a later generation.
				if err := PublishAccountDiskMutation(ctx, ref.store.StoreDir(), func() (bool, error) {
					if remove {
						_, changed, err := store.RemoveManagedAccount("work")
						return changed, err
					}
					credential.AccessToken, credential.RefreshToken = "replacement-access", "replacement-refresh"
					_, err := store.SaveManagedCredential("work", credential)
					return err == nil, err
				}); err != nil {
					return err
				}
				_, err := ref.Reload()
				return err
			}
			ref.oauthSources[0] = configured
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					http.Error(w, "expired", http.StatusUnauthorized)
					return
				}
				if r.Header.Get("Authorization") != "Bearer replacement-access" {
					t.Error("repair replayed a credential superseded by re-login")
				}
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
			}))
			defer upstream.Close()
			sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
			if err != nil {
				t.Fatal(err)
			}
			server := Server{AccountRef: ref, Sessions: sessions, SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)), QwenOAuthUpstream: mustParseURL(t, upstream.URL+"/v1"), MaxBodyBytes: 4096}
			req := httptest.NewRequest(http.MethodPost, "/qwen-oauth/v1/chat/completions", strings.NewReader(`{"model":"qwen3.8-max","messages":[]}`))
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, req)
			available, _ := ref.Snapshot()
			if remove {
				if calls.Load() != 1 || len(available) != 0 || recorder.Code == http.StatusOK {
					t.Fatalf("removed enrollment returned: calls=%d accounts=%d HTTP=%d", calls.Load(), len(available), recorder.Code)
				}
			} else if calls.Load() != 2 || len(available) != 1 || available[0].Token != "replacement-access" || recorder.Code != http.StatusOK {
				t.Fatalf("replacement enrollment not used: calls=%d accounts=%d HTTP=%d", calls.Load(), len(available), recorder.Code)
			}
		})
	}
}

type qwenUserinfoTransport struct{}

func TestQwenSubscriptionMembershipDoesNotClearQuotaExhaustion(t *testing.T) {
	store := qwensubscription.Store{Dir: t.TempDir()}
	acct, err := store.SaveManagedCredential("work", qwensubscription.CredentialInfo{AccessToken: "subscription-access", RefreshToken: "subscription-refresh", ExpiresAt: time.Now().Add(time.Hour), Subject: "verified-qwen-owner", MemberType: "pro"})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: qwenUserinfoTransport{}}
	ref, err := OpenAccountRefWithSources(t.Context(), accounts.CodexStore{Dir: t.TempDir()}, agentclaude.Store{Dir: t.TempDir()}, client, []OAuthAccountSource{store})
	if err != nil {
		t.Fatal(err)
	}
	if plan, windows, err := store.FetchUsage(t.Context(), client, acct); err != nil || plan != "pro" || len(windows) != 0 {
		t.Fatal("membership probe failed", err)
	}
	scheduler := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{AccountID: acct.ID, Provider: acct.Provider, Headroom: 1, ShortHeadroom: 1}}))
	until := time.Now().Add(time.Hour)
	scheduler.MarkExhaustedUntil(acct.Provider, acct.ID, "", until)
	server := Server{AccountRef: ref, SchedulerRef: scheduler}
	scores, scored := server.scoreAccounts(t.Context(), []accounts.Account{acct})
	scheduler.Set(selectacct.NewScheduler(scores))
	if got, blocked := scheduler.BlockedUntilFor(acct.Provider, acct.ID, "", time.Now()); scored != 0 || !blocked || !got.Equal(until) {
		t.Fatalf("membership cleared quota exhaustion: scored=%d blocked=%t until=%v", scored, blocked, got)
	}
}

func TestQwenSubscriptionCatalogRepairsRejectedAccessToken(t *testing.T) {
	for _, secondStatus := range []int{http.StatusOK, http.StatusUnauthorized} {
		t.Run(http.StatusText(secondStatus), func(t *testing.T) {
			store := qwensubscription.Store{Dir: t.TempDir()}
			if _, err := store.SaveManagedCredential("work", qwensubscription.CredentialInfo{AccessToken: "subscription-access", RefreshToken: "subscription-refresh", ExpiresAt: time.Now().Add(time.Hour), Subject: "verified-qwen-owner", MemberType: "pro"}); err != nil {
				t.Fatal(err)
			}
			ref, err := OpenAccountRefWithSources(t.Context(), accounts.CodexStore{Dir: t.TempDir()}, agentclaude.Store{Dir: t.TempDir()}, &http.Client{Transport: qwenUserinfoTransport{}}, []OAuthAccountSource{store})
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					http.Error(w, "expired", http.StatusUnauthorized)
					return
				}
				if r.Header.Get("Authorization") != "Bearer attested-access" {
					t.Error("catalog did not use the repaired grant")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(secondStatus)
				_, _ = io.WriteString(w, `{"data":[{"id":"qwen3.8-max"}]}`)
			}))
			defer upstream.Close()
			sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
			if err != nil {
				t.Fatal(err)
			}
			scheduler := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
			server := Server{AccountRef: ref, Sessions: sessions, SchedulerRef: scheduler, QwenOAuthUpstream: mustParseURL(t, upstream.URL+"/v1")}
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/qwen-oauth/v1/models", nil))
			if calls.Load() != 2 || recorder.Code != secondStatus {
				t.Fatalf("catalog repair calls=%d HTTP=%d", calls.Load(), recorder.Code)
			}
			if _, blocked := scheduler.BlockedUntilFor(accounts.ProviderQwenOAuth, "qwen-subscription:work", "", time.Now()); secondStatus == http.StatusOK && blocked {
				t.Fatal("repaired catalog request left the account exhausted")
			}
		})
	}
}

func TestQwenSubscriptionImportWithSchedulerPublishesWithoutDeadlock(t *testing.T) {
	store := qwensubscription.Store{Dir: t.TempDir()}
	ref, err := OpenAccountRefWithSources(t.Context(), accounts.CodexStore{Dir: t.TempDir()}, agentclaude.Store{Dir: t.TempDir()}, &http.Client{Transport: qwenUserinfoTransport{}}, []OAuthAccountSource{store})
	if err != nil {
		t.Fatal(err)
	}
	server := Server{AccountRef: ref, AccountImportToken: "import-secret", SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))}
	body, err := json.Marshal(accountImportRequest{Provider: accounts.ProviderQwenOAuth, QwenSubscription: &qwenSubscriptionAccountImport{Label: "work", Credential: qwensubscription.CredentialInfo{AccessToken: "subscription-access", RefreshToken: "subscription-refresh", ExpiresAt: time.Now().Add(time.Hour)}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/_subrouter/account-import", strings.NewReader(string(body))).WithContext(ctx)
	req.RemoteAddr = "100.64.0.2:3000"
	req.Header.Set("Authorization", "Bearer import-secret")
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || ctx.Err() != nil {
		t.Fatalf("subscription import stalled during scoring: HTTP %d, %v: %s", recorder.Code, ctx.Err(), recorder.Body.String())
	}
	available, _ := ref.Snapshot()
	if len(available) != 1 || available[0].Token != "attested-access" {
		t.Fatal("scheduled import did not publish the verified credential")
	}
}

func (qwenUserinfoTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.String() == "https://auth.qwen.ai/api/oauth/token" {
		_ = r.ParseForm()
		if r.Form.Get("refresh_token") != "subscription-refresh" {
			return &http.Response{StatusCode: 400, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"invalid_grant"}`))}, nil
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"access_token":"attested-access","refresh_token":"attested-refresh","expires_in":3600}`))}, nil
	}
	if r.URL.String() != "https://auth.qwen.ai/api/oauth/userinfo" || (r.Header.Get("Authorization") != "Bearer subscription-access" && r.Header.Get("Authorization") != "Bearer attested-access") {
		return &http.Response{StatusCode: 401, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"sub":"verified-qwen-owner","email":"owner@example.test","member_type":"pro"}`))}, nil
}

func TestQwenSubscriptionImportHotLoadsVerifiedAccountAndRemovesIt(t *testing.T) {
	store := qwensubscription.Store{Dir: filepath.Join(t.TempDir(), "subscription")}
	ref, err := OpenAccountRefWithSources(t.Context(), accounts.CodexStore{Dir: t.TempDir()}, agentclaude.Store{Dir: t.TempDir()}, &http.Client{Transport: qwenUserinfoTransport{}}, []OAuthAccountSource{store})
	if err != nil {
		t.Fatal(err)
	}
	s := Server{AccountRef: ref, AdminToken: "admin-secret", AccountImportToken: "import-secret"}
	input := accountImportRequest{Provider: accounts.ProviderQwenOAuth, QwenSubscription: &qwenSubscriptionAccountImport{Label: "work", Credential: qwensubscription.CredentialInfo{AccessToken: "subscription-access", RefreshToken: "subscription-refresh", ExpiresAt: time.Now().Add(time.Hour), Email: "untrusted@example.test", MemberType: "untrusted"}}}
	id, err := s.installImportedAccount(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if id != "qwen-subscription:work" {
		t.Fatalf("unexpected canonical ID %s", id)
	}
	listed, err := store.ListAccounts(t.Context())
	if err != nil || len(listed) != 1 {
		t.Fatal("import did not persist", err)
	}
	if listed[0].Token != "attested-access" {
		t.Fatal("server did not take refresh ownership on import")
	}
	if listed[0].Email != "owner@example.test" || listed[0].AccountID != "verified-qwen-owner" {
		t.Fatal("caller-supplied identity was trusted")
	}
	if loaded := s.accountListContext(t.Context()); len(loaded) != 1 || loaded[0].ID != id {
		t.Fatal("published import was not hot-loaded")
	}
	statuses := ref.UsageStatuses(t.Context())
	if len(statuses) != 1 || statuses[0].PlanType != "pro" || statuses[0].QuotaUsageKnown || len(statuses[0].Windows) != 0 {
		t.Fatal("membership status invented quota or omitted the verified plan")
	}
	// Import credentials cannot read administrative usage state.
	req := httptest.NewRequest(http.MethodGet, "/_subrouter/usage-status?snapshot=1", nil)
	req.RemoteAddr = "100.64.0.2:3000"
	req.Header.Set("Authorization", "Bearer import-secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("import token read usage: %d", rec.Code)
	}
	input.QwenSubscription = &qwenSubscriptionAccountImport{Label: "work", Remove: true}
	if _, err := s.installImportedAccount(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	if loaded := s.accountListContext(t.Context()); len(loaded) != 0 {
		t.Fatal("removed profile still serving")
	}
}

func TestQwenSubscriptionImportRejectsMixedAndAPIKeyPayloads(t *testing.T) {
	store := qwensubscription.Store{Dir: t.TempDir()}
	ref, err := OpenAccountRefWithSources(t.Context(), accounts.CodexStore{Dir: t.TempDir()}, agentclaude.Store{Dir: t.TempDir()}, http.DefaultClient, []OAuthAccountSource{store})
	if err != nil {
		t.Fatal(err)
	}
	s := Server{AccountRef: ref}
	for _, input := range []accountImportRequest{
		{Provider: accounts.ProviderCodex, QwenSubscription: &qwenSubscriptionAccountImport{Label: "work"}},
		{Provider: accounts.ProviderQwenOAuth, QwenSubscription: &qwenSubscriptionAccountImport{Label: "work"}, Kimi: &kimiAccountImport{}},
		{Provider: accounts.ProviderQwenOAuth, Codex: &accounts.StoredCodexAccount{}},
	} {
		if _, err := s.installImportedAccount(t.Context(), input); err == nil {
			t.Fatal("invalid subscription import accepted")
		}
	}
	if _, ok := APIKeyProviderForName("qwen-oauth"); ok {
		t.Fatal("subscription accepts API keys")
	}
	if strings.Contains(APIKeyProviderList(), "qwen-oauth") {
		t.Fatal("OAuth-only provider advertised as accepting API keys")
	}
}

func TestQwenSubscriptionRoutesOpenAIProtocolAndKeepsPlanPoolSeparate(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/compatible-mode/v1/chat/completions" {
			t.Errorf("wrong subscription path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer subscription-token" || r.Header.Get("X-Api-Key") != "" {
			t.Error("client credential leaked or subscription bearer missing")
		}
		if r.Header.Get("X-Subrouter-Account-ID") != "" {
			t.Error("local account selector leaked")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := Server{Accounts: []accounts.Account{
		{ID: "qwen-subscription:work", Provider: accounts.ProviderQwenOAuth, AuthMode: accounts.AuthModeOAuth, Token: "subscription-token"},
		{ID: "qwen:plan", Provider: accounts.ProviderQwen, AuthMode: accounts.AuthModeAPIKey, Token: "plan-key"},
	}, Sessions: sessions, QwenOAuthUpstream: mustParseURL(t, upstream.URL+"/compatible-mode/v1"), MaxBodyBytes: 4096}
	handler := s.Handler()
	req := httptest.NewRequest(http.MethodPost, "/qwen-oauth/v1/chat/completions", strings.NewReader(`{"model":"qwen3.8-max","messages":[]}`))
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("X-Api-Key", "client-key")
	req.Header.Set("X-Subrouter-Session", "subscription-test")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 || calls.Load() != 1 {
		t.Fatalf("route returned HTTP %d: %s", rec.Code, rec.Body.String())
	}
	// A plan key cannot be selected on the subscription route.
	req = httptest.NewRequest(http.MethodPost, "/qwen-oauth/v1/chat/completions", strings.NewReader(`{"model":"qwen3.8-max","messages":[]}`))
	req.Header.Set("X-Subrouter-Account-ID", "qwen:plan")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code == 200 || calls.Load() != 1 {
		t.Fatal("subscription route admitted a Coding Plan key")
	}
}

func TestQwenSubscriptionQuotaFailureFailsOverWithinOAuthPool(t *testing.T) {
	var first, second atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer exhausted" {
			first.Add(1)
			http.Error(w, `{"error":{"message":"quota exceeded"}}`, 429)
			return
		}
		second.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer upstream.Close()
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = sessions.Put("qwen-oauth", "sticky", "qwen-subscription:first", "")
	if err != nil {
		t.Fatal(err)
	}
	s := Server{Accounts: []accounts.Account{{ID: "qwen-subscription:first", Provider: accounts.ProviderQwenOAuth, AuthMode: accounts.AuthModeOAuth, Token: "exhausted"}, {ID: "qwen-subscription:second", Provider: accounts.ProviderQwenOAuth, AuthMode: accounts.AuthModeOAuth, Token: "healthy"}}, Sessions: sessions, SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)), QwenOAuthUpstream: mustParseURL(t, upstream.URL+"/v1"), MaxBodyBytes: 4096}
	req := httptest.NewRequest(http.MethodPost, "/qwen-oauth/v1/chat/completions", strings.NewReader(`{"model":"qwen3.8-max","messages":[]}`))
	req.Header.Set("X-Subrouter-Session", "sticky")
	req.Header.Set("X-Subrouter-Agent", "qwen-oauth")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || first.Load() == 0 || second.Load() != 1 {
		t.Fatalf("quota failover HTTP %d; attempts %d/%d", rec.Code, first.Load(), second.Load())
	}
	assigned, ok := sessions.Get("qwen-oauth", "sticky")
	if !ok || assigned.AccountID != "qwen-subscription:second" {
		t.Fatal("successful alternate was not made sticky")
	}
}

func TestQwenSubscriptionRepairsOnePreExpiry401BeforeFailover(t *testing.T) {
	for _, secondStatus := range []int{http.StatusOK, http.StatusUnauthorized} {
		t.Run(http.StatusText(secondStatus), func(t *testing.T) {
			store := qwensubscription.Store{Dir: t.TempDir()}
			_, err := store.SaveManagedCredential("work", qwensubscription.CredentialInfo{
				AccessToken: "subscription-access", RefreshToken: "subscription-refresh", ExpiresAt: time.Now().Add(time.Hour),
				Subject: "verified-qwen-owner", Email: "owner@example.test", MemberType: "pro",
			})
			if err != nil {
				t.Fatal(err)
			}
			ref, err := OpenAccountRefWithSources(t.Context(), accounts.CodexStore{Dir: t.TempDir()}, agentclaude.Store{Dir: t.TempDir()}, &http.Client{Transport: qwenUserinfoTransport{}}, []OAuthAccountSource{store})
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempt := calls.Add(1)
				if attempt == 1 {
					if r.Header.Get("Authorization") != "Bearer subscription-access" {
						t.Error("first attempt did not use the original token")
					}
					http.Error(w, `{"error":"expired access token"}`, http.StatusUnauthorized)
					return
				}
				if r.Header.Get("Authorization") != "Bearer attested-access" {
					t.Error("replay did not use the repaired token")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(secondStatus)
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
			}))
			defer upstream.Close()
			sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
			if err != nil {
				t.Fatal(err)
			}
			server := Server{AccountRef: ref, Sessions: sessions, SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)), QwenOAuthUpstream: mustParseURL(t, upstream.URL+"/v1"), MaxBodyBytes: 4096}
			req := httptest.NewRequest(http.MethodPost, "/qwen-oauth/v1/chat/completions", strings.NewReader(`{"model":"qwen3.8-max","messages":[]}`))
			req.Header.Set("X-Subrouter-Session", "repair-session")
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, req)
			if calls.Load() != 2 {
				t.Fatalf("401 repair made %d attempts, want one initial call and one replay; HTTP %d: %s", calls.Load(), recorder.Code, recorder.Body.String())
			}
			if secondStatus == http.StatusOK && recorder.Code != http.StatusOK {
				t.Fatalf("repaired generation failed: HTTP %d: %s", recorder.Code, recorder.Body.String())
			}
			if secondStatus != http.StatusOK && recorder.Code == http.StatusOK {
				t.Fatal("repeated auth rejection was hidden")
			}
		})
	}
}
