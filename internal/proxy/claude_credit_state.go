package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Claude API-credit and revocation state.
//
// Both free Claude pools are use-it-or-lose-it: plan quota is lost at each
// 5h/weekly reset, and a Console credit grant is lost at its expiry (the Max
// billing renewal). The router therefore needs to know, per API key, how much
// grant is left and when it expires, so it can pull a key ahead of plan
// quota when the key would otherwise expire with credit unspent. Anthropic
// exposes neither the balance nor the expiry of a key, so this file keeps
// the router's own view: an operator-set (or defaulted) grant and expiry,
// spend metered from response usage at API list prices, a decaying burn
// rate, and a "credit balance is too low" hold that lasts until the period
// rolls over (with one re-probe a day).
//
// It also records Claude OAuth credentials that Anthropic has revoked, so
// they stay out of routing until the credential changes, across restarts.
//
// Several workers share one state file during a canary rollout, so every
// write is a locked read-modify-write and metered spend is added as a delta.

const (
	// claudeCreditDefaultGrantUSD is the Claude Max monthly API-credit grant,
	// assumed until an operator sets the real amount.
	claudeCreditDefaultGrantUSD = 200.0
	// claudeCreditDefaultPeriod is the assumed grant period when the expiry
	// is unknown: grant date + 30 days.
	claudeCreditDefaultPeriod = 30 * 24 * time.Hour
	// claudeCreditBurnTau is the time constant of the burn-rate EWMA.
	claudeCreditBurnTau = 6 * time.Hour
	// claudeCreditFinalWindow promotes a key with credit left unconditionally.
	claudeCreditFinalWindow = 48 * time.Hour
	// claudeCreditOnPaceFactor: the current burn must project at least this
	// multiple of the remaining credit by expiry to count as on pace.
	claudeCreditOnPaceFactor = 1.2
	// claudeCreditBehindSlack is how far (as a fraction of the grant) metered
	// spend may trail a linear schedule before the key is promoted. It keeps
	// plan quota first early in a period, when a linear schedule expects
	// almost nothing.
	claudeCreditBehindSlack = 0.10
	// claudeCreditReprobeInterval is how often a key held for a spent balance
	// gets one probe request before its period rolls over.
	claudeCreditReprobeInterval = 24 * time.Hour
	// claudeCostStateFlushInterval bounds how long metered spend stays only
	// in memory, and how stale another worker's writes can look.
	claudeCostStateFlushInterval = 15 * time.Second
)

const (
	claudeCreditPaceAhead    = "on schedule"
	claudeCreditPaceOnPace   = "on pace"
	claudeCreditPaceBehind   = "behind, promoted"
	claudeCreditPaceFinal    = "final 48h, promoted"
	claudeCreditPaceSpent    = "spent"
	claudeCreditPaceReprobe  = "spent, re-probing"
	claudeCreditPaceMetered0 = "metered out"
)

type claudeCreditKeyState struct {
	AmountUSD      float64   `json:"amount_usd"`
	AmountSet      bool      `json:"amount_set,omitempty"`
	PeriodStart    time.Time `json:"period_start"`
	ExpiresAt      time.Time `json:"expires_at"`
	ExpirySet      bool      `json:"expiry_set,omitempty"`
	SpentUSD       float64   `json:"spent_usd"`
	BurnUSDPerHour float64   `json:"burn_usd_per_hour"`
	BurnUpdatedAt  time.Time `json:"burn_updated_at,omitzero"`
	ExhaustedAt    time.Time `json:"exhausted_at,omitzero"`
	ProbeAfter     time.Time `json:"probe_after,omitzero"`
}

type claudeRevokedState struct {
	CredentialFingerprint string    `json:"credential_fingerprint"`
	FirstSeen             time.Time `json:"first_seen"`
}

type claudeCostStateFile struct {
	Keys    map[string]*claudeCreditKeyState `json:"keys"`
	Revoked map[string]claudeRevokedState    `json:"revoked"`
}

func newClaudeCostStateFile() claudeCostStateFile {
	return claudeCostStateFile{Keys: map[string]*claudeCreditKeyState{}, Revoked: map[string]claudeRevokedState{}}
}

func (f claudeCostStateFile) clone() claudeCostStateFile {
	out := newClaudeCostStateFile()
	for id, state := range f.Keys {
		copied := *state
		out.Keys[id] = &copied
	}
	for id, state := range f.Revoked {
		out.Revoked[id] = state
	}
	return out
}

