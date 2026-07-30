package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The auto-switch sweep is a singleton operation even though every worker runs
// its own timer for it. Each sweep fetches usage for every OAuth account and
// then rewrites the shared active-account file, so N live workers means N times
// the upstream usage traffic and N times the churn on one file.
//
// This is not hypothetical. The supervisor's drain has no timeout, so upgraded
// -away workers survive as long as a client holds a connection. On the mac mini
// six generations were live at once and `sr auto-switch selected account` was
// firing about seven times per ten minutes instead of once, thrashing account
// selection roughly every 90 seconds.
//
// A filesystem lease makes the sweep cooperative: whichever worker claims it
// performs the sweep, and the rest skip until the interval has elapsed. The
// cadence then depends on the configured interval rather than on how many
// workers happen to be draining.
type srAutoSwitchLease struct {
	path string
	now  func() time.Time
}

func newSRAutoSwitchLease(stateDir string) srAutoSwitchLease {
	return srAutoSwitchLease{path: filepath.Join(stateDir, "sr-auto-switch.lease")}
}

func (l srAutoSwitchLease) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// acquire reports whether this worker should run the sweep now. It claims the
// lease by stamping the file, so a concurrent worker checking within the same
// interval declines.
func (l srAutoSwitchLease) acquire(interval time.Duration) (bool, error) {
	if l.path == "" || interval <= 0 {
		return true, nil
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		// Never let a lease problem stop the sweep entirely; degrading to the
		// old per-worker behavior is better than never switching accounts.
		return true, fmt.Errorf("prepare auto-switch lease dir: %w", err)
	}

	// Checking the stamp and claiming it must be one atomic step. Doing them
	// separately lets simultaneous workers all observe the same stale stamp and
	// every one of them sweep, which is the exact behavior this exists to stop.
	// O_EXCL create is the portable cross-process mutex for that.
	unlock, held := l.lock()
	if !held {
		// Another worker is deciding right now; it will do the sweep if one is
		// due, so this tick has nothing to do.
		return false, nil
	}
	defer unlock()

	info, err := os.Stat(l.path)
	switch {
	case err == nil:
		// Another worker swept recently, so stand down. A small tolerance keeps
		// timers that drift slightly from both firing.
		if elapsed := l.clock().Sub(info.ModTime()); elapsed < interval-interval/10 {
			return false, nil
		}
	case !errors.Is(err, os.ErrNotExist):
		return true, fmt.Errorf("stat auto-switch lease: %w", err)
	}

	// Claim before sweeping, not after, so a slow sweep does not let every other
	// worker in behind it.
	if err := l.stamp(); err != nil {
		return true, err
	}
	return true, nil
}

// lockStaleAfter bounds how long a crashed or killed worker can block the
// sweep. Workers are terminated routinely during upgrades, so an abandoned lock
// must not freeze account selection.
const lockStaleAfter = 2 * time.Minute

// lock takes a short cross-process mutex around the check-and-claim.
func (l srAutoSwitchLease) lock() (func(), bool) {
	path := l.path + ".lock"
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		_ = file.Close()
		return func() { _ = os.Remove(path) }, true
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, false
	}
	// Reclaim a lock abandoned by a worker that died holding it.
	if info, statErr := os.Stat(path); statErr == nil && l.clock().Sub(info.ModTime()) > lockStaleAfter {
		if os.Remove(path) == nil {
			if file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
				_ = file.Close()
				return func() { _ = os.Remove(path) }, true
			}
		}
	}
	return nil, false
}

func (l srAutoSwitchLease) stamp() error {
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("claim auto-switch lease: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close auto-switch lease: %w", err)
	}
	now := l.clock()
	if err := os.Chtimes(l.path, now, now); err != nil {
		return fmt.Errorf("stamp auto-switch lease: %w", err)
	}
	return nil
}
