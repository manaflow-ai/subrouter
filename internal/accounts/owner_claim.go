package accounts

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// OwnerClaim records which host is the live writer for an OAuth account file.
// Refresh tokens rotate; two hosts refreshing the same chain permanently burn
// the account (refresh_token_reused). The claim is the guard that refuses a
// foreign refresh unless an explicit takeover bumps the epoch.
type OwnerClaim struct {
	Host      string `json:"host"`
	Epoch     uint64 `json:"epoch"`
	ClaimedAt string `json:"claimed_at,omitempty"`
}

// ForeignOwnerClaimError is returned when this host must not refresh an account
// owned by another host.
type ForeignOwnerClaimError struct {
	Account string
	Claim   OwnerClaim
	Local   string
}

func (e *ForeignOwnerClaimError) Error() string {
	return fmt.Sprintf(
		"account %q is owned by host %q (epoch %d); this host is %q — refuse refresh to avoid refresh_token_reused (use --takeover to cut over)",
		e.Account, e.Claim.Host, e.Claim.Epoch, e.Local,
	)
}

var (
	localHostIDOnce sync.Once
	localHostID     string
)

// LocalHostID returns the stable host identity used in owner claims.
// Override with SUBROUTER_HOST_ID when hostname is unstable across reboots.
func LocalHostID() string {
	localHostIDOnce.Do(func() {
		if v := strings.TrimSpace(os.Getenv("SUBROUTER_HOST_ID")); v != "" {
			localHostID = v
			return
		}
		host, err := os.Hostname()
		if err != nil || strings.TrimSpace(host) == "" {
			localHostID = "unknown-host"
			return
		}
		localHostID = host
	})
	return localHostID
}

// ResetLocalHostIDForTest clears the cached host id (tests only).
func ResetLocalHostIDForTest() {
	localHostIDOnce = sync.Once{}
	localHostID = ""
}

// AssertLocalOwner returns a ForeignOwnerClaimError when claim names another host.
// A nil or empty-host claim is treated as unclaimed (legacy files) and allowed.
func AssertLocalOwner(accountEmail string, claim *OwnerClaim) error {
	if claim == nil || strings.TrimSpace(claim.Host) == "" {
		return nil
	}
	local := LocalHostID()
	if strings.EqualFold(strings.TrimSpace(claim.Host), local) {
		return nil
	}
	return &ForeignOwnerClaimError{
		Account: accountEmail,
		Claim:   *claim,
		Local:   local,
	}
}

// TakeoverOwnerClaim bumps the epoch and assigns this host as the live writer.
func TakeoverOwnerClaim(claim *OwnerClaim) OwnerClaim {
	return StampOwnerClaim(claim, true)
}

// StampOwnerClaim sets this host as owner. When takeover is true, the epoch is
// always bumped (even if already local) so a previous owner observing a higher
// epoch knows it lost the claim. When false, an existing local claim is kept
// and only unclaimed/legacy files are stamped at epoch 1. A foreign claim with
// takeover=false is left unchanged — callers must use takeover explicitly.
func StampOwnerClaim(claim *OwnerClaim, takeover bool) OwnerClaim {
	local := LocalHostID()
	now := time.Now().UTC().Format(time.RFC3339)
	if claim == nil || strings.TrimSpace(claim.Host) == "" {
		return OwnerClaim{Host: local, Epoch: 1, ClaimedAt: now}
	}
	sameHost := strings.EqualFold(strings.TrimSpace(claim.Host), local)
	if sameHost && !takeover {
		out := *claim
		if out.ClaimedAt == "" {
			out.ClaimedAt = now
		}
		return out
	}
	if !sameHost && !takeover {
		return *claim
	}
	epoch := claim.Epoch + 1
	if epoch == 0 {
		epoch = 1
	}
	return OwnerClaim{Host: local, Epoch: epoch, ClaimedAt: now}
}