type claudeCostStore struct {
	path string
	now  func() time.Time

	mu        sync.Mutex
	cache     claudeCostStateFile
	loadedAt  time.Time
	pending   map[string]float64
	successes map[string]bool
	lastFlush time.Time
}

func newClaudeCostStore(path string) *claudeCostStore {
	return &claudeCostStore{path: path, now: time.Now, cache: newClaudeCostStateFile()}
}

func (s *claudeCostStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *claudeCostStore) readLocked() claudeCostStateFile {
	if s.path == "" {
		return s.cache.clone()
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return newClaudeCostStateFile()
	}
	file := newClaudeCostStateFile()
	if err := json.Unmarshal(data, &file); err != nil {
		return newClaudeCostStateFile()
	}
	if file.Keys == nil {
		file.Keys = map[string]*claudeCreditKeyState{}
	}
	if file.Revoked == nil {
		file.Revoked = map[string]claudeRevokedState{}
	}
	return file
}

func (s *claudeCostStore) writeLocked(file claudeCostStateFile) error {
	s.cache = file.clone()
	s.loadedAt = s.clock()
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// mutate runs one locked read-modify-write. Pending metered spend is folded
// in on every write.
func (s *claudeCostStore) mutate(change func(*claudeCostStateFile, time.Time)) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock := lockClaudeCostState(s.path)
	defer unlock()
	now := s.clock()
	file := s.readLocked()
	s.applyPendingLocked(&file, now)
	if change != nil {
		change(&file, now)
	}
	s.lastFlush = now
	return s.writeLocked(file)
}

func (s *claudeCostStore) applyPendingLocked(file *claudeCostStateFile, now time.Time) {
	for id, usd := range s.pending {
		state := file.Keys[id]
		if state == nil {
			continue
		}
		rollClaudeCreditPeriod(state, now)
		state.SpentUSD += usd
		state.BurnUSDPerHour = decayedClaudeCreditBurn(*state, now) + usd/claudeCreditBurnTau.Hours()
		state.BurnUpdatedAt = now
	}
	for id := range s.successes {
		if state := file.Keys[id]; state != nil {
			state.ExhaustedAt = time.Time{}
			state.ProbeAfter = time.Time{}
		}
	}
	s.pending = nil
	s.successes = nil
}

// snapshot returns the current state, refreshed from disk (and flushing local
// pending spend) at most every claudeCostStateFlushInterval.
func (s *claudeCostStore) snapshot() claudeCostStateFile {
	if s == nil {
		return newClaudeCostStateFile()
	}
	s.mu.Lock()
	now := s.clock()
	stale := s.loadedAt.IsZero() || now.Sub(s.loadedAt) >= claudeCostStateFlushInterval
	if !stale {
		out := s.cache.clone()
		s.mu.Unlock()
		return out
	}
	s.mu.Unlock()
	if len(s.pendingCopy()) > 0 || s.hasSuccesses() {
		_ = s.mutate(nil)
	} else {
		s.mu.Lock()
		unlock := lockClaudeCostState(s.path)
		s.cache = s.readLocked()
		unlock()
		s.loadedAt = now
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cache.clone()
}

func (s *claudeCostStore) pendingCopy() map[string]float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]float64, len(s.pending))
	for k, v := range s.pending {
		out[k] = v
	}
	return out
}

func (s *claudeCostStore) hasSuccesses() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.successes) > 0
}

// ensureKeys creates default state for keys the store has not seen: a
// $200 grant starting at the key's add date, expiring 30 days later.
func (s *claudeCostStore) ensureKeys(keys map[string]time.Time) {
	if s == nil || len(keys) == 0 {
		return
	}
	current := s.snapshot()
	missing := false
	for id := range keys {
		if current.Keys[id] == nil {
			missing = true
			break
		}
	}
	if !missing {
		return
	}
	_ = s.mutate(func(file *claudeCostStateFile, now time.Time) {
		for id, added := range keys {
			if file.Keys[id] != nil {
				continue
			}
			start := added
			if start.IsZero() || start.After(now) {
				start = now
			}
			file.Keys[id] = &claudeCreditKeyState{
				AmountUSD:   claudeCreditDefaultGrantUSD,
				PeriodStart: start.UTC(),
				ExpiresAt:   start.Add(claudeCreditDefaultPeriod).UTC(),
			}
		}
	})
}

