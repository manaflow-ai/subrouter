package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/storepath"
)

// Claude's prepaid extra-usage balance is not exposed by the OAuth usage API;
// it is only served by claude.ai's first-party web JSON API behind a
// sessionKey browser cookie. This file enriches `sr status` rows locally with
// that balance, the way CodexBar does. It is display-only: routing decisions
// never see these values.
//
// Security rules for this machinery:
//   - sessionKey values are secrets. They are never logged and are persisted
//     only in claude-web-sessions.json under storepath.CodexDir(), mode 0600.
//   - Cookie reading is read-only and macOS-only (build-tagged); everywhere
//     else discovery is a no-op stub.
//   - Any failure leaves status rows exactly as the server/OAuth data
//     produced them.

const (
	claudeWebDefaultBaseURL  = "https://claude.ai/api"
	claudeWebBalanceCacheTTL = 5 * time.Minute
	claudeWebEnrichTimeout   = 4 * time.Second
	claudeWebRequestTimeout  = 5 * time.Second
	claudeWebMaxBodyBytes    = 1 << 20
)

// claudeWebBaseURL is a variable so tests can point the client at an
// httptest server.
var claudeWebBaseURL = claudeWebDefaultBaseURL

// claudeWebDiscoverSessionKeys reads claude.ai sessionKey cookies from local
// browsers. It is platform-specific: sr_claude_balance_darwin.go installs the
// real implementation and sr_claude_balance_other.go installs a no-op stub.
// Tests override and restore it; the discovery path must never touch a real
// Keychain or browser profile in tests.
var claudeWebDiscoverSessionKeys func(ctx context.Context) []claudeWebSessionKeyCandidate

type claudeWebSessionKeyCandidate struct {
	SessionKey string
	Source     string
}

// claudeWebSession is a persisted claude.ai web session. SessionKey is a
// secret: never log it.
type claudeWebSession struct {
	SessionKey string `json:"session_key"`
	Email      string `json:"email,omitempty"`
	OrgID      string `json:"org_id,omitempty"`
	Source     string `json:"source,omitempty"`
}

type claudeWebSessionFile struct {
	Sessions []claudeWebSession `json:"sessions"`
}

func claudeWebSessionsPath() string {
	return filepath.Join(storepath.CodexDir(), "claude-web-sessions.json")
}

func loadClaudeWebSessions() []claudeWebSession {
	data, err := os.ReadFile(claudeWebSessionsPath())
	if err != nil {
		return nil
	}
	var file claudeWebSessionFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil
	}
	return file.Sessions
}

// saveClaudeWebSessions persists sessions with owner-only permissions. The
// file contains session keys, so it must never be world-readable.
func saveClaudeWebSessions(sessions []claudeWebSession) {
	writeClaudeWebJSON(claudeWebSessionsPath(), claudeWebSessionFile{Sessions: sessions})
}

func writeClaudeWebJSON(path string, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return
	}
	// Rename preserves the temp file's mode, but chmod defensively in case an
	// older, looser file was replaced.
	_ = os.Chmod(path, 0600)
}

// claudeWebBalanceCacheEntry is a disk-cached balance reading so repeated
// `sr status` runs are instant and claude.ai is not hammered.
type claudeWebBalanceCacheEntry struct {
	BalanceCents float64   `json:"balance_cents"`
	FetchedAt    time.Time `json:"fetched_at"`
}

type claudeWebBalanceCacheFile struct {
	Balances map[string]claudeWebBalanceCacheEntry `json:"balances"`
}

func claudeWebBalanceCachePath() string {
	return filepath.Join(storepath.CodexDir(), "claude-web-balances.json")
}

func loadClaudeWebBalanceCache() claudeWebBalanceCacheFile {
	data, err := os.ReadFile(claudeWebBalanceCachePath())
	if err != nil {
		return claudeWebBalanceCacheFile{Balances: map[string]claudeWebBalanceCacheEntry{}}
	}
	var file claudeWebBalanceCacheFile
	if err := json.Unmarshal(data, &file); err != nil || file.Balances == nil {
		return claudeWebBalanceCacheFile{Balances: map[string]claudeWebBalanceCacheEntry{}}
	}
	return file
}

func saveClaudeWebBalanceCache(cache claudeWebBalanceCacheFile) {
	writeClaudeWebJSON(claudeWebBalanceCachePath(), cache)
}

