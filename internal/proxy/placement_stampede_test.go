package proxy

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func codexStampedeServer(t *testing.T, logs *bytes.Buffer, scores []selectacct.Score) (Server, *session.Store) {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	accountList := make([]accounts.Account, 0, len(scores))
	for _, score := range scores {
		accountList = append(accountList, accounts.Account{
			ID:       score.AccountID,
			Provider: accounts.ProviderCodex,
			AuthMode: accounts.AuthModeOAuth,
			Token:    "tok-" + score.AccountID,
		})
	}
	server := Server{
		Accounts:     accountList,
		Sessions:     store,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(scores)),
		MaxBodyBytes: 1024,
		Logger:       slog.New(slog.NewTextHandler(logs, nil)),
	}
	return server, store
}

func codexScore(id string, headroom float64) selectacct.Score {
	return selectacct.Score{
		AccountID:     id,
		Provider:      accounts.ProviderCodex,
		Headroom:      headroom,
		ShortHeadroom: headroom,
		Fresh:         true,
	}
}

func codexResponsesRequest(t *testing.T, sessionID string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://subrouter.test/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Agent", "codex")
	req.Header.Set("X-Subrouter-Session", sessionID)
	return req
}

// On 2026-08-18 every new Codex session was placed on the same account
// (aziz+8, the momentary headroom argmax) while six healthy accounts sat
// idle; the account was then drained to its weekly cap and every session on
// it was rerouted at once. New sessions must spread across the healthy pool.
func TestNewCodexSessionsSpreadAcrossHealthyPool(t *testing.T) {
	var logs bytes.Buffer
	server, store := codexStampedeServer(t, &logs, []selectacct.Score{
		codexScore("aziz+8@example.com", 0.96),
		codexScore("aziz+9@example.com", 0.85),
		codexScore("aziz+10@example.com", 0.88),
		codexScore("aziz+11@example.com", 0.88),
		codexScore("aziz+12@example.com", 0.81),
		codexScore("aziz+13@example.com", 0.80),
		codexScore("aziz+14@example.com", 0.87),
	})

	const placements = 60
	for i := 0; i < placements; i++ {
		sessionID := fmt.Sprintf("session-%03d", i)
		req := codexResponsesRequest(t, sessionID)
		if _, _, _, err := server.accountForSessionProvider(accounts.ProviderCodex, "codex", sessionID, req); err != nil {
			t.Fatal(err)
		}
	}

	counts := store.CountByAccount()
	top := 0
	for _, count := range counts {
		if count > top {
			top = count
		}
	}
	if len(counts) < 4 {
		t.Fatalf("new sessions landed on %d accounts (%v), want at least 4 of the 7 healthy accounts", len(counts), counts)
	}
	if top > placements*6/10 {
		t.Fatalf("one account received %d of %d new sessions (%v), want no account above 60%%", top, placements, counts)
	}
}

// When the whole pool sits below the sticky-retention floor (the overnight
// state that produced 26,165 account moves in one day, peaking at 8,043 in
// one hour), moving a session re-bills its entire cached prefix and buys
// nothing: the destination is exactly as empty as the source. The session
// must stay where its cache lives until a materially better account exists.
func TestConstrainedPoolDoesNotPingPongStickySessions(t *testing.T) {
	var logs bytes.Buffer
	server, store := codexStampedeServer(t, &logs, []selectacct.Score{
		codexScore("cooked-a@example.com", 0.03),
		codexScore("cooked-b@example.com", 0.04),
		codexScore("cooked-c@example.com", 0.02),
	})
	if _, err := store.Put("codex", "session-1", "cooked-a@example.com", ""); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 8; i++ {
		req := codexResponsesRequest(t, "session-1")
		account, _, _, err := server.accountForSessionProvider(accounts.ProviderCodex, "codex", "session-1", req)
		if err != nil {
			t.Fatal(err)
		}
		if account.ID != "cooked-a@example.com" {
			t.Fatalf("request %d: session moved to %q although no account is materially better", i, account.ID)
		}
	}
	if strings.Contains(logs.String(), "session moved to another account") {
		t.Fatalf("session ping-ponged across an equally-drained pool: %s", logs.String())
	}
}

// The retention floor is not weakened: as soon as a genuinely usable account
// exists (headroom at or above the new-session threshold), a session on a
// nearly-empty account moves there, once, and then sticks.
func TestStickySessionLeavesConstrainedAccountOnlyForHealthyTarget(t *testing.T) {
	var logs bytes.Buffer
	server, store := codexStampedeServer(t, &logs, []selectacct.Score{
		codexScore("cooked-a@example.com", 0.03),
		codexScore("tight-b@example.com", 0.30), // above retention, below new-session threshold
		codexScore("fresh-c@example.com", 0.90),
	})
	if _, err := store.Put("codex", "session-1", "cooked-a@example.com", ""); err != nil {
		t.Fatal(err)
	}

	req := codexResponsesRequest(t, "session-1")
	account, _, _, err := server.accountForSessionProvider(accounts.ProviderCodex, "codex", "session-1", req)
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != "fresh-c@example.com" {
		t.Fatalf("account = %q, want the session moved to the healthy account fresh-c@example.com", account.ID)
	}

	// The move happened once; the session now sticks to the new account.
	req = codexResponsesRequest(t, "session-1")
	account, _, _, err = server.accountForSessionProvider(accounts.ProviderCodex, "codex", "session-1", req)
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != "fresh-c@example.com" {
		t.Fatalf("account = %q, want the session to stay on fresh-c@example.com", account.ID)
	}
}

// A target that is barely above the retention floor and barely better than
// the current account is not worth a cache-dropping move: it trades a
// nearly-empty account for an almost-as-empty one and re-bills the whole
// prefix for a few percent of runway.
func TestStickySessionIgnoresMarginallyBetterTarget(t *testing.T) {
	var logs bytes.Buffer
	server, store := codexStampedeServer(t, &logs, []selectacct.Score{
		codexScore("cooked-a@example.com", 0.03),
		codexScore("tight-b@example.com", 0.08),
	})
	if _, err := store.Put("codex", "session-1", "cooked-a@example.com", ""); err != nil {
		t.Fatal(err)
	}

	req := codexResponsesRequest(t, "session-1")
	account, _, _, err := server.accountForSessionProvider(accounts.ProviderCodex, "codex", "session-1", req)
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != "cooked-a@example.com" {
		t.Fatalf("account = %q, want the session held on cooked-a@example.com", account.ID)
	}
	if strings.Contains(logs.String(), "session moved to another account") {
		t.Fatalf("session moved for a marginal gain: %s", logs.String())
	}
}
