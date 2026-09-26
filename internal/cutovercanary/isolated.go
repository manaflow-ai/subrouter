package cutovercanary

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func runIsolatedFailover(ctx context.Context) (int, error) {
	total := 0
	for _, responseDriven := range []bool{false, true} {
		attempts, err := isolatedCase(ctx, responseDriven)
		if err != nil {
			return total, err
		}
		total += attempts
	}
	return total, nil
}

func isolatedCase(ctx context.Context, responseDriven bool) (int, error) {
	var mu sync.Mutex
	hits := map[string]int{}
	marker := "SUBROUTER_ISOLATED_PROOF"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		hits[token]++
		mu.Unlock()
		if responseDriven && token == "token-a" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\""+marker+"\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
	}))
	defer upstream.Close()
	tempDir, err := os.MkdirTemp("", "subrouter-canary-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(tempDir)
	store, err := session.NewStore(filepath.Join(tempDir, "sessions.json"))
	if err != nil {
		return 0, err
	}
	const sessionID = "isolated-sticky"
	if _, err := store.Put("codex", sessionID, "account-a", ""); err != nil {
		return 0, err
	}
	scores := []selectacct.Score{
		{AccountID: "account-a", Provider: accounts.ProviderCodex, Headroom: 1, ShortHeadroom: 1, Fresh: true},
		{AccountID: "account-b", Provider: accounts.ProviderCodex, Headroom: 1, ShortHeadroom: 1, Fresh: true},
	}
	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler(scores))
	if !responseDriven {
		ref.MarkExhausted(accounts.ProviderCodex, "account-a", "")
	}
	u, err := url.Parse(upstream.URL)
	if err != nil {
		return 0, err
	}
	server := proxy.Server{
		CodexUpstream: u,
		Accounts: []accounts.Account{
			{ID: "account-a", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "token-a"},
			{ID: "account-b", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "token-b"},
		},
		Sessions: store, SchedulerRef: ref, MaxBodyBytes: 4096,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	body, _ := responseRequest("gpt-5.6-sol", marker)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body))).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "codex")
	req.Header.Set("X-Subrouter-Session", sessionID)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, req)
	if response.Code < 200 || response.Code >= 300 || !markerResponse(response.Body.Bytes(), marker) {
		return 0, errors.New("isolated failover response not proven")
	}
	assignment, ok := store.Get("codex", sessionID)
	if !ok || assignment.AccountID != "account-b" {
		return 0, errors.New("isolated failover did not move sticky assignment")
	}
	mu.Lock()
	defer mu.Unlock()
	if responseDriven {
		if hits["token-a"] != 1 || hits["token-b"] != 1 {
			return 0, errors.New("response-driven failover attempt count invalid")
		}
		return 2, nil
	}
	if hits["token-a"] != 0 || hits["token-b"] != 1 {
		return 0, errors.New("pre-exhausted failover attempt count invalid")
	}
	return 1, nil
}