var errClaudeWebUnauthorized = errors.New("claude web session unauthorized")

// claudeWebResponse is one HTTP response from the web API transport.
type claudeWebResponse struct {
	status    int
	body      []byte
	setCookie string
}

// claudeWebTransport fetches url with the sessionKey cookie and returns the
// raw response for the caller to map onto errors.
type claudeWebTransport func(ctx context.Context, url, sessionKey string) (claudeWebResponse, error)

// claudeWebDefaultTransport is net/http everywhere except darwin, where
// claude.ai's Cloudflare blocks Go's TLS fingerprint and the Swift URLSession
// probe (sr_claude_balance_darwin.go) is installed instead. Tests override it
// with netHTTPClaudeWebTransport against httptest servers.
var claudeWebDefaultTransport claudeWebTransport = netHTTPClaudeWebTransport

// claudeWebTransportReady reports whether the platform transport can make
// requests right now. On darwin it kicks off the lazy probe compile and
// reports false until the binary exists, so a run that would only fail
// skips browser-cookie reads (and their potential Keychain prompts) entirely.
var claudeWebTransportReady = func() bool { return true }

func netHTTPClaudeWebTransport(ctx context.Context, url, sessionKey string) (claudeWebResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return claudeWebResponse{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cookie", "sessionKey="+sessionKey)
	client := &http.Client{Timeout: claudeWebRequestTimeout}
	res, err := client.Do(req)
	if err != nil {
		return claudeWebResponse{}, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, claudeWebMaxBodyBytes))
	if err != nil {
		return claudeWebResponse{}, err
	}
	return claudeWebResponse{
		status:    res.StatusCode,
		body:      body,
		setCookie: strings.Join(res.Header.Values("Set-Cookie"), "\n"),
	}, nil
}

// claudeWebRotatedSessionKey extracts a rotated sessionKey value from
// Set-Cookie header text. Values that do not look like a Claude session key
// (e.g. sessionKeyV3) are ignored.
func claudeWebRotatedSessionKey(setCookie string) string {
	rest := setCookie
	for {
		idx := strings.Index(rest, "sessionKey=")
		if idx < 0 {
			return ""
		}
		rest = rest[idx+len("sessionKey="):]
		token := rest
		if end := strings.IndexAny(rest, ";,\n\r\t "); end >= 0 {
			token = rest[:end]
		}
		if strings.HasPrefix(token, "sk-ant-") {
			return token
		}
	}
}

// claudeWebIsCloudflareChallenge recognizes Cloudflare's managed challenge
// page, which says the TLS fingerprint was blocked — not that the session is
// dead. It must never be treated as unauthorized.
func claudeWebIsCloudflareChallenge(body []byte) bool {
	prefix := string(body[:min(len(body), 64*1024)])
	return strings.Contains(strings.ToLower(prefix), "just a moment")
}

type claudeWebClient struct {
	baseURL   string
	transport claudeWebTransport
}

func newClaudeWebClient() *claudeWebClient {
	return &claudeWebClient{
		baseURL:   strings.TrimRight(claudeWebBaseURL, "/"),
		transport: claudeWebDefaultTransport,
	}
}

// get performs a session-authenticated GET and folds a rotated sessionKey
// from Set-Cookie back into the session so it survives claude.ai's rotation.
func (c *claudeWebClient) get(ctx context.Context, session *claudeWebSession, path string) ([]byte, error) {
	resp, err := c.transport(ctx, c.baseURL+path, session.SessionKey)
	if err != nil {
		return nil, err
	}
	if rotated := claudeWebRotatedSessionKey(resp.setCookie); rotated != "" {
		session.SessionKey = rotated
	}
	if resp.status == http.StatusUnauthorized || resp.status == http.StatusForbidden {
		if claudeWebIsCloudflareChallenge(resp.body) {
			return nil, fmt.Errorf("claude web api %s blocked by a cloudflare challenge", path)
		}
		return nil, errClaudeWebUnauthorized
	}
	if resp.status != http.StatusOK {
		return nil, fmt.Errorf("claude web api %s returned status %d", path, resp.status)
	}
	return resp.body, nil
}