// recordSpend meters one successful response on a key. A success also ends a
// spent-balance hold: the re-probe went through.
func (s *claudeCostStore) recordSpend(accountID string, usd float64) {
	if s == nil || accountID == "" {
		return
	}
	s.mu.Lock()
	if usd > 0 {
		if s.pending == nil {
			s.pending = map[string]float64{}
		}
		s.pending[accountID] += usd
	}
	if s.cache.Keys[accountID] != nil && !s.cache.Keys[accountID].ExhaustedAt.IsZero() {
		if s.successes == nil {
			s.successes = map[string]bool{}
		}
		s.successes[accountID] = true
	}
	due := s.clock().Sub(s.lastFlush) >= claudeCostStateFlushInterval || len(s.successes) > 0
	s.mu.Unlock()
	if due {
		_ = s.mutate(nil)
	}
}

// markExhausted holds a key whose balance Anthropic reports spent until its
// period rolls over, with one re-probe per claudeCreditReprobeInterval.
func (s *claudeCostStore) markExhausted(accountID string) time.Time {
	var probe time.Time
	_ = s.mutate(func(file *claudeCostStateFile, now time.Time) {
		state := file.Keys[accountID]
		if state == nil {
			state = &claudeCreditKeyState{AmountUSD: claudeCreditDefaultGrantUSD, PeriodStart: now, ExpiresAt: now.Add(claudeCreditDefaultPeriod)}
			file.Keys[accountID] = state
		}
		rollClaudeCreditPeriod(state, now)
		state.ExhaustedAt = now
		probe = now.Add(claudeCreditReprobeInterval)
		if state.ExpiresAt.Before(probe) {
			probe = state.ExpiresAt
		}
		state.ProbeAfter = probe
	})
	return probe
}

// set records an operator's grant, expiry, and optionally spend. It starts a
// fresh period ending at expiresAt and clears any spent-balance hold.
func (s *claudeCostStore) set(accountID string, amount float64, expiresAt time.Time, spent *float64) error {
	return s.mutate(func(file *claudeCostStateFile, now time.Time) {
		state := file.Keys[accountID]
		if state == nil {
			state = &claudeCreditKeyState{}
			file.Keys[accountID] = state
		}
		if amount > 0 {
			state.AmountUSD = amount
			state.AmountSet = true
		}
		if !expiresAt.IsZero() {
			state.ExpiresAt = expiresAt.UTC()
			state.PeriodStart = expiresAt.AddDate(0, -1, 0).UTC()
			state.ExpirySet = true
		}
		if spent != nil {
			state.SpentUSD = *spent
		}
		state.ExhaustedAt = time.Time{}
		state.ProbeAfter = time.Time{}
		rollClaudeCreditPeriod(state, now)
	})
}

func claudeCredentialFingerprint(identity string) string {
	sum := sha256.Sum256([]byte("claude-credential\x00" + identity))
	return hex.EncodeToString(sum[:12])
}

// markRevoked records a revoked credential the first time it is seen.
func (s *claudeCostStore) markRevoked(accountID, credentialIdentity string) time.Time {
	fingerprint := claudeCredentialFingerprint(credentialIdentity)
	var first time.Time
	_ = s.mutate(func(file *claudeCostStateFile, now time.Time) {
		existing, ok := file.Revoked[accountID]
		if ok && existing.CredentialFingerprint == fingerprint {
			first = existing.FirstSeen
			return
		}
		first = now.UTC()
		file.Revoked[accountID] = claudeRevokedState{CredentialFingerprint: fingerprint, FirstSeen: first}
	})
	return first
}

// revokedSince reports whether accountID's current credential is the one
// recorded as revoked.
func claudeRevokedSince(file claudeCostStateFile, accountID, credentialIdentity string) (time.Time, bool) {
	state, ok := file.Revoked[accountID]
	if !ok || credentialIdentity == "" || state.CredentialFingerprint != claudeCredentialFingerprint(credentialIdentity) {
		return time.Time{}, false
	}
	return state.FirstSeen, true
}

// rollClaudeCreditPeriod advances an expired period by whole months: unused
// credit is gone, spend and any spent-balance hold reset.
func rollClaudeCreditPeriod(state *claudeCreditKeyState, now time.Time) {
	if state == nil || state.ExpiresAt.IsZero() {
		return
	}
	for !now.Before(state.ExpiresAt) {
		state.PeriodStart = state.ExpiresAt
		state.ExpiresAt = state.ExpiresAt.AddDate(0, 1, 0)
		state.SpentUSD = 0
		state.ExhaustedAt = time.Time{}
		state.ProbeAfter = time.Time{}
	}
}

