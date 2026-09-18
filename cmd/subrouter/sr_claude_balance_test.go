package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/storepath"
)

// Chromium's macOS cookie key: PBKDF2-SHA1("password", "saltysalt", 1003, 16).
// Expected value produced by hashlib.pbkdf2_hmac.
func TestPBKDF2SHA1ChromiumKey(t *testing.T) {
	key := pbkdf2SHA1([]byte("password"), []byte("saltysalt"), 1003, 16)
	if got := hex.EncodeToString(key); got != "9395139d5abdba8b749042ad882c0937" {
		t.Fatalf("pbkdf2SHA1 key = %s", got)
	}
}

// v10 fixtures were encrypted with AES-128-CBC, key 9395139d..., IV of 16
// space bytes, over "sk-ant-test-session-key-value". The second has the
// Chrome 80+ SHA256(host_key) plaintext prefix.
func TestDecryptChromiumCookieValue(t *testing.T) {
	key, err := hex.DecodeString("9395139d5abdba8b749042ad882c0937")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		hex  string
	}{
		{"plain", "76313052b43e8a288bf88bf8e59cd64db23ebbe428c35fc15b7983e146b80c7d550085"},
		{"host-prefixed", "7631304a6838bc052d1ab8220899577356683a670f623ff0cff0cba8ffd2839339ae37f406738045c432621fc8dbdbc45913e5d268294ae96b6867bdbfc411ae14ad7b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encrypted, err := hex.DecodeString(tc.hex)
			if err != nil {
				t.Fatal(err)
			}
			value, err := decryptChromiumCookieValue(encrypted, key)
			if err != nil {
				t.Fatal(err)
			}
			if value != "sk-ant-test-session-key-value" {
				t.Fatalf("decrypted %q", value)
			}
		})
	}
	if _, err := decryptChromiumCookieValue([]byte("v11aaaaaaaaaaaaaaaa"), key); err == nil {
		t.Fatal("expected error for non-v10 value")
	}
}

// claudeWebTestServer serves the account -> organizations -> prepaid credits
// chain, rotating sessionKey via Set-Cookie on the credits response.
func claudeWebTestServer(t *testing.T, email, orgID string, balanceCents float64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("sessionKey")
		if err != nil || !strings.HasPrefix(cookie.Value, "sk-ant-") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/account":
			fmt.Fprintf(w, `{"email_address": %q}`, email)
		case "/organizations":
			fmt.Fprintf(w, `[{"uuid": "other", "capabilities": ["api"]}, {"uuid": %q, "capabilities": ["chat"]}]`, orgID)
		case "/organizations/" + orgID + "/prepaid/credits":
			http.SetCookie(w, &http.Cookie{Name: "sessionKey", Value: "sk-ant-rotated"})
			fmt.Fprintf(w, `{"amount": %v, "currency": "USD"}`, balanceCents)
		default:
			http.NotFound(w, r)
		}
	}))
}

func claudeWebTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SUBROUTER_STATE_DIR", t.TempDir())
	// CodexDir() runs a legacy-migration check against ~/.codex-accounts; give
	// tests a temp HOME so they never read the real one.
	t.Setenv("HOME", t.TempDir())
	oldBase := claudeWebBaseURL
	oldDiscover := claudeWebDiscoverSessionKeys
	oldTransport := claudeWebDefaultTransport
	oldReady := claudeWebTransportReady
	// Tests talk to httptest servers over plain net/http, never the Swift
	// probe or a real browser.
	claudeWebDefaultTransport = netHTTPClaudeWebTransport
	claudeWebTransportReady = func() bool { return true }
	t.Cleanup(func() {
		claudeWebBaseURL = oldBase
		claudeWebDiscoverSessionKeys = oldDiscover
		claudeWebDefaultTransport = oldTransport
		claudeWebTransportReady = oldReady
	})
}