func (c *claudeWebClient) accountEmail(ctx context.Context, session *claudeWebSession) (string, error) {
	body, err := c.get(ctx, session, "/account")
	if err != nil {
		return "", err
	}
	var response struct {
		EmailAddress string `json:"email_address"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", err
	}
	email := strings.TrimSpace(response.EmailAddress)
	if email == "" {
		return "", errors.New("claude web account response has no email_address")
	}
	return email, nil
}

type claudeWebOrganization struct {
	UUID         string   `json:"uuid"`
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
}

func (org claudeWebOrganization) hasChatCapability() bool {
	for _, capability := range org.Capabilities {
		if strings.EqualFold(strings.TrimSpace(capability), "chat") {
			return true
		}
	}
	return false
}

// organizationID picks the first chat-capable org, else the first org, the
// same defensive selection CodexBar uses.
func (c *claudeWebClient) organizationID(ctx context.Context, session *claudeWebSession) (string, error) {
	body, err := c.get(ctx, session, "/organizations")
	if err != nil {
		return "", err
	}
	var orgs []claudeWebOrganization
	if err := json.Unmarshal(body, &orgs); err != nil {
		return "", err
	}
	for _, org := range orgs {
		if org.UUID != "" && org.hasChatCapability() {
			return org.UUID, nil
		}
	}
	for _, org := range orgs {
		if org.UUID != "" {
			return org.UUID, nil
		}
	}
	return "", errors.New("claude web account has no organization")
}

func (c *claudeWebClient) prepaidBalanceCents(ctx context.Context, session *claudeWebSession, orgID string) (float64, error) {
	body, err := c.get(ctx, session, "/organizations/"+orgID+"/prepaid/credits")
	if err != nil {
		return 0, err
	}
	var response struct {
		Amount   float64 `json:"amount"`
		Currency string  `json:"currency"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return 0, err
	}
	if response.Amount < 0 {
		return 0, fmt.Errorf("claude web prepaid credits returned negative amount")
	}
	return response.Amount, nil
}

// fetchBalance validates the session with a cheap /api/account call, resolves
// the organization, and reads the prepaid credit balance in cents. It updates
// the session in place (email, org, rotated session key).
func (c *claudeWebClient) fetchBalance(ctx context.Context, session *claudeWebSession) (string, float64, error) {
	email, err := c.accountEmail(ctx, session)
	if err != nil {
		return "", 0, err
	}
	session.Email = email
	if session.OrgID == "" {
		orgID, err := c.organizationID(ctx, session)
		if err != nil {
			return "", 0, err
		}
		session.OrgID = orgID
	}
	balance, err := c.prepaidBalanceCents(ctx, session, session.OrgID)
	if err != nil {
		return "", 0, err
	}
	return email, balance, nil
}

// claudeWebBalances resolves prepaid balances (in cents) for the wanted
// lower-cased emails, using the disk cache when fresh, then stored sessions,
// then browser cookie discovery. All failures are silent: whatever could not
// be resolved is simply absent from the result.
func claudeWebBalances(ctx context.Context, wanted map[string]bool) map[string]float64 {
	balances, _ := claudeWebBalancesWithFresh(ctx, wanted)
	return balances
}