func decayedClaudeCreditBurn(state claudeCreditKeyState, now time.Time) float64 {
	if state.BurnUSDPerHour <= 0 || state.BurnUpdatedAt.IsZero() {
		return math.Max(0, state.BurnUSDPerHour)
	}
	elapsed := now.Sub(state.BurnUpdatedAt)
	if elapsed <= 0 {
		return state.BurnUSDPerHour
	}
	return state.BurnUSDPerHour * math.Exp(-elapsed.Hours()/claudeCreditBurnTau.Hours())
}

// claudeCreditView is the evaluated state of one key at one instant.
type claudeCreditView struct {
	AccountID    string
	AmountUSD    float64
	SpentUSD     float64
	RemainingUSD float64
	BurnPerHour  float64
	PeriodStart  time.Time
	ExpiresAt    time.Time
	Defaulted    bool
	// HeldUntil is set while a spent-balance hold excludes the key.
	HeldUntil time.Time
	Pace      string
	Promoted  bool
}

// evaluateClaudeCredit decides a key's pace. A key is promoted above plan
// quota when its grant would otherwise expire with credit left: always in
// the final 48 hours, and earlier when metered spend trails a linear
// schedule by more than claudeCreditBehindSlack of the grant AND the current
// burn rate would not drain the remainder by expiry.
func evaluateClaudeCredit(accountID string, stored claudeCreditKeyState, now time.Time) claudeCreditView {
	state := stored
	rollClaudeCreditPeriod(&state, now)
	view := claudeCreditView{
		AccountID:   accountID,
		AmountUSD:   state.AmountUSD,
		SpentUSD:    state.SpentUSD,
		BurnPerHour: decayedClaudeCreditBurn(state, now),
		PeriodStart: state.PeriodStart,
		ExpiresAt:   state.ExpiresAt,
		Defaulted:   !state.AmountSet || !state.ExpirySet,
	}
	view.RemainingUSD = math.Max(0, state.AmountUSD-state.SpentUSD)
	if !state.ExhaustedAt.IsZero() {
		if now.Before(state.ProbeAfter) {
			view.HeldUntil = state.ProbeAfter
			view.Pace = claudeCreditPaceSpent
			return view
		}
		view.Pace = claudeCreditPaceReprobe
		return view
	}
	if view.RemainingUSD <= 0 {
		// The meter is an estimate; a key metered out still routes in its
		// ordinary tier until Anthropic itself reports the balance spent.
		view.Pace = claudeCreditPaceMetered0
		return view
	}
	hoursLeft := state.ExpiresAt.Sub(now).Hours()
	if hoursLeft <= claudeCreditFinalWindow.Hours() {
		view.Pace, view.Promoted = claudeCreditPaceFinal, true
		return view
	}
	if view.BurnPerHour*hoursLeft >= view.RemainingUSD*claudeCreditOnPaceFactor {
		view.Pace = claudeCreditPaceOnPace
		return view
	}
	period := state.ExpiresAt.Sub(state.PeriodStart)
	expected := 0.0
	if period > 0 {
		elapsed := math.Max(0, now.Sub(state.PeriodStart).Hours())
		expected = state.AmountUSD * math.Min(1, elapsed/period.Hours())
	}
	if state.SpentUSD+claudeCreditBehindSlack*state.AmountUSD < expected {
		view.Pace, view.Promoted = claudeCreditPaceBehind, true
		return view
	}
	view.Pace = claudeCreditPaceAhead
	return view
}

func (s *claudeCostStore) views(now time.Time) map[string]claudeCreditView {
	file := s.snapshot()
	out := make(map[string]claudeCreditView, len(file.Keys))
	ids := make([]string, 0, len(file.Keys))
	for id := range file.Keys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		out[id] = evaluateClaudeCredit(id, *file.Keys[id], now)
	}
	return out
}

// claudeAPIListCostUSD prices one response's usage at Anthropic API list
// prices. Cache writes are priced at the 5-minute rate because the normalized
// usage does not keep the TTL split, so 1-hour writes are under-metered.
func claudeAPIListCostUSD(model string, usage tokenUsage) float64 {
	price := bedrockPriceFor(model)
	uncached := usage.InputTokens - usage.CachedInputTokens - usage.CacheWriteInputTokens
	if uncached < 0 {
		uncached = 0
	}
	return (float64(uncached)*price.input +
		float64(usage.CachedInputTokens)*price.cacheRead +
		float64(usage.CacheWriteInputTokens)*price.cacheWrite +
		float64(usage.OutputTokens)*price.output) / 1_000_000
}