func TestClaudeWebFetchBalanceChain(t *testing.T) {
	claudeWebTestEnv(t)
	server := claudeWebTestServer(t, "user@example.com", "org-1", 374)
	defer server.Close()
	claudeWebBaseURL = server.URL

	session := &claudeWebSession{SessionKey: "sk-ant-initial", Source: "test"}
	email, balance, err := newClaudeWebClient().fetchBalance(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	if email != "user@example.com" {
		t.Fatalf("email = %q", email)
	}
	if balance != 374 {
		t.Fatalf("balance = %v", balance)
	}
	if session.OrgID != "org-1" {
		t.Fatalf("org = %q", session.OrgID)
	}
	if session.SessionKey != "sk-ant-rotated" {
		t.Fatalf("session key was not renewed from Set-Cookie")
	}
}

func TestClaudeWebBalances401DropsSession(t *testing.T) {
	claudeWebTestEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()
	claudeWebBaseURL = server.URL
	claudeWebDiscoverSessionKeys = func(context.Context) []claudeWebSessionKeyCandidate { return nil }

	saveClaudeWebSessions([]claudeWebSession{{SessionKey: "sk-ant-dead", Email: "user@example.com"}})
	balances := claudeWebBalances(context.Background(), map[string]bool{"user@example.com": true})
	if len(balances) != 0 {
		t.Fatalf("balances = %v", balances)
	}
	if sessions := loadClaudeWebSessions(); len(sessions) != 0 {
		t.Fatalf("dead session was not dropped: %+v", sessions)
	}
}

func TestClaudeWebBalancesUsesFreshCache(t *testing.T) {
	claudeWebTestEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("network must not be touched when the cache is fresh")
	}))
	defer server.Close()
	claudeWebBaseURL = server.URL

	saveClaudeWebBalanceCache(claudeWebBalanceCacheFile{Balances: map[string]claudeWebBalanceCacheEntry{
		"user@example.com": {BalanceCents: 374, FetchedAt: time.Now()},
	}})
	balances := claudeWebBalances(context.Background(), map[string]bool{"user@example.com": true})
	if balances["user@example.com"] != 374 {
		t.Fatalf("balances = %v", balances)
	}
}

func TestClaudeWebBalancesDiscoversAndPersists(t *testing.T) {
	claudeWebTestEnv(t)
	server := claudeWebTestServer(t, "user@example.com", "org-1", 374)
	defer server.Close()
	claudeWebBaseURL = server.URL
	claudeWebDiscoverSessionKeys = func(context.Context) []claudeWebSessionKeyCandidate {
		return []claudeWebSessionKeyCandidate{
			{SessionKey: "not-a-claude-key", Source: "test"},
			{SessionKey: "sk-ant-discovered", Source: "Chrome"},
		}
	}

	balances := claudeWebBalances(context.Background(), map[string]bool{"user@example.com": true})
	if balances["user@example.com"] != 374 {
		t.Fatalf("balances = %v", balances)
	}

	sessions := loadClaudeWebSessions()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %+v", sessions)
	}
	session := sessions[0]
	if session.SessionKey != "sk-ant-rotated" || session.Email != "user@example.com" || session.OrgID != "org-1" || session.Source != "Chrome" {
		t.Fatalf("persisted session = %+v", session)
	}
	info, err := os.Stat(claudeWebSessionsPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("session file mode = %o", info.Mode().Perm())
	}

	// The fresh cache entry must serve the next run without network or
	// discovery.
	server.Close()
	claudeWebDiscoverSessionKeys = func(context.Context) []claudeWebSessionKeyCandidate {
		t.Error("discovery must not run when the cache is fresh")
		return nil
	}
	balances = claudeWebBalances(context.Background(), map[string]bool{"user@example.com": true})
	if balances["user@example.com"] != 374 {
		t.Fatalf("cached balances = %v", balances)
	}
}