// claudeWebBalancesWithFresh additionally reports the balances that were
// fetched from the network during this call (cache hits excluded), so callers
// can fan those out to the server for clients without a local web session.
func claudeWebBalancesWithFresh(ctx context.Context, wanted map[string]bool) (map[string]float64, map[string]float64) {
	balances := map[string]float64{}
	fresh := map[string]float64{}
	if len(wanted) == 0 {
		return balances, fresh
	}
	cache := loadClaudeWebBalanceCache()
	now := time.Now()
	missing := map[string]bool{}
	for email := range wanted {
		if entry, ok := cache.Balances[email]; ok && now.Sub(entry.FetchedAt) < claudeWebBalanceCacheTTL {
			balances[email] = entry.BalanceCents
		} else {
			missing[email] = true
		}
	}
	if len(missing) == 0 {
		return balances, fresh
	}
	if !claudeWebTransportReady() {
		// The transport cannot make requests this run (e.g. the darwin probe
		// binary is still compiling); leave the rows as they are and let the
		// next run pick up the ready transport.
		return balances, fresh
	}

	client := newClaudeWebClient()
	sessions := loadClaudeWebSessions()
	keptSessions := make([]claudeWebSession, 0, len(sessions)+1)
	sessionsChanged := false
	cacheChanged := false

	for _, session := range sessions {
		emailKey := strings.ToLower(strings.TrimSpace(session.Email))
		if emailKey == "" || !missing[emailKey] {
			keptSessions = append(keptSessions, session)
			continue
		}
		email, balance, err := client.fetchBalance(ctx, &session)
		if err != nil {
			if errors.Is(err, errClaudeWebUnauthorized) {
				// The stored key is dead; drop it so the next run re-reads
				// browser cookies instead of retrying a known-bad secret.
				sessionsChanged = true
				continue
			}
			keptSessions = append(keptSessions, session)
			continue
		}
		emailKey = strings.ToLower(email)
		balances[emailKey] = balance
		fresh[emailKey] = balance
		cache.Balances[emailKey] = claudeWebBalanceCacheEntry{BalanceCents: balance, FetchedAt: now}
		cacheChanged = true
		delete(missing, emailKey)
		keptSessions = append(keptSessions, session)
		sessionsChanged = true
	}

	if len(missing) > 0 && claudeWebDiscoverSessionKeys != nil {
		for _, candidate := range claudeWebDiscoverSessionKeys(ctx) {
			if len(missing) == 0 || ctx.Err() != nil {
				break
			}
			key := strings.TrimSpace(candidate.SessionKey)
			if !strings.HasPrefix(key, "sk-ant-") || claudeWebSessionKeyKnown(keptSessions, key) {
				continue
			}
			session := claudeWebSession{SessionKey: key, Source: candidate.Source}
			email, balance, err := client.fetchBalance(ctx, &session)
			if err != nil {
				continue
			}
			emailKey := strings.ToLower(email)
			balances[emailKey] = balance
			fresh[emailKey] = balance
			cache.Balances[emailKey] = claudeWebBalanceCacheEntry{BalanceCents: balance, FetchedAt: now}
			cacheChanged = true
			delete(missing, emailKey)
			keptSessions = append(keptSessions, session)
			sessionsChanged = true
		}
	}

	if sessionsChanged {
		saveClaudeWebSessions(dedupeClaudeWebSessions(keptSessions))
	}
	if cacheChanged {
		saveClaudeWebBalanceCache(cache)
	}
	return balances, fresh
}

func claudeWebSessionKeyKnown(sessions []claudeWebSession, key string) bool {
	for _, session := range sessions {
		if session.SessionKey == key {
			return true
		}
	}
	return false
}

// dedupeClaudeWebSessions keeps one session per email (the most recently
// validated wins) and per key for sessions that never resolved an email.
func dedupeClaudeWebSessions(sessions []claudeWebSession) []claudeWebSession {
	out := make([]claudeWebSession, 0, len(sessions))
	seenKey := map[string]bool{}
	byEmail := map[string]int{}
	for _, session := range sessions {
		if session.SessionKey == "" || seenKey[session.SessionKey] {
			continue
		}
		seenKey[session.SessionKey] = true
		emailKey := strings.ToLower(strings.TrimSpace(session.Email))
		if emailKey != "" {
			if idx, ok := byEmail[emailKey]; ok {
				out[idx] = session
				continue
			}
			byEmail[emailKey] = len(out)
		}
		out = append(out, session)
	}
	return out
}

// enrichClaudeRowsWithWebBalances sets extraUsage.CreditsBalance on every
// Claude row whose email matches a resolved web session. Server rows carry
// the profile name instead of an email when the profile ID is not one, so
// those are resolved through the local Claude profile store first. It runs
// under a short overall timeout and any failure leaves the rows untouched.
func enrichClaudeRowsWithWebBalances(ctx context.Context, rows []srUsageRow) {
	enrichClaudeRowsWithWebBalancesFresh(ctx, rows)
}

