package accounts

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// HostIDEnv names the serving host for Codex OAuth host claims. It is opt-in:
// while it is unset nothing is stamped, and only files that already carry a
// claim are guarded.
const HostIDEnv = "SUBROUTER_HOST_ID"

// CodexHostClaim records which host rotates an OAuth account's refresh-token
// chain. Refresh tokens are single use, so when the same account file is copied
// to a second live server both hosts redeem the same token and the slower one
// burns the account with refresh_token_reused (#129). A claim naming another
// host makes this host refuse the refresh instead of racing the owner.
type CodexHostClaim struct {
	Host      string `json:"host"`
	ClaimedAt string `json:"claimed_at,omitempty"`
}

// LocalHostID returns the configured host identity, or "" when host claims
// are not enabled on this host.
func LocalHostID() string {
	return strings.TrimSpace(os.Getenv(HostIDEnv))
}

func (c *CodexHostClaim) claimed() bool {
	return c != nil && strings.TrimSpace(c.Host) != ""
}

// hostClaimable reports whether account is a Codex OAuth chain a host claim
// can guard.
func (a StoredCodexAccount) hostClaimable() bool {
	return a.ProviderOrDefault() == ProviderCodex && !a.IsAPIKey() && a.Auth.Tokens != nil
}

func codexRefreshToken(account StoredCodexAccount) string {
	if account.Auth.Tokens == nil {
		return ""
	}
	return account.Auth.Tokens.RefreshToken
}

// settleCodexHostClaim applies the claim rule on every save. A save that keeps
// the stored refresh token keeps the claim, and stamps this host only onto an
// unclaimed chain. A save that installs a different refresh token (a fresh
// login, an import, or a rotation, which a foreign claim never reaches) starts
// a chain this host holds, so the claim moves here, or is dropped when this
// host has no identity.
func settleCodexHostClaim(account *StoredCodexAccount, newChain bool) {
	if !account.hostClaimable() {
		return
	}
	if account.HostClaim.claimed() {
		if !newChain {
			return
		}
		if local := LocalHostID(); local != "" && strings.EqualFold(strings.TrimSpace(account.HostClaim.Host), local) {
			return
		}
	}
	account.HostClaim = localCodexHostClaim()
}

func localCodexHostClaim() *CodexHostClaim {
	host := LocalHostID()
	if host == "" {
		return nil
	}
	return &CodexHostClaim{Host: host, ClaimedAt: time.Now().UTC().Format(time.RFC3339)}
}

// CodexForeignHostClaimError reports an OAuth account whose refresh-token chain
// belongs to another host.
type CodexForeignHostClaimError struct {
	Account   string
	ClaimHost string
	LocalHost string
}

func (e *CodexForeignHostClaimError) Error() string {
	if e.LocalHost == "" {
		return fmt.Sprintf(
			"account %q is claimed by host %q and %s is unset here; refusing to refresh a chain another host may still rotate "+
				"(set %s=%s if this is that host, or re-add the account on this host)",
			e.Account, e.ClaimHost, HostIDEnv, HostIDEnv, e.ClaimHost,
		)
	}
	return fmt.Sprintf(
		"account %q is claimed by host %q, not this host %q; refusing to refresh a chain the other host may still rotate "+
			"(stop serving it there and re-add the account on this host)",
		e.Account, e.ClaimHost, e.LocalHost,
	)
}

// checkCodexHostClaim returns an error when another host owns account's chain.
// Unclaimed accounts are allowed; the next save stamps them when this host has
// an identity.
func checkCodexHostClaim(account StoredCodexAccount) error {
	if !account.HostClaim.claimed() {
		return nil
	}
	claimHost := strings.TrimSpace(account.HostClaim.Host)
	local := LocalHostID()
	if local != "" && strings.EqualFold(claimHost, local) {
		return nil
	}
	return &CodexForeignHostClaimError{Account: account.Email, ClaimHost: claimHost, LocalHost: local}
}

// ClaimUnclaimedOAuth stamps this host onto every Codex OAuth account that has
// no claim yet, so a file copied away after this point carries the claim. It
// does nothing while host claims are disabled.
func (s CodexStore) ClaimUnclaimedOAuth() (int, error) {
	if LocalHostID() == "" {
		return 0, nil
	}
	stored, err := s.ListStoredReadOnly()
	if err != nil {
		return 0, err
	}
	claimed := 0
	var errs []error
	for _, candidate := range stored {
		// Accounts inside a migration batch are left to that batch.
		if !candidate.hostClaimable() || candidate.HostClaim.claimed() || candidate.MigrationBatchID != "" {
			continue
		}
		ok, err := s.claimUnclaimedOAuth(candidate.Email)
		if err != nil {
			errs = append(errs, fmt.Errorf("claim %q: %w", candidate.Email, err))
			continue
		}
		if ok {
			claimed++
		}
	}
	return claimed, errors.Join(errs...)
}

func (s CodexStore) claimUnclaimedOAuth(identifier string) (bool, error) {
	lock, err := s.lockStoredAccount(identifier)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	account, found, err := s.findStoredExact(identifier)
	if err != nil || !found {
		return false, err
	}
	if !account.hostClaimable() || account.HostClaim.claimed() || account.MigrationBatchID != "" {
		return false, nil
	}
	return true, s.saveStoredUnlocked(account)
}