func TestEnrichClaudeRowsWithWebBalances(t *testing.T) {
	claudeWebTestEnv(t)
	server := claudeWebTestServer(t, "user@example.com", "org-1", 374)
	defer server.Close()
	claudeWebBaseURL = server.URL
	claudeWebDiscoverSessionKeys = func(context.Context) []claudeWebSessionKeyCandidate {
		return []claudeWebSessionKeyCandidate{{SessionKey: "sk-ant-discovered", Source: "test"}}
	}

	limit := 5000.0
	used := 4626.0
	rows := []srUsageRow{
		{
			// Case-insensitive email match; extra usage lives on the
			// synthetic window the way local rows carry it.
			email:    "User@Example.com",
			provider: accounts.ProviderClaude,
			authMode: accounts.AuthModeOAuth,
			windows: []accounts.UsageWindow{{
				Name:       "Extra usage",
				ExtraUsage: &accounts.ExtraUsageInfo{IsEnabled: true, MonthlyLimit: &limit, UsedCredits: &used},
			}},
		},
		{
			// No server extra-usage data at all.
			email:    "user@example.com",
			provider: accounts.ProviderClaude,
			authMode: accounts.AuthModeOAuth,
		},
		{
			email:    "other@example.com",
			provider: accounts.ProviderClaude,
			authMode: accounts.AuthModeOAuth,
		},
		{
			email:    "user@example.com",
			provider: accounts.ProviderCodex,
			authMode: accounts.AuthModeOAuth,
		},
	}
	enrichClaudeRowsWithWebBalances(context.Background(), rows)

	extra := claudeExtraUsageForRow(rows[0])
	if extra == nil || extra.CreditsBalance == nil || *extra.CreditsBalance != 374 {
		t.Fatalf("row 0 extra = %+v", extra)
	}
	if extra.MonthlyLimit == nil || *extra.MonthlyLimit != 5000 {
		t.Fatalf("row 0 lost its monthly limit: %+v", extra)
	}
	if rows[1].extraUsage == nil || rows[1].extraUsage.CreditsBalance == nil || *rows[1].extraUsage.CreditsBalance != 374 {
		t.Fatalf("row 1 extra = %+v", rows[1].extraUsage)
	}
	if rows[2].extraUsage != nil {
		t.Fatalf("unmatched row was enriched: %+v", rows[2].extraUsage)
	}
	if rows[3].extraUsage != nil {
		t.Fatalf("non-Claude row was enriched: %+v", rows[3].extraUsage)
	}
}

func TestEnrichClaudeRowsWithWebBalancesNoClaudeRows(t *testing.T) {
	claudeWebTestEnv(t)
	claudeWebDiscoverSessionKeys = func(context.Context) []claudeWebSessionKeyCandidate {
		t.Error("discovery must not run without Claude rows")
		return nil
	}
	rows := []srUsageRow{{email: "user@example.com", provider: accounts.ProviderCodex}}
	enrichClaudeRowsWithWebBalances(context.Background(), rows)
}

func TestEnrichClaudeRowsWithWebBalancesNonDarwinNoop(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("non-darwin stub behavior")
	}
	claudeWebTestEnv(t)
	rows := []srUsageRow{{email: "user@example.com", provider: accounts.ProviderClaude}}
	enrichClaudeRowsWithWebBalances(context.Background(), rows)
	if rows[0].extraUsage != nil {
		t.Fatalf("non-darwin stub enriched a row: %+v", rows[0].extraUsage)
	}
}

func TestUsageGridClaudeExtraSpendCellPrefersBalance(t *testing.T) {
	limit := 5000.0
	used := 2204.0
	balance := 374.0
	zero := 0.0

	cell := usageGridClaudeExtraSpendCell(srUsageRow{extraUsage: &accounts.ExtraUsageInfo{
		IsEnabled: true, MonthlyLimit: &limit, UsedCredits: &used, CreditsBalance: &balance,
	}})
	if cell.Text != "$3.74/$50.00" || cell.Style != ansiGreen {
		t.Fatalf("balance cell = %+v", cell)
	}

	cell = usageGridClaudeExtraSpendCell(srUsageRow{extraUsage: &accounts.ExtraUsageInfo{
		IsEnabled: true, MonthlyLimit: &limit, CreditsBalance: &zero,
	}})
	if cell.Text != "$0.00/$50.00" || cell.Style != ansiYellow {
		t.Fatalf("empty balance cell = %+v", cell)
	}

	cell = usageGridClaudeExtraSpendCell(srUsageRow{extraUsage: &accounts.ExtraUsageInfo{
		IsEnabled: true, MonthlyLimit: &limit, UsedCredits: &used,
	}})
	if cell.Text != "$22.04/$50.00" || cell.Style != ansiGreen {
		t.Fatalf("used/limit fallback cell = %+v", cell)
	}

	cell = usageGridClaudeExtraSpendCell(srUsageRow{extraUsage: &accounts.ExtraUsageInfo{
		IsEnabled: true, UsedCredits: &used, CreditsBalance: &balance,
	}})
	if cell.Text != "?" {
		t.Fatalf("missing limit cell = %+v", cell)
	}

	cell = usageGridClaudeExtraSpendCell(srUsageRow{extraUsage: &accounts.ExtraUsageInfo{
		IsEnabled: false, MonthlyLimit: &limit, CreditsBalance: &balance,
	}})
	if cell.Text != "$3.74/$50.00" || cell.Style != ansiGreen {
		t.Fatalf("balance-only cell = %+v", cell)
	}
}