// enrichClaudeRowsWithWebBalancesFresh enriches like
// enrichClaudeRowsWithWebBalances and additionally returns the balances that
// were freshly fetched from claude.ai during this call (cache hits excluded),
// so the caller can fan them out to the server.
func enrichClaudeRowsWithWebBalancesFresh(ctx context.Context, rows []srUsageRow) map[string]float64 {
	resolver := newClaudeProfileEmailResolver()
	wanted := map[string]bool{}
	for _, row := range rows {
		if row.provider != accounts.ProviderClaude {
			continue
		}
		if email := resolver.key(row.email); email != "" {
			wanted[email] = true
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	enrichCtx, cancel := context.WithTimeout(ctx, claudeWebEnrichTimeout)
	defer cancel()
	balances, fresh := claudeWebBalancesWithFresh(enrichCtx, wanted)
	if len(balances) == 0 {
		return fresh
	}
	for i := range rows {
		if rows[i].provider != accounts.ProviderClaude {
			continue
		}
		balance, ok := balances[resolver.key(rows[i].email)]
		if !ok {
			continue
		}
		applyClaudeWebBalance(&rows[i], balance)
	}
	return fresh
}

// claudeProfileEmailResolver maps Claude profile names to the account email
// recorded in the profile instance's .claude.json (oauthAccount.emailAddress).
// Lookups are cached per enrichment call and every failure — unknown profile,
// missing file, missing field — resolves to "", leaving the row untouched.
type claudeProfileEmailResolver struct {
	store *agentclaude.Store
	cache map[string]string
}

func newClaudeProfileEmailResolver() *claudeProfileEmailResolver {
	return &claudeProfileEmailResolver{cache: map[string]string{}}
}

// key returns the lower-cased balance-map key for a row's email field: the
// email itself when present, else the resolved profile email, else "".
func (r *claudeProfileEmailResolver) key(name string) string {
	trimmed := strings.TrimSpace(name)
	key := strings.ToLower(trimmed)
	if key == "" || strings.Contains(key, "@") {
		return key
	}
	if email, ok := r.cache[key]; ok {
		return email
	}
	email := r.lookup(trimmed)
	r.cache[key] = email
	return email
}

func (r *claudeProfileEmailResolver) lookup(name string) string {
	if r.store == nil {
		store := agentclaude.DefaultStore()
		r.store = &store
	}
	if _, ok := r.store.FindProfile(name); !ok {
		return ""
	}
	dir := r.store.PreferredInstancePath(r.store.InstancePath(name))
	data, err := os.ReadFile(filepath.Join(dir, ".claude.json"))
	if err != nil {
		return ""
	}
	var config struct {
		OAuthAccount struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return ""
	}
	email := strings.ToLower(strings.TrimSpace(config.OAuthAccount.EmailAddress))
	if !strings.Contains(email, "@") {
		return ""
	}
	return email
}

// applyClaudeWebBalance records the balance on the row's existing extra-usage
// metadata when present — including the synthetic "extra" window local rows
// carry, which must not be shadowed by a fresh row-level value — and creates
// the ExtraUsageInfo when the server sent none.
func applyClaudeWebBalance(row *srUsageRow, balanceCents float64) {
	balance := balanceCents
	if extra := claudeExtraUsageForRow(*row); extra != nil {
		extra.CreditsBalance = &balance
		return
	}
	row.extraUsage = &accounts.ExtraUsageInfo{CreditsBalance: &balance}
}

// pbkdf2SHA1 derives a key per RFC 2898 with HMAC-SHA1. Chromium's macOS
// cookie key is PBKDF2-SHA1(password, salt "saltysalt", 1003 iterations, 16
// bytes); this hand-rolled implementation avoids a golang.org/x/crypto
// dependency.
func pbkdf2SHA1(password, salt []byte, iterations, keyLen int) []byte {
	prf := hmac.New(sha1.New, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen
	var dk []byte
	var blockBuf [4]byte
	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		_, _ = prf.Write(salt)
		binary.BigEndian.PutUint32(blockBuf[:], uint32(block))
		_, _ = prf.Write(blockBuf[:])
		u := prf.Sum(nil)
		t := make([]byte, hashLen)
		copy(t, u)
		for i := 1; i < iterations; i++ {
			prf.Reset()
			_, _ = prf.Write(u)
			u = prf.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}

// decryptChromiumCookieValue decrypts a Chromium "v10" cookie value:
// AES-128-CBC with the PBKDF2-derived key and an IV of 16 space bytes.
// Chrome 80+ prepends a SHA256 of the host key to the plaintext; when the
// plaintext does not itself look like a token, that prefix is stripped.
func decryptChromiumCookieValue(encrypted, key []byte) (string, error) {
	if !bytes.HasPrefix(encrypted, []byte("v10")) {
		return "", errors.New("chromium cookie value is not v10 encrypted")
	}
	payload := encrypted[len("v10"):]
	if len(payload) == 0 || len(payload)%aes.BlockSize != 0 {
		return "", errors.New("chromium cookie value has invalid length")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	iv := bytes.Repeat([]byte(" "), aes.BlockSize)
	plain := make([]byte, len(payload))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, payload)
	if len(plain) == 0 {
		return "", errors.New("chromium cookie value is empty")
	}
	padding := int(plain[len(plain)-1])
	if padding == 0 || padding > aes.BlockSize || padding > len(plain) {
		return "", errors.New("chromium cookie value has invalid padding")
	}
	for _, b := range plain[len(plain)-padding:] {
		if int(b) != padding {
			return "", errors.New("chromium cookie value has invalid padding")
		}
	}
	plain = plain[:len(plain)-padding]
	if text := string(plain); strings.HasPrefix(text, "sk-") {
		return text, nil
	}
	if len(plain) > sha256.Size {
		if text := string(plain[sha256.Size:]); strings.HasPrefix(text, "sk-") {
			return text, nil
		}
	}
	return "", errors.New("decrypted cookie value does not look like a token")
}

// parseClaudeBinaryCookies extracts claude.ai sessionKey values from a Safari
// binarycookies file. The format: "cook" magic, a big-endian page count and
// page-size table, then pages that start with 0x00000100 big-endian and carry
// a little-endian cookie count and offset table. Each record is little-endian
// u32 size/unknown/flags/unknown/string-offsets followed by big-endian
// float64 expiry and creation (Mac absolute time) and null-terminated
// strings; the fixed header is 56 bytes. Every malformed input fails soft:
// whatever could be parsed is returned, the rest is skipped, so machines
// without Full Disk Access just fall through the discovery chain.
func parseClaudeBinaryCookies(data []byte) []string {
	if len(data) < 8 || string(data[:4]) != "cook" {
		return nil
	}
	numPages := int(binary.BigEndian.Uint32(data[4:8]))
	if numPages <= 0 || numPages > 4096 || len(data) < 8+4*numPages {
		return nil
	}
	var out []string
	offset := 8 + 4*numPages
	for i := 0; i < numPages; i++ {
		pageSize := int(binary.BigEndian.Uint32(data[8+4*i : 12+4*i]))
		if pageSize <= 0 || offset+pageSize > len(data) {
			return out
		}
		out = append(out, parseClaudeBinaryCookiePage(data[offset:offset+pageSize])...)
		offset += pageSize
	}
	return out
}

func parseClaudeBinaryCookiePage(page []byte) []string {
	if len(page) < 8 || binary.BigEndian.Uint32(page[0:4]) != 0x00000100 {
		return nil
	}
	count := int(binary.LittleEndian.Uint32(page[4:8]))
	if count <= 0 || count > 1<<20 || len(page) < 8+4*count {
		return nil
	}
	var out []string
	for i := 0; i < count; i++ {
		recordOffset := int(binary.LittleEndian.Uint32(page[8+4*i : 12+4*i]))
		if recordOffset < 0 || recordOffset >= len(page) {
			continue
		}
		if value, ok := parseClaudeBinaryCookieRecord(page[recordOffset:]); ok {
			out = append(out, value)
		}
	}
	return out
}

func parseClaudeBinaryCookieRecord(record []byte) (string, bool) {
	// A record needs at least the 56-byte header before any strings.
	if len(record) < 56 {
		return "", false
	}
	size := int(binary.LittleEndian.Uint32(record[0:4]))
	if size >= 56 && size < len(record) {
		record = record[:size]
	}
	domain := binaryCookieString(record, int(binary.LittleEndian.Uint32(record[16:20])))
	name := binaryCookieString(record, int(binary.LittleEndian.Uint32(record[20:24])))
	value := binaryCookieString(record, int(binary.LittleEndian.Uint32(record[28:32])))
	if name == "sessionKey" && strings.Contains(domain, "claude.ai") && strings.HasPrefix(value, "sk-ant-") {
		return value, true
	}
	return "", false
}

func binaryCookieString(record []byte, offset int) string {
	if offset < 0 || offset >= len(record) {
		return ""
	}
	end := bytes.IndexByte(record[offset:], 0)
	if end < 0 {
		return ""
	}
	return string(record[offset : offset+end])
}