// A stored session that is still valid must be reused instead of re-reading
// browser cookies, and the rotated key must be persisted back.
func TestClaudeWebBalancesReusesStoredSession(t *testing.T) {
	claudeWebTestEnv(t)
	server := claudeWebTestServer(t, "user@example.com", "org-1", 900)
	defer server.Close()
	claudeWebBaseURL = server.URL
	claudeWebDiscoverSessionKeys = func(context.Context) []claudeWebSessionKeyCandidate {
		t.Error("discovery must not run when a stored session works")
		return nil
	}

	saveClaudeWebSessions([]claudeWebSession{{SessionKey: "sk-ant-stored", Email: "user@example.com"}})
	balances := claudeWebBalances(context.Background(), map[string]bool{"user@example.com": true})
	if balances["user@example.com"] != 900 {
		t.Fatalf("balances = %v", balances)
	}
	sessions := loadClaudeWebSessions()
	if len(sessions) != 1 || sessions[0].SessionKey != "sk-ant-rotated" {
		data, _ := json.Marshal(sessions)
		t.Fatalf("sessions after renewal = %s", data)
	}
}

// binaryCookieRecord builds one Safari binarycookies record with strings laid
// out after the 56-byte header (size, unknown, flags, unknown, four string
// offsets, 8-byte end marker, big-endian float64 expiry and creation).
func binaryCookieRecord(domain, name, path, value string) []byte {
	stringsBlob := domain + "\x00" + name + "\x00" + path + "\x00" + value + "\x00"
	record := make([]byte, 56+len(stringsBlob))
	binary.LittleEndian.PutUint32(record[0:4], uint32(len(record)))
	nameOffset := 56 + len(domain) + 1
	pathOffset := nameOffset + len(name) + 1
	valueOffset := pathOffset + len(path) + 1
	binary.LittleEndian.PutUint32(record[16:20], 56)
	binary.LittleEndian.PutUint32(record[20:24], uint32(nameOffset))
	binary.LittleEndian.PutUint32(record[24:28], uint32(pathOffset))
	binary.LittleEndian.PutUint32(record[28:32], uint32(valueOffset))
	binary.BigEndian.PutUint64(record[40:48], math.Float64bits(800000000))
	binary.BigEndian.PutUint64(record[48:56], math.Float64bits(700000000))
	copy(record[56:], stringsBlob)
	return record
}

// buildBinaryCookiesFixture assembles a single-page binarycookies file.
func buildBinaryCookiesFixture(records ...[]byte) []byte {
	page := make([]byte, 8+4*len(records))
	binary.BigEndian.PutUint32(page[0:4], 0x00000100)
	binary.LittleEndian.PutUint32(page[4:8], uint32(len(records)))
	offset := len(page)
	for i, record := range records {
		binary.LittleEndian.PutUint32(page[8+4*i:12+4*i], uint32(offset))
		page = append(page, record...)
		offset += len(record)
	}
	page = append(page, 0, 0, 0, 0) // page footer
	var be [4]byte
	data := []byte("cook")
	binary.BigEndian.PutUint32(be[:], 1)
	data = append(data, be[:]...)
	binary.BigEndian.PutUint32(be[:], uint32(len(page)))
	data = append(data, be[:]...)
	return append(data, page...)
}

func TestParseClaudeBinaryCookies(t *testing.T) {
	fixture := buildBinaryCookiesFixture(
		binaryCookieRecord(".claude.ai", "sessionKey", "/", "sk-ant-safari-key"),
		binaryCookieRecord(".example.com", "sessionKey", "/", "sk-ant-wrong-domain"),
		binaryCookieRecord(".claude.ai", "otherCookie", "/", "sk-ant-wrong-name"),
		binaryCookieRecord(".claude.ai", "sessionKey", "/", "not-a-session-key"),
	)
	got := parseClaudeBinaryCookies(fixture)
	if len(got) != 1 || got[0] != "sk-ant-safari-key" {
		t.Fatalf("parseClaudeBinaryCookies = %v", got)
	}
}

func TestParseClaudeBinaryCookiesSoftFail(t *testing.T) {
	valid := buildBinaryCookiesFixture(binaryCookieRecord(".claude.ai", "sessionKey", "/", "sk-ant-safari-key"))
	cases := map[string][]byte{
		"empty":            nil,
		"garbage":          []byte("not a cookies file at all, just text"),
		"bad magic":        append([]byte("xxxx"), make([]byte, 64)...),
		"truncated header": []byte("cook\x00"),
		"zero pages":       []byte("cook\x00\x00\x00\x00"),
		"absurd pages":     []byte("cook\xff\xff\xff\xff"),
		"truncated page":   valid[:len(valid)/2],
	}
	for name, data := range cases {
		if got := parseClaudeBinaryCookies(data); len(got) != 0 {
			t.Errorf("%s: parseClaudeBinaryCookies = %v, want soft-fail", name, got)
		}
	}
}

func TestClaudeWebRotatedSessionKey(t *testing.T) {
	cases := []struct {
		name      string
		setCookie string
		want      string
	}{
		{"simple", "sessionKey=sk-ant-rotated; Path=/; HttpOnly", "sk-ant-rotated"},
		{"among others", "other=1; sessionKey=sk-ant-new; Expires=Wed, 21 Oct 2041 07:28:00 GMT", "sk-ant-new"},
		{"ignores v3 cookie", "sessionKeyV3=sk-ant-v3; Path=/", ""},
		{"ignores non-claude value", "sessionKey=not-a-key; Path=/", ""},
		{"multi-line", "x=1\nsessionKey=sk-ant-second; Path=/", "sk-ant-second"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		if got := claudeWebRotatedSessionKey(tc.setCookie); got != tc.want {
			t.Errorf("%s: claudeWebRotatedSessionKey = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A Cloudflare managed challenge (403 "Just a moment") means the TLS
// fingerprint was blocked, not that the session died; the stored session must
// survive so a later, unblocked run can use it.
func TestClaudeWebBalancesCloudflareChallengeKeepsSession(t *testing.T) {
	claudeWebTestEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "<html><title>Just a moment...</title></html>")
	}))
	defer server.Close()
	claudeWebBaseURL = server.URL
	claudeWebDiscoverSessionKeys = func(context.Context) []claudeWebSessionKeyCandidate { return nil }

	saveClaudeWebSessions([]claudeWebSession{{SessionKey: "sk-ant-stored", Email: "user@example.com"}})
	balances := claudeWebBalances(context.Background(), map[string]bool{"user@example.com": true})
	if len(balances) != 0 {
		t.Fatalf("balances = %v", balances)
	}
	sessions := loadClaudeWebSessions()
	if len(sessions) != 1 || sessions[0].SessionKey != "sk-ant-stored" {
		t.Fatalf("cloudflare challenge dropped the session: %+v", sessions)
	}
}

// When the transport cannot make requests this run (probe still compiling),
// enrichment must bail before any session validation or browser discovery.
func TestClaudeWebBalancesTransportNotReady(t *testing.T) {
	claudeWebTestEnv(t)
	claudeWebTransportReady = func() bool { return false }
	claudeWebDiscoverSessionKeys = func(context.Context) []claudeWebSessionKeyCandidate {
		t.Error("discovery must not run when the transport is not ready")
		return nil
	}
	saveClaudeWebSessions([]claudeWebSession{{SessionKey: "sk-ant-stored", Email: "user@example.com"}})
	if balances := claudeWebBalances(context.Background(), map[string]bool{"user@example.com": true}); len(balances) != 0 {
		t.Fatalf("balances = %v", balances)
	}
	sessions := loadClaudeWebSessions()
	if len(sessions) != 1 {
		t.Fatalf("sessions touched while transport not ready: %+v", sessions)
	}
}

// Server status rows carry the Claude profile name when the profile ID is not
// an email; enrichment must resolve it through the local profile store's
// .claude.json (oauthAccount.emailAddress) before matching the balance map.
func TestEnrichClaudeRowsResolvesProfileNames(t *testing.T) {
	claudeWebTestEnv(t)

	// Profile "daniel-raffel" with an instance dir recording the account email.
	codexDir := storepath.CodexDir()
	if err := os.MkdirAll(codexDir, 0700); err != nil {
		t.Fatal(err)
	}
	profiles := `{"profiles":{"daniel-raffel":{"name":"daniel-raffel","createdAt":"2026-01-01T00:00:00Z","dir":"daniel-raffel"}}}`
	if err := os.WriteFile(filepath.Join(codexDir, "claude.json"), []byte(profiles), 0600); err != nil {
		t.Fatal(err)
	}
	instanceDir := filepath.Join(codexDir, "claude", "daniel-raffel")
	if err := os.MkdirAll(instanceDir, 0700); err != nil {
		t.Fatal(err)
	}
	config := `{"oauthAccount":{"emailAddress":"daniel.raffel@gmail.com"}}`
	if err := os.WriteFile(filepath.Join(instanceDir, ".claude.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}

	server := claudeWebTestServer(t, "daniel.raffel@gmail.com", "org-1", 365)
	defer server.Close()
	claudeWebBaseURL = server.URL
	claudeWebDiscoverSessionKeys = func(context.Context) []claudeWebSessionKeyCandidate {
		return []claudeWebSessionKeyCandidate{{SessionKey: "sk-ant-discovered", Source: "test"}}
	}
	// Rows already carrying a real email still match directly; serve this one
	// from a fresh cache entry so the test needs only one web account.
	saveClaudeWebBalanceCache(claudeWebBalanceCacheFile{Balances: map[string]claudeWebBalanceCacheEntry{
		"user@example.com": {BalanceCents: 500, FetchedAt: time.Now()},
	}})

	rows := []srUsageRow{
		{email: "daniel-raffel", provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth},
		{email: "unknown-name", provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth},
		{email: "user@example.com", provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth},
	}
	enrichClaudeRowsWithWebBalances(context.Background(), rows)

	if rows[0].extraUsage == nil || rows[0].extraUsage.CreditsBalance == nil || *rows[0].extraUsage.CreditsBalance != 365 {
		t.Fatalf("profile-named row not enriched: %+v", rows[0].extraUsage)
	}
	if rows[1].extraUsage != nil {
		t.Fatalf("unknown profile name was enriched: %+v", rows[1].extraUsage)
	}
	if rows[2].extraUsage == nil || rows[2].extraUsage.CreditsBalance == nil || *rows[2].extraUsage.CreditsBalance != 500 {
		t.Fatalf("direct email row not enriched: %+v", rows[2].extraUsage)
	}
}

func TestPushClaudeWebBalances(t *testing.T) {
	type push struct {
		Email        string  `json:"email"`
		BalanceCents float64 `json:"balance_cents"`
	}
	var got []push
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_subrouter/claude-web-balance" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var p push
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		got = append(got, p)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	runner := srRunner{}
	config := srServerConfig{Name: "test", URL: server.URL, requestClient: server.Client()}
	runner.pushClaudeWebBalances(context.Background(), config, map[string]float64{"user@example.com": 365})
	if len(got) != 1 || got[0].Email != "user@example.com" || got[0].BalanceCents != 365 {
		t.Fatalf("pushes = %+v", got)
	}

	// Push failures are silent: a dead server must not error, hang, or panic.
	server.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runner.pushClaudeWebBalances(context.Background(), config, map[string]float64{"user@example.com": 365})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("push to a dead server did not return")
	}
}

// The fan-out only pushes what was freshly fetched; cache hits are excluded.
func TestClaudeWebBalancesFreshReporting(t *testing.T) {
	claudeWebTestEnv(t)
	server := claudeWebTestServer(t, "user@example.com", "org-1", 900)
	defer server.Close()
	claudeWebBaseURL = server.URL
	claudeWebDiscoverSessionKeys = func(context.Context) []claudeWebSessionKeyCandidate { return nil }

	saveClaudeWebBalanceCache(claudeWebBalanceCacheFile{Balances: map[string]claudeWebBalanceCacheEntry{
		"cached@example.com": {BalanceCents: 100, FetchedAt: time.Now()},
	}})
	saveClaudeWebSessions([]claudeWebSession{{SessionKey: "sk-ant-stored", Email: "user@example.com"}})

	balances, fresh := claudeWebBalancesWithFresh(context.Background(), map[string]bool{
		"user@example.com":   true,
		"cached@example.com": true,
	})
	if balances["user@example.com"] != 900 || balances["cached@example.com"] != 100 {
		t.Fatalf("balances = %v", balances)
	}
	if len(fresh) != 1 || fresh["user@example.com"] != 900 {
		t.Fatalf("fresh = %v, want only the network-fetched balance", fresh)
	}
}
